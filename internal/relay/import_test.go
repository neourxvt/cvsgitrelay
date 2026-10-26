package relay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"cvsgitrelay/internal/process"
)

func TestRlogCapturedDialect(t *testing.T) {
	data, err := os.ReadFile("../../testdata/gnu-cvs-1.11.23.rlog")
	if err != nil {
		t.Fatal(err)
	}
	in, err := parseRlog(data, "/fixture/cvsroot/module/")
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Files) != 4 || len(in.Revisions) != 9 || in.Trunk != 6 || in.Dead != 2 {
		t.Fatalf("unexpected inventory: %+v", in)
	}
	if !strings.Contains(string(in.Revisions[key("active", "1.2")].Log), "\n----------------------------\nseparator in message\n") {
		t.Fatal("separator-like log line was lost")
	}
	for _, broken := range [][]byte{data[:len(data)-5], []byte(strings.Replace(string(data), "selected revisions: 4", "selected revisions: 3", 1)), []byte(strings.Replace(string(data), "revision 1.2\n", "revision 1.3\n", 1)), append([]byte("unexpected stdout\n"), data...)} {
		if _, err := parseRlog(broken, "/fixture/cvsroot/module/"); err == nil {
			t.Fatal("malformed inventory accepted")
		}
	}
}

func TestTrunkImport(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	runner := &process.Runner{Log: process.DiscardLogger(), Timeout: time.Minute, MaxOutput: 16 << 20}
	dest := filepath.Join(t.TempDir(), "mirror.git")
	if err := Init(ctx, runner, dest, ":fork:"+root, "module"); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, runner, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Sync(ctx, Options{Scope: "all"}); err == nil {
		t.Fatal("full import must refuse nontrunk history before branch support")
	}
	s, err := r.Sync(ctx, Options{Scope: "trunk"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Published != 6 || s.Unimported != 3 {
		t.Fatalf("unexpected summary %+v", s)
	}
	checkpoint := r.StateOID
	s, err = r.Sync(ctx, Options{Scope: "trunk"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Published != 0 || checkpoint != r.StateOID {
		t.Fatal("second sync changed history")
	}
	entries, err := r.treeEntries(ctx, r.Manifest.Refs[trunkRef])
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries["active"].OID == "" || entries["binary file"].OID == "" {
		t.Fatalf("wrong trunk tree: %+v", entries)
	}
	for _, mode := range []string{"metadata", "deep", "tip"} {
		if _, err := r.Verify(ctx, Options{Scope: "trunk", Mode: mode}); err != nil {
			t.Fatalf("verify %s: %v", mode, err)
		}
	}
	// A second process must be excluded even when the writer has no pending work.
	if other, err := Open(ctx, runner, dest); err == nil {
		other.Close()
		t.Fatal("overlapping writer was allowed")
	}
	r.Close()
	r, err = Open(ctx, runner, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, err = r.Status(ctx, "trunk")
	if err != nil {
		t.Fatal(err)
	}
	if s.Imported != 6 || s.Pending != 0 {
		t.Fatalf("restart failed: %+v", s)
	}
	// An isolated external ref deletion must never be treated as a new mirror.
	if _, err := r.git(ctx, nil, nil, "update-ref", "-d", trunkRef); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if other, err := Open(ctx, runner, dest); err == nil {
		other.Close()
		t.Fatal("vanished managed ref was accepted")
	}
}

func TestCrashRecovery(t *testing.T) {
	root, _, _ := fixture(t)
	bin := filepath.Join(t.TempDir(), "cvsgitrelay")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/cvsgitrelay")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	ctx := context.Background()
	for _, stage := range []string{"scan-start", "scan-complete", "fetch-start", "fetch-complete", "blob-written", "tree-written", "commit-written", "before-publish", "after-publish"} {
		t.Run(stage, func(t *testing.T) {
			runner := &process.Runner{Log: process.DiscardLogger(), Timeout: time.Minute, MaxOutput: 16 << 20}
			dest := filepath.Join(t.TempDir(), "mirror.git")
			if err := Init(ctx, runner, dest, ":fork:"+root, "module"); err != nil {
				t.Fatal(err)
			}
			ordinal := "2"
			if strings.HasPrefix(stage, "scan-") {
				ordinal = "1"
			}
			cmd := exec.Command(bin, "sync", "--repo", dest, "--scope", "trunk", "--failpoint", "kill:"+stage+":"+ordinal)
			out, err := cmd.CombinedOutput()
			e, ok := err.(*exec.ExitError)
			if !ok || !e.ProcessState.Sys().(syscall.WaitStatus).Signaled() || e.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("expected SIGKILL at %s: %v\n%s", stage, err, out)
			}
			r, err := Open(ctx, runner, dest)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, err := r.Sync(ctx, Options{Scope: "trunk"}); err != nil {
				t.Fatal(err)
			}
			s, err := r.Verify(ctx, Options{Scope: "trunk", Mode: "deep"})
			if err != nil {
				t.Fatal(err)
			}
			if s.Imported != 6 || s.Pending != 0 {
				t.Fatalf("incomplete restart: %+v", s)
			}
		})
	}
}

func TestSourceMutation(t *testing.T) {
	root, work, _ := fixture(t)
	ctx := context.Background()
	runner := &process.Runner{Log: process.DiscardLogger(), Timeout: time.Minute, MaxOutput: 16 << 20}
	dest := filepath.Join(t.TempDir(), "mirror.git")
	if err := Init(ctx, runner, dest, ":fork:"+root, "module"); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, runner, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Sync(ctx, Options{Scope: "trunk"}); err != nil {
		t.Fatal(err)
	}
	fixtureCVS(t, root, work, "admin", "-m1.2:rewritten history", "active")
	if _, err := r.Status(ctx, "trunk"); err == nil {
		t.Fatal("source log rewrite was accepted")
	}
}
