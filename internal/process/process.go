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
	bytes.Buffer
	limit int
	total int
	tail  bool
}

func (b *buffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += n
	if b.tail {
		if n >= b.limit {
			b.Reset()
			_, _ = b.Buffer.Write(p[n-b.limit:])
		} else {
			if b.Len()+n > b.limit {
				b.Next(b.Len() + n - b.limit)
			}
			_, _ = b.Buffer.Write(p)
		}
	} else if b.Len() < b.limit {
		_, _ = b.Buffer.Write(p[:min(n, b.limit-b.Len())])
	}
	// Always drain both streams, even after exceeding the storage budget.
	return n, nil
}

func (r *Runner) Run(ctx context.Context, dir string, env []string, input []byte, name string, args ...string) ([]byte, error) {
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
	if err != nil || out.total > out.limit {
		code, message := "subprocess", "command failed; inspect exit code and stderr"
		if ctx.Err() != nil {
			code, message, err = "timeout", "command interrupted or deadline exceeded", ctx.Err()
		}
		if out.total > out.limit {
			code, message = "limit", "command output exceeds --max-output; raise the limit after checking source size"
		}
		fields = append(fields, "stderr_tail", diag.Redact(stderr.String(), r.Root), "stderr_truncated", stderr.total > stderr.limit, "stdout_limit", out.limit)
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
	env = append(env, "LC_ALL=C", "TZ=UTC", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
	return append(env, extra...)
}

// DiscardLogger is useful to callers embedding the importer without diagnostics.
func DiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
