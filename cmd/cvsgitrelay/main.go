package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
	"cvsgitrelay/internal/relay"
	"cvsgitrelay/internal/settings"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) (exit int) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cvsgitrelay init|sync|status|verify FILE.toml [overrides]")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: cvsgitrelay %s FILE.toml [overrides]\n", args[0])
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "override destination.repo (relative to the caller)")
	root := fs.String("cvsroot", "", "override source.cvsroot; use external authentication")
	module := fs.String("module", "", "override source.module")
	submodules := fs.String("submodules", "exclude", "override source.submodules: exclude or include")
	scope := fs.String("scope", "all", "override sync.scope: trunk or all")
	mode := fs.String("mode", "metadata", "override verify.mode: metadata, deep or tip")
	format := fs.String("log-format", "text", "override logging.format: text or json")
	level := fs.String("log-level", "info", "override logging.level: debug, info, warn or error")
	stacks := fs.Bool("error-stacks", true, "override logging.error_stacks")
	timeout := fs.Duration("command-timeout", 5*time.Minute, "override process.command_timeout")
	maxOutput := fs.Int("max-output", 256<<20, "override process.max_output")
	progress := fs.Duration("progress-interval", 15*time.Second, "override logging.progress_interval; zero disables progress")
	failpoint := fs.String("failpoint", "", "override debug.failpoint: [kill:]STAGE[:ORDINAL]")
	// Consume the required path before parsing overrides, since flag parsing
	// stops at the first positional argument. Help still works without a file.
	configPath := ""
	flagArgs := args[1:]
	if len(flagArgs) > 0 && !strings.HasPrefix(flagArgs[0], "-") {
		configPath, flagArgs = flagArgs[0], flagArgs[1:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	// Configuration failures need readable diagnostics before the chosen handler
	// is available. Raw TOML parser errors can contain private connection details.
	log := slog.New(diag.NewTextHandler(os.Stderr, nil)).With("run_id", hex.EncodeToString(id), "command", args[0], "config", configPath)
	defer func() {
		if recovered := recover(); recovered != nil {
			// Panic values can contain source data. Retain type and Go frames,
			// while omitting the untrusted value and all environment/arguments.
			log.Error("unexpected panic", "code", "internal", "panic_type", fmt.Sprintf("%T", recovered), "stack", string(debug.Stack()))
			exit = 70
		}
	}()
	if fs.NArg() != 0 || (args[0] != "init" && args[0] != "sync" && args[0] != "status" && args[0] != "verify") {
		diag.Failure(log, diag.New("usage", "cli", "invalid arguments; see command --help", nil), *stacks)
		return 2
	}
	cfg, err := settings.Load(configPath)
	if err != nil {
		diag.Failure(log, err, *stacks)
		return exitCode(diag.Code(err))
	}
	// Visit only explicitly supplied flags: flag defaults must never overwrite
	// TOML values, particularly booleans and zero progress intervals.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "repo":
			cfg.Destination.Repo = *repo
		case "cvsroot":
			cfg.Source.CVSRoot = *root
		case "module":
			cfg.Source.Module = *module
		case "submodules":
			cfg.Source.Submodules = *submodules
		case "scope":
			cfg.Sync.Scope = *scope
		case "mode":
			cfg.Verify.Mode = *mode
		case "log-format":
			cfg.Logging.Format = *format
		case "log-level":
			cfg.Logging.Level = *level
		case "error-stacks":
			cfg.Logging.ErrorStacks = *stacks
		case "command-timeout":
			cfg.Process.CommandTimeout = timeout.String()
		case "max-output":
			cfg.Process.MaxOutput = *maxOutput
		case "progress-interval":
			cfg.Logging.ProgressInterval = progress.String()
		case "failpoint":
			cfg.Debug.Failpoint = *failpoint
		}
	})
	if err := cfg.Validate(); err != nil {
		diag.Failure(log, err, cfg.Logging.ErrorStacks)
		return exitCode(diag.Code(err))
	}
	var verbosity slog.Level
	_ = verbosity.UnmarshalText([]byte(cfg.Logging.Level))
	options := &slog.HandlerOptions{Level: verbosity}
	var handler slog.Handler = diag.NewTextHandler(os.Stderr, options)
	if cfg.Logging.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, options)
	}
	log = slog.New(handler).With("run_id", hex.EncodeToString(id), "command", args[0], "repo", cfg.Destination.Repo, "config", configPath)
	commandTimeout, _ := time.ParseDuration(cfg.Process.CommandTimeout)
	progressInterval, _ := time.ParseDuration(cfg.Logging.ProgressInterval)
	runner := &process.Runner{Log: log, Timeout: commandTimeout, MaxOutput: cfg.Process.MaxOutput, Root: cfg.Source.CVSRoot}
	log.Debug("runtime context", "go", runtime.Version(), "os", runtime.GOOS, "arch", runtime.GOARCH, "pid", os.Getpid())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	start := time.Now()
	var summary any
	if args[0] == "init" {
		err = relay.Init(ctx, runner, cfg.Destination.Repo, cfg.Source.CVSRoot, cfg.Source.Module)
		summary = map[string]any{"operation": "init", "scope": "all", "submodules": cfg.Source.Submodules, "initialized": err == nil}
	} else {
		// Preserve the sync wrapper's first-run behavior without duplicating
		// destination settings in Bash. Init still refuses unmanaged data refs.
		if args[0] == "sync" {
			if _, statErr := os.Stat(filepath.Join(cfg.Destination.Repo, "cvsgitrelay.json")); os.IsNotExist(statErr) {
				err = relay.Init(ctx, runner, cfg.Destination.Repo, cfg.Source.CVSRoot, cfg.Source.Module)
			} else if statErr != nil {
				err = diag.New("io", "sync.open", "cannot inspect destination configuration", statErr)
			}
		}
		if err != nil {
			diag.Failure(log, err, cfg.Logging.ErrorStacks)
			return exitCode(diag.Code(err))
		}
		var r *relay.Repository
		r, err = relay.Open(ctx, runner, cfg.Destination.Repo)
		if err == nil {
			defer r.Close()
			log = log.With("source_id", r.Config.SourceID)
			runner.Log = log
			if err = r.ConfigureSource(cfg.Source.CVSRoot, cfg.Source.Module, cfg.Source.Submodules); err == nil {
				switch args[0] {
				case "sync":
					summary, err = r.Sync(ctx, relay.Options{Scope: cfg.Sync.Scope, ProgressInterval: progressInterval, Failpoint: cfg.Debug.Failpoint})
				case "status":
					summary, err = r.Status(ctx, cfg.Sync.Scope)
				case "verify":
					summary, err = r.Verify(ctx, relay.Options{Scope: cfg.Sync.Scope, Mode: cfg.Verify.Mode, ProgressInterval: progressInterval})
				}
			}
		}
	}
	if err != nil {
		diag.Failure(log, err, cfg.Logging.ErrorStacks)
		return exitCode(diag.Code(err))
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		diag.Failure(log, diag.New("io", "cli.output", "write summary", err), cfg.Logging.ErrorStacks)
		return 74
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
