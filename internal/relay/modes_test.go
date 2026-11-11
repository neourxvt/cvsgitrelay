package relay

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
)

// observeModes works without a mirror: it reports the archive mode of files in
// a root checkout and, through one historical checkout, of removed files, and
// leaves out archives that never had a live revision to expose a mode.
func TestObserveModesReportsArchiveModes(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("cvs"); err != nil {
		t.Skip("CVS is required")
	}
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureCVS(t, root, "", "init")
	work := t.TempDir()
	for _, name := range []string{"plain", "script", "removed"} {
		writeFixture(t, filepath.Join(work, name), []byte(name+"\n"))
	}
	fixtureCVS(t, root, work, "import", "-m", "initial", "mod", "VENDOR", "FIRST")
	checkout := t.TempDir()
	fixtureCVS(t, root, checkout, "checkout", "mod")
	if err := os.Remove(filepath.Join(checkout, "mod", "removed")); err != nil {
		t.Fatal(err)
	}
	fixtureCVS(t, root, filepath.Join(checkout, "mod"), "remove", "removed")
	fixtureCVS(t, root, filepath.Join(checkout, "mod"), "commit", "-m", "remove", "removed")
	// The executable bit lives on the RCS archive, including in the Attic.
	for _, archive := range []string{"script,v", "Attic/removed,v"} {
		if err := os.Chmod(filepath.Join(root, "mod", archive), 0555); err != nil {
			t.Fatal(err)
		}
	}
	in, err := buildMirrorInventory([]*File{
		{SourcePath: "mod/plain", Head: "1.1", Revisions: []*Revision{{Number: "1.1", State: "Exp"}}},
		{SourcePath: "mod/script", Head: "1.1", Revisions: []*Revision{{Number: "1.1", State: "Exp"}}},
		{SourcePath: "mod/removed", Head: "1.2", InAttic: true, Revisions: []*Revision{{Number: "1.1", State: "Exp"}, {Number: "1.2", State: "dead"}}},
		{SourcePath: "mod/never", Head: "1.1", InAttic: true, Revisions: []*Revision{{Number: "1.1", State: "dead"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &Repository{Config: Config{CVSRoot: ":fork:" + root, Module: "mod"}, Runner: testRunner()}
	state := t.TempDir()
	modes, _, err := r.observeModes(context.Background(), in, modeObservation{
		Roots:       []string{"mod"},
		RootStage:   "Observing CVS file permissions",
		WorkDir:     state,
		ProbeCache:  filepath.Join(state, "probes.json"),
		Connections: 1,
		Progress:    newProgress(context.Background(), process.DiscardLogger(), 0, "test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// With umask 000 a writable checkout keeps the archive's execute bits and
	// grants write access to everyone.
	want := map[string]fs.FileMode{"mod/plain": 0666, "mod/script": 0777, "mod/removed": 0777}
	if len(modes) != len(want) {
		t.Fatalf("observed modes %v, want %v", modes, want)
	}
	for path, mode := range want {
		if modes[path] != mode {
			t.Fatalf("observed modes %v, want %v", modes, want)
		}
	}
}

// gitMode returns the Git mode of path in the tree of rev.
func gitMode(t *testing.T, r *Repository, rev, path string) string {
	t.Helper()
	out, err := r.git(context.Background(), nil, nil, "ls-tree", rev, "--", path)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	mode, _, _ := strings.Cut(string(out), " ")
	if mode == "" {
		t.Fatalf("%s missing from %s", path, rev)
	}
	return mode
}

// chmodArchive changes the mode of a fixture RCS archive, as an administrator
// on the CVS server would.
func chmodArchive(t *testing.T, root, archive string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(filepath.Join(root, "module", archive), mode); err != nil {
		t.Fatal(err)
	}
}

// A new destination gives files of executable archives mode 100755, on the
// trunk as well as for a file that only lives on a branch in the Attic. A
// later chmod is accepted as changed history of that file only: canonical
// history rewrites the branches and supersedes the old generation, growing
// history adds correction commits on top of them.
func TestSyncKeepsExecutableModes(t *testing.T) {
	t.Parallel()
	for _, history := range []string{"growing", "canonical"} {
		t.Run(history, func(t *testing.T) {
			t.Parallel()
			testSyncKeepsExecutableModes(t, history)
		})
	}
}

func testSyncKeepsExecutableModes(t *testing.T, history string) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	chmodArchive(t, root, "active,v", 0555)
	chmodArchive(t, root, "Attic/branch-only,v", 0555)
	r := openHistoryFixture(t, root, "full", history)
	if !r.Config.Policy.Executable {
		t.Fatal("new destination does not keep executable modes")
	}
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	branch := "refs/heads/feature"
	for _, c := range []struct{ rev, path, mode string }{
		{trunkRef, "active", "100755"}, {trunkRef, "binary file", "100644"},
		{branch, "active", "100755"}, {branch, "branch-only", "100755"},
	} {
		if mode := gitMode(t, r, c.rev, c.path); mode != c.mode {
			t.Fatalf("%s:%s has mode %s, want %s", c.rev, c.path, mode, c.mode)
		}
	}
	original, before := r.StateOID, maps.Clone(r.Manifest.Refs)

	chmodArchive(t, root, "active,v", 0444)
	s, err := r.Sync(ctx, Options{AcceptHistoryChanges: true})
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if s.HistoryChanged == 0 || s.HistoryRemoved != 0 {
		t.Fatalf("chmod not accepted as changed history: %+v", s)
	}
	if history == "canonical" && supersededRefs(t, r)[s.Superseded] != original {
		t.Fatalf("chmod did not supersede the old generation: %+v", s)
	}
	if history == "growing" {
		// One correction per line with the file: trunk, the vendor branch
		// and feature.
		if s.Superseded != "" || len(supersededRefs(t, r)) != 0 || s.Corrections != 3 {
			t.Fatalf("chmod not corrected on top of growing history: %+v", s)
		}
		requireFastForward(t, r, original, before)
	}
	for _, c := range []struct{ rev, path, mode string }{
		{trunkRef, "active", "100644"}, {branch, "active", "100644"}, {branch, "branch-only", "100755"},
	} {
		if mode := gitMode(t, r, c.rev, c.path); mode != c.mode {
			t.Fatalf("after chmod %s:%s has mode %s, want %s", c.rev, c.path, mode, c.mode)
		}
	}
	if _, err := r.Verify(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
}

// trust-append-only takes the executable flag from authenticated cached
// records. Like an edit of old content, a chmod alone surfaces only when the
// archive gets a new revision or a scheduled content check reads it again.
func TestTrustModeObservesModesOfChangedArchives(t *testing.T) {
	t.Parallel()
	root, work, _ := fixture(t)
	ctx := context.Background()
	chmodArchive(t, root, "active,v", 0555)
	chmodArchive(t, root, "binary file,v", 0555)
	r := openFixture(t, root, "trust-append-only")
	month := 30 * 24 * time.Hour
	options := Options{AcceptHistoryChanges: true, ContentCheckInterval: month}
	if _, err := r.Sync(ctx, options); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	trusted := r.StateOID

	chmodArchive(t, root, "active,v", 0444)
	chmodArchive(t, root, "binary file,v", 0444)
	if s, err := r.Sync(ctx, options); err != nil || r.StateOID != trusted || s.ObjectWrites != 0 {
		t.Fatalf("unchanged archives were observed again: %+v, %v", s, err)
	}

	// A new branch revision of active makes its archive mode current again.
	writeFixture(t, filepath.Join(work, "active"), []byte("branch again\n"))
	fixtureCVS(t, root, work, "commit", "-m", "branch again", "active")
	s, err := r.Sync(ctx, options)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if s.HistoryChanged == 0 || gitMode(t, r, trunkRef, "active") != "100644" || gitMode(t, r, trunkRef, "binary file") != "100755" {
		t.Fatalf("new revision did not refresh only its archive mode: %+v", s)
	}

	state := r.readMaintenance()
	state.LastContentCheck = state.LastContentCheck.Add(-month)
	if err := r.writeMaintenance(state); err != nil {
		t.Fatal(err)
	}
	if s, err = r.Sync(ctx, options); err != nil || !s.ContentChecked || s.HistoryChanged == 0 {
		t.Fatalf("content check missed the chmod: %+v, %v", s, err)
	}
	if mode := gitMode(t, r, trunkRef, "binary file"); mode != "100644" {
		t.Fatalf("binary file kept mode %s after the content check", mode)
	}
}
