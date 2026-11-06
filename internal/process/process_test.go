package process

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cvsgitrelay/internal/diag"
)

func TestProcessFailures(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode, code string
		timeout    time.Duration
	}{
		{"partial", "subprocess", time.Second},
		{"limit", "limit", time.Second},
		{"hang", "timeout", 20 * time.Millisecond},
		{"warning", "subprocess", time.Second},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := Runner{Log: DiscardLogger(), Timeout: tc.timeout, MaxOutput: 1024, Root: ":pserver:user:secret@host:/repo"}
			out, err := r.RunQuiet(context.Background(), "", []string{"CVSGITRELAY_PROCESS_HELPER=1"}, nil, bin, "-test.run=TestProcessHelper", "--", tc.mode)
			if out != nil || diag.Code(err) != tc.code {
				t.Fatalf("got output=%d code=%s error=%v", len(out), diag.Code(err), err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestProcessHelper(t *testing.T) {
	if os.Getenv("CVSGITRELAY_PROCESS_HELPER") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "partial":
		_, _ = os.Stdout.WriteString("partial content")
		_, _ = os.Stderr.WriteString(":pserver:user:secret@host:/repo failed\n")
		os.Exit(1)
	case "limit":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 2048))
		os.Exit(0)
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "warning":
		_, _ = os.Stdout.WriteString("partial metadata")
		_, _ = os.Stderr.WriteString("omitting inaccessible file\n")
		os.Exit(0)
	}
}

// Secrets passed to a command are removed from its failure report, both from
// the arguments and from what the command printed.
func TestRunnerRedactsSecrets(t *testing.T) {
	r := Runner{Log: DiscardLogger(), Timeout: time.Second, MaxOutput: 1024, Secrets: []string{"tok-123", ""}}
	_, err := r.Run(context.Background(), "", nil, nil, "sh", "-c", "echo bad tok-123 >&2; exit 1", "tok-123")
	if err == nil {
		t.Fatal("the command succeeded")
	}
	report := err.Error() + strings.Join(func() (all []string) {
		for _, d := range diag.Details(err, false) {
			all = append(all, strings.TrimSpace(fmt.Sprint(d)))
		}
		return
	}(), "\n")
	if strings.Contains(report, "tok-123") || !strings.Contains(report, "[redacted]") {
		t.Fatalf("secret not redacted: %s", report)
	}
}
