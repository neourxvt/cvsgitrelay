package process

import (
	"context"
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
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := Runner{Log: DiscardLogger(), Timeout: tc.timeout, MaxOutput: 1024, Root: ":pserver:user:secret@host:/repo"}
			out, err := r.Run(context.Background(), "", []string{"CVSGITRELAY_PROCESS_HELPER=1"}, nil, bin, "-test.run=TestProcessHelper", "--", tc.mode)
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
	}
}
