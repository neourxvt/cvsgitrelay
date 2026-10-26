package relay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cvsgitrelay/internal/process"
)

func TestLateBranchFiles(t *testing.T) {
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
	defer func() { r.Close() }()
	if _, err := r.Sync(ctx, Options{Scope: "all"}); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(work, "late"), []byte("late branch member\n"))
	fixtureCVS(t, root, work, "add", "late")
	fixtureCVS(t, root, work, "commit", "-m", "late addition", "late")
	// Interrupt between the dead trunk placeholder and the live branch add.
	if _, err := r.Sync(ctx, Options{Scope: "all", Failpoint: "after-publish:1"}); err == nil {
		t.Fatal("expected deliberate interruption")
	}
	r.Close()
	r, err = Open(ctx, runner, dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sync(ctx, Options{Scope: "all"}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"deep", "tip"} {
		if _, err := r.Verify(ctx, Options{Scope: "all", Mode: mode}); err != nil {
			t.Fatalf("late addition verify %s: %v", mode, err)
		}
	}
	// Every revision between polls must be imported, including an explicit
	// identical-content check-in and branch deletion/re-add transitions.
	for _, content := range []string{"second branch version\n", "third branch version\n"} {
		writeFixture(t, filepath.Join(work, "active"), []byte(content))
		fixtureCVS(t, root, work, "commit", "-m", "between polls", "active")
	}
	fixtureCVS(t, root, work, "commit", "-f", "-m", "identical bytes", "active")
	fixtureCVS(t, root, work, "remove", "-f", "branch-only")
	fixtureCVS(t, root, work, "commit", "-m", "branch deletion", "branch-only")
	writeFixture(t, filepath.Join(work, "branch-only"), []byte("branch restored\n"))
	fixtureCVS(t, root, work, "add", "branch-only")
	fixtureCVS(t, root, work, "commit", "-m", "branch restored", "branch-only")
	s, err := r.Sync(ctx, Options{Scope: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Published != 5 {
		t.Fatalf("missed between-poll revisions: %+v", s)
	}
	if _, err := r.Verify(ctx, Options{Scope: "all", Mode: "tip"}); err != nil {
		t.Fatal(err)
	}
	// A tag added later to a live trunk file requires a synthetic membership
	// transition. Stop before publication rather than omit it from the branch.
	fixtureCVS(t, root, work, "update", "-A")
	writeFixture(t, filepath.Join(work, "retro"), []byte("live forkpoint\n"))
	fixtureCVS(t, root, work, "add", "retro")
	fixtureCVS(t, root, work, "commit", "-m", "new trunk file", "retro")
	if _, err := r.Sync(ctx, Options{Scope: "all"}); err != nil {
		t.Fatal(err)
	}
	checkpoint := r.StateOID
	fixtureCVS(t, root, work, "tag", "-b", "feature", "retro")
	if _, err := r.Sync(ctx, Options{Scope: "all"}); err == nil {
		t.Fatal("late live membership was silently accepted")
	}
	if r.StateOID != checkpoint {
		t.Fatal("failed membership check changed published refs")
	}
}

func TestVendorDefaultPolicy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureCVS(t, root, "", "init")
	work := t.TempDir()
	writeFixture(t, filepath.Join(work, "vendor-file"), []byte("first vendor version\n"))
	fixtureCVS(t, root, work, "import", "-m", "first vendor", "module", "VENDOR", "FIRST")
	writeFixture(t, filepath.Join(work, "vendor-file"), []byte("second vendor version\n"))
	fixtureCVS(t, root, work, "import", "-m", "second vendor", "module", "VENDOR", "SECOND")
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
	if _, err := r.Sync(ctx, Options{Scope: "all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(ctx, Options{Scope: "all", Mode: "tip"}); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	fixtureCVS(t, root, filepath.Dir(checkout), "checkout", "-d", "checkout", "module")
	writeFixture(t, filepath.Join(checkout, "vendor-file"), []byte("first local trunk change\n"))
	fixtureCVS(t, root, checkout, "commit", "-m", "advance trunk", "vendor-file")
	if _, err := r.Sync(ctx, Options{Scope: "all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(ctx, Options{Scope: "all", Mode: "tip"}); err != nil {
		t.Fatal(err)
	}
}
