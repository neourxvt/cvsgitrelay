// Package process executes bounded, cancellable subprocesses without a shell.
package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"cvsgitrelay/internal/diag"
)

type Runner struct {
	Log       *slog.Logger
	Timeout   time.Duration
	MaxOutput int
	Root      string
}

type buffer struct {
	// Do not embed bytes.Buffer: its promoted ReadFrom would bypass Write's
	// output budget when os/exec drains a subprocess pipe through io.Copy.
	data  bytes.Buffer
	limit int
	total int
	tail  bool
}

func (b *buffer) Len() int       { return b.data.Len() }
func (b *buffer) Bytes() []byte  { return b.data.Bytes() }
func (b *buffer) String() string { return b.data.String() }

func (b *buffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += n
	if b.tail {
		if n >= b.limit {
			b.data.Reset()
			_, _ = b.data.Write(p[n-b.limit:])
		} else {
			if b.Len()+n > b.limit {
				b.data.Next(b.Len() + n - b.limit)
			}
			_, _ = b.data.Write(p)
		}
	} else if b.Len() < b.limit {
		_, _ = b.data.Write(p[:min(n, b.limit-b.Len())])
	}
	// Always drain both streams, even after exceeding the storage budget.
	return n, nil
}

func (r *Runner) Run(ctx context.Context, dir string, env []string, input []byte, name string, args ...string) ([]byte, error) {
	return r.run(ctx, dir, env, input, name, args, false)
}

// RunQuiet fails on diagnostics even after exit zero. Metadata enumeration
// cannot safely treat a warning about an omitted file as a complete scan.
func (r *Runner) RunQuiet(ctx context.Context, dir string, env []string, input []byte, name string, args ...string) ([]byte, error) {
	return r.run(ctx, dir, env, input, name, args, true)
}

func (r *Runner) run(ctx context.Context, dir string, env []string, input []byte, name string, args []string, quiet bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, environment(env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = bytes.NewReader(input)
	out, stderr := &buffer{limit: r.MaxOutput}, &buffer{limit: 64 << 10, tail: true}
	cmd.Stdout, cmd.Stderr = out, stderr
	// CVSROOT is deliberately absent from this safe command description.
	safe := append([]string(nil), args...)
	for i := range safe {
		safe[i] = diag.Redact(safe[i], r.Root)
	}
	start := time.Now()
	err := cmd.Run()
	fields := []any{"executable", name, "args", safe, "elapsed_ms", time.Since(start).Milliseconds(), "stdout_bytes", out.total, "stderr_bytes", stderr.total}
	if cmd.ProcessState != nil {
		fields = append(fields, "exit_code", cmd.ProcessState.ExitCode())
	}
	if err != nil || out.total > out.limit || (quiet && stderr.total > 0) {
		code, message := "subprocess", "command failed; inspect exit code and stderr"
		if quiet && stderr.total > 0 && err == nil {
			message = "metadata command produced stderr despite exit zero; completeness is unproven; resolve the warning or configure a quiet transport"
		}
		if ctx.Err() != nil {
			code, message, err = "timeout", "command interrupted or deadline exceeded", ctx.Err()
		}
		if out.total > out.limit {
			code, message = "limit", "command output exceeds --max-output; raise the limit after checking source size"
		}
		fields = append(fields, "stderr_tail", diag.Redact(stderr.String(), r.Root), "stderr_truncated", stderr.total > stderr.limit, "stdout_limit", out.limit)
		// Some Git diagnostics, particularly fsck object errors, are emitted
		// on stdout. Only admit known diagnostic commands, never CVS bytes or
		// cat-file/show output containing source content and log messages.
		if name == "git" {
			for _, arg := range args {
				if arg == "fsck" || arg == "update-ref" {
					data := out.Bytes()
					if len(data) > 8192 {
						data = data[len(data)-8192:]
					}
					fields = append(fields, "stdout_diagnostic", diag.Redact(string(data), r.Root))
					break
				}
			}
		}
		return nil, diag.New(code, "process.run", message, err, fields...)
	}
	if r.Log != nil {
		r.Log.Debug("command completed", fields...)
	}
	return out.Bytes(), nil
}

// Ignore inherited Git plumbing overrides and global configuration. Keep CVS
// authentication/transport facilities and the user's PATH available.
func environment(extra []string) []string {
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "GIT_") || key == "LC_ALL" || key == "TZ" {
			continue
		}
		env = append(env, value)
	}
	env = append(env, "LC_ALL=C", "TZ=UTC", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
	return append(env, extra...)
}

// DiscardLogger is useful to callers embedding the importer without diagnostics.
func DiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
