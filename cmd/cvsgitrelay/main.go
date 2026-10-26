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
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
	"cvsgitrelay/internal/relay"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) (exit int) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cvsgitrelay init|sync|status|verify [options]")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	repo := fs.String("repo", "", "dedicated bare Git destination (required)")
	root := fs.String("cvsroot", "", "CVS locator for init; use external authentication")
	module := fs.String("module", "", "literal repository directory module for init")
	scope := fs.String("scope", "all", "revision scope: trunk or all")
	mode := fs.String("mode", "metadata", "verification mode: metadata, deep or tip")
	format := fs.String("log-format", "text", "diagnostic format: text or json")
	level := fs.String("log-level", "info", "diagnostic level: debug, info, warn or error")
	stacks := fs.Bool("error-stacks", true, "include captured error stack frames")
	timeout := fs.Duration("command-timeout", 5*time.Minute, "timeout for each CVS/Git command")
	maxOutput := fs.Int("max-output", 256<<20, "maximum stdout bytes for each subprocess")
	progress := fs.Duration("progress-interval", 15*time.Second, "minimum interval between progress messages; zero disables them")
	failpoint := fs.String("failpoint", "", "developer interruption: [kill:]STAGE[:ORDINAL]")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	var verbosity slog.Level
	if err := verbosity.UnmarshalText([]byte(*level)); err != nil || (*format != "text" && *format != "json") {
		fmt.Fprintln(os.Stderr, "invalid diagnostic format or level")
		return 2
	}
	options := &slog.HandlerOptions{Level: verbosity}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, options)
	if *format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, options)
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	log := slog.New(handler).With("run_id", hex.EncodeToString(id), "command", args[0], "repo", *repo)
	defer func() {
		if recovered := recover(); recovered != nil {
			// Panic values can contain source data. Retain type and Go frames,
			// while omitting the untrusted value and all environment/arguments.
			log.Error("unexpected panic", "code", "internal", "panic_type", fmt.Sprintf("%T", recovered), "stack", string(debug.Stack()))
			exit = 70
		}
	}()
	if fs.NArg() != 0 || *repo == "" || *timeout <= 0 || *maxOutput < 1 || *progress < 0 || (*scope != "all" && *scope != "trunk") || (*mode != "metadata" && *mode != "deep" && *mode != "tip") {
		diag.Failure(log, diag.New("usage", "cli", "invalid arguments; see command --help", nil), *stacks)
		return 2
	}
	runner := &process.Runner{Log: log, Timeout: *timeout, MaxOutput: *maxOutput, Root: *root}
	log.Debug("runtime context", "go", runtime.Version(), "os", runtime.GOOS, "arch", runtime.GOARCH, "pid", os.Getpid(), "repo", *repo)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	start := time.Now()
	var err error
	var summary any
	if args[0] == "init" {
		if *root == "" || *module == "" {
			err = diag.New("usage", "init", "--cvsroot and --module are required", nil)
		} else {
			err = relay.Init(ctx, runner, *repo, *root, *module)
			summary = map[string]any{"operation": "init", "scope": "all", "initialized": err == nil}
		}
	} else if args[0] == "sync" || args[0] == "status" || args[0] == "verify" {
		var r *relay.Repository
		r, err = relay.Open(ctx, runner, *repo)
		if err == nil {
			defer r.Close()
			log = log.With("source_id", r.Config.SourceID)
			runner.Log = log
			switch args[0] {
			case "sync":
				summary, err = r.Sync(ctx, relay.Options{Scope: *scope, ProgressInterval: *progress, Failpoint: *failpoint})
			case "status":
				summary, err = r.Status(ctx, *scope)
			case "verify":
				summary, err = r.Verify(ctx, relay.Options{Scope: *scope, Mode: *mode, ProgressInterval: *progress})
			}
		}
	} else {
		err = diag.New("usage", "cli", "unknown command; use init, sync, status or verify", nil)
	}
	if err != nil {
		diag.Failure(log, err, *stacks)
		return exitCode(diag.Code(err))
	}
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		diag.Failure(log, diag.New("io", "cli.output", "write summary", err), *stacks)
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
