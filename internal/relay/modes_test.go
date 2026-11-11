package relay

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"cvsgitrelay/internal/process"
)

// observeModes works without a mirror: it reports the archive mode of files in
// a root checkout and, through one historical checkout, of removed files, and
// leaves out archives that never had a live revision to expose a mode.
func TestObserveModesReportsArchiveModes(t *testing.T) {
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
	modes, err := r.observeModes(context.Background(), in, modeObservation{
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
