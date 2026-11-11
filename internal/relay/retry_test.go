package relay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

// A CVS server that stays unreachable through every retry is a reason to run
// again later, not a defect of the source; other CVS failures keep their code.
func TestUnreachableCVSIsRetryable(t *testing.T) {
	delays := cvsRetryDelays
	cvsRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { cvsRetryDelays = delays })
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	// Stand in for the CVS client: count the attempts and fail like pserver
	// does when the server refuses the connection, or like a missing module.
	script := "#!/bin/sh\necho x >> " + calls + "\n" +
		"if [ -n \"$CVS_FAKE_REFUSE\" ]; then echo 'cvs [rlog aborted]: connect to cvs.example.org(1.2.3.4):2401 failed: Connection refused' >&2; exit 1; fi\n" +
		"echo 'cvs [rlog aborted]: there is no repository /cvs/module' >&2; exit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "cvs"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := &Repository{Config: Config{CVSRoot: ":pserver:anonymous@cvs.example.org:/cvs", Module: "module"}, Runner: testRunner()}

	t.Setenv("CVS_FAKE_REFUSE", "1")
	if _, err := r.cvs(context.Background(), "", "rlog", "module"); diag.Code(err) != "retry" {
		t.Fatalf("unreachable server reported as %q: %v", diag.Code(err), err)
	}
	if data, _ := os.ReadFile(calls); len(data) != 2*(len(cvsRetryDelays)+1) {
		t.Fatalf("%d attempts, want %d", len(data)/2, len(cvsRetryDelays)+1)
	}

	os.Remove(calls)
	t.Setenv("CVS_FAKE_REFUSE", "")
	if _, err := r.cvs(context.Background(), "", "rlog", "module"); diag.Code(err) != "subprocess" {
		t.Fatalf("missing module reported as %q: %v", diag.Code(err), err)
	}
	if data, _ := os.ReadFile(calls); len(data) != 2 {
		t.Fatalf("a permanent failure was retried: %d attempts", len(data)/2)
	}
}

// Two complete scans that disagree mean the source moved during the read; the
// next run reads it again.
func TestChangingSourceIsRetryable(t *testing.T) {
	t.Parallel()
	r := &Repository{Config: Config{CVSRoot: ":pserver:anonymous@cvs.example.org:/cvs", SourceID: "source"}, Runner: testRunner()}
	scans := 0
	r.sourceReader = func(context.Context) (canonical.Inventory, sourceStats, error) {
		scans++
		if scans == 1 {
			return canonical.Inventory{}, sourceStats{}, nil
		}
		return canonical.Inventory{Symbols: []canonical.Symbol{{Archive: "module/a", Name: "NEW", Target: "1.1"}}}, sourceStats{}, nil
	}
	if _, _, err := r.acceptSource(context.Background(), &Options{}); diag.Code(err) != "retry" {
		t.Fatalf("changing source reported as %q: %v", diag.Code(err), err)
	}
}
