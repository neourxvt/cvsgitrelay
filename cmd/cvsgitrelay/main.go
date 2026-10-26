package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
	"cvsgitrelay/internal/relay"
	"cvsgitrelay/internal/settings"
)

type cliOptions struct {
	output                      string
	repo                        string
	cvsroot                     string
	module                      string
	submodules                  string
	requireConsecutiveRevisions bool
	scope                       string
	connections                 int
	mode                        string
	logFormat                   string
	logLevel                    string
	errorStacks                 bool
	commandTimeout              time.Duration
	maxOutput                   int
	progressInterval            time.Duration
	failpoint                   string
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	exit := 0
	root := newRootCommand(func(code int) { exit = code })
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return exit
}

func newRootCommand(setExit func(int)) *cobra.Command {
	options := &cliOptions{}
	root := &cobra.Command{
		Use:           "cvsgitrelay FILE.toml",
		Short:         "Mirror CVS revision history into Git",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, args []string) {
			setExit(runOperation("sync", args[0], cmd, options))
		},
	}
	addOperationFlags(root, "sync", options)
	root.AddCommand(
		newOperationCommand("status", "Show discovered, imported, and pending CVS revisions.", setExit),
		newOperationCommand("verify", "Check the mirror against CVS and Git integrity rules.", setExit),
		newOperationCommand("mirror", "Create or incrementally update a deterministic local CVSROOT.", setExit),
	)
	return root
}

func newOperationCommand(operation, short string, setExit func(int)) *cobra.Command {
	options := &cliOptions{}
	command := &cobra.Command{
		Use:           operation + " FILE.toml",
		Short:         short,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, args []string) {
			setExit(runOperation(operation, args[0], cmd, options))
		},
	}

	addOperationFlags(command, operation, options)
	if operation == "mirror" {
		command.Flags().StringVar(&options.output, "output", "", "required local CVSROOT directory to create or update (relative to the caller)")
		_ = command.MarkFlagRequired("output")
	}
	return command
}

func addOperationFlags(command *cobra.Command, operation string, options *cliOptions) {
	flags := command.Flags()
	if operation != "mirror" {
		flags.StringVar(&options.repo, "repo", "", "override destination.repo (relative to the caller)")
		flags.StringVar(&options.submodules, "submodules", "exclude", "override source.submodules: exclude or include")
		flags.StringVar(&options.scope, "scope", "all", "override sync.scope: trunk or all")
	}
	flags.StringVar(&options.cvsroot, "cvsroot", "", "override source.cvsroot; use external authentication")
	flags.StringVar(&options.module, "module", "", "override source.module")
	flags.BoolVar(&options.requireConsecutiveRevisions, "require-consecutive-revisions", false, "override source.require_consecutive_revisions: reject numbering gaps")
	flags.StringVar(&options.logFormat, "log-format", "text", "override logging.format: text or json")
	flags.StringVar(&options.logLevel, "log-level", "info", "override logging.level: debug, info, warn or error")
	flags.BoolVar(&options.errorStacks, "error-stacks", true, "override logging.error_stacks")
	flags.DurationVar(&options.commandTimeout, "command-timeout", 5*time.Minute, "override process.command_timeout")
	flags.IntVar(&options.maxOutput, "max-output", 256<<20, "override process.max_output")
	flags.DurationVar(&options.progressInterval, "progress-interval", 15*time.Second, "override logging.progress_interval; zero disables progress")
	if operation == "sync" || operation == "mirror" {
		flags.IntVar(&options.connections, "connections", 2, "override sync.connections: maximum concurrent CVS revision fetches")
	}
	if operation == "sync" {
		flags.StringVar(&options.failpoint, "failpoint", "", "override debug.failpoint: [kill:]STAGE[:ORDINAL]")
	}
	if operation == "verify" {
		flags.StringVar(&options.mode, "mode", "metadata", "override verify.mode: metadata, deep or tip")
	}
}

func runOperation(operation, configPath string, command *cobra.Command, options *cliOptions) (exit int) {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	// Configuration failures need readable diagnostics before the chosen handler
	// is available. Raw TOML parser errors can contain private connection details.
	log := slog.New(diag.NewTextHandler(os.Stderr, nil)).With("run_id", hex.EncodeToString(id), "command", operation, "config", configPath)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	reportFailure := func(err error, stacks bool) int {
		if ctx.Err() != nil {
			log.Warn("Interrupted; shutting down")
			return 130
		}
		diag.Failure(log, err, stacks)
		return exitCode(diag.Code(err))
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			// Panic values can contain source data. Retain type and Go frames,
			// while omitting the untrusted value and all environment/arguments.
			log.Error("unexpected panic", "code", "internal", "panic_type", fmt.Sprintf("%T", recovered), "stack", string(debug.Stack()))
			exit = 70
		}
	}()
	cfg, err := settings.Load(configPath)
	if err != nil {
		return reportFailure(err, options.errorStacks)
	}
	if ctx.Err() != nil {
		return reportFailure(ctx.Err(), options.errorStacks)
	}
	// Visit only explicit flags so defaults never overwrite values from TOML.
	command.Flags().Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "repo":
			cfg.Destination.Repo = options.repo
		case "cvsroot":
			cfg.Source.CVSRoot = options.cvsroot
		case "module":
			cfg.Source.Module = options.module
		case "submodules":
			cfg.Source.Submodules = options.submodules
		case "require-consecutive-revisions":
			cfg.Source.RequireConsecutiveRevisions = options.requireConsecutiveRevisions
		case "scope":
			cfg.Sync.Scope = options.scope
		case "connections":
			cfg.Sync.Connections = options.connections
		case "mode":
			cfg.Verify.Mode = options.mode
		case "log-format":
			cfg.Logging.Format = options.logFormat
		case "log-level":
			cfg.Logging.Level = options.logLevel
		case "error-stacks":
			cfg.Logging.ErrorStacks = options.errorStacks
		case "command-timeout":
			cfg.Process.CommandTimeout = options.commandTimeout.String()
		case "max-output":
			cfg.Process.MaxOutput = options.maxOutput
		case "progress-interval":
			cfg.Logging.ProgressInterval = options.progressInterval.String()
		case "failpoint":
			cfg.Debug.Failpoint = options.failpoint
		}
	})
	if err := cfg.Validate(); err != nil {
		return reportFailure(err, cfg.Logging.ErrorStacks)
	}
	var verbosity slog.Level
	_ = verbosity.UnmarshalText([]byte(cfg.Logging.Level))
	optionsLog := &slog.HandlerOptions{Level: verbosity}
	var handler slog.Handler = diag.NewTextHandler(os.Stderr, optionsLog)
	if cfg.Logging.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, optionsLog)
	}
	log = slog.New(handler).With("run_id", hex.EncodeToString(id), "command", operation, "config", configPath)
	if operation == "mirror" {
		log = log.With("output", options.output)
	} else {
		log = log.With("repo", cfg.Destination.Repo)
	}
	log.Info("Starting " + operation + " with " + configPath)
	commandTimeout, _ := time.ParseDuration(cfg.Process.CommandTimeout)
	progressInterval, _ := time.ParseDuration(cfg.Logging.ProgressInterval)
	runner := &process.Runner{Log: log, Timeout: commandTimeout, MaxOutput: cfg.Process.MaxOutput, Root: cfg.Source.CVSRoot}
	start := time.Now()
	var summary any
	if operation == "sync" {
		// First sync provisions a new destination before opening it for import.
		if _, statErr := os.Stat(filepath.Join(cfg.Destination.Repo, "cvsgitrelay.json")); os.IsNotExist(statErr) {
			err = relay.Init(ctx, runner, cfg.Destination.Repo, cfg.Source.CVSRoot, cfg.Source.Module)
		} else if statErr != nil {
			err = diag.New("io", "sync.open", "cannot inspect destination configuration", statErr)
		}
	}
	if err != nil {
		return reportFailure(err, cfg.Logging.ErrorStacks)
	}
	if ctx.Err() != nil {
		return reportFailure(ctx.Err(), cfg.Logging.ErrorStacks)
	}
	if operation == "mirror" {
		// A CVS-to-CVS mirror does not open the configured Git destination.
		summary, err = relay.Mirror(ctx, runner, cfg.Source.CVSRoot, cfg.Source.Module, options.output, cfg.Source.RequireConsecutiveRevisions, relay.Options{Connections: cfg.Sync.Connections, ProgressInterval: progressInterval})
	} else {
		var repository *relay.Repository
		repository, err = relay.Open(ctx, runner, cfg.Destination.Repo)
		if err == nil {
			defer repository.Close()
			log = log.With("source_id", repository.Config.SourceID)
			runner.Log = log
			if err = repository.ConfigureSource(cfg.Source.CVSRoot, cfg.Source.Module, cfg.Source.Submodules); err == nil {
				repository.RequireConsecutiveRevisions = cfg.Source.RequireConsecutiveRevisions
				switch operation {
				case "sync":
					summary, err = repository.Sync(ctx, relay.Options{Scope: cfg.Sync.Scope, Connections: cfg.Sync.Connections, ProgressInterval: progressInterval, Failpoint: cfg.Debug.Failpoint})
				case "status":
					summary, err = repository.Status(ctx, cfg.Sync.Scope)
				case "verify":
					summary, err = repository.Verify(ctx, relay.Options{Scope: cfg.Sync.Scope, Mode: cfg.Verify.Mode, ProgressInterval: progressInterval})
				}
			}
		}
	}
	if err != nil {
		return reportFailure(err, cfg.Logging.ErrorStacks)
	}
	if ctx.Err() != nil {
		return reportFailure(ctx.Err(), cfg.Logging.ErrorStacks)
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		return reportFailure(diag.New("io", "cli.output", "write summary", err), cfg.Logging.ErrorStacks)
	}
	log.Debug("operation completed", "elapsed_ms", time.Since(start).Milliseconds())
	return 0
}

func exitCode(code string) int {
	switch code {
	case "usage":
		return 2
	case "config":
		return 3
	case "unsupported":
		return 4
	case "parse", "source", "integrity":
		return 5
	case "conflict", "locked":
		return 6
	case "subprocess", "timeout", "limit":
		return 7
	case "io":
		return 74
	case "injected":
		return 8
	default:
		return 70
	}
}
