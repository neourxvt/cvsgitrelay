package relay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cvsgitrelay/internal/process"
)

// The main physical directory can share its name with a composite alias, as
// rxvt-unicode does. Checkout observation must bypass that alias expansion.
func TestMirrorCompositeAliasMatchingPhysicalDirectory(t *testing.T) {
	if _, err := exec.LookPath("cvs"); err != nil {
		t.Skip("CVS is required")
	}
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureCVS(t, root, "", "init")
	for _, source := range []string{"project", "library"} {
		work := t.TempDir()
		writeFixture(t, filepath.Join(work, "Changes"), []byte(source+"\n"))
		fixtureCVS(t, root, work, "import", "-m", "initial", source, "VENDOR", "FIRST")
	}
	adminParent := t.TempDir()
	fixtureCVS(t, root, adminParent, "checkout", "CVSROOT")
	admin := filepath.Join(adminParent, "CVSROOT")
	writeFixture(t, filepath.Join(admin, "modules"), []byte("project -a project_ project/lib\nproject_ -d project project\nproject/lib -d project/lib library\n"))
	fixtureCVS(t, root, admin, "commit", "-m", "define composite alias", "modules")
	// Permission and empty-directory observations must belong to physical paths,
	// rather than the composite module's checkout destination for the library.
	if err := os.Chmod(filepath.Join(root, "library", "Changes,v"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "project", "empty"), 0750); err != nil {
		t.Fatal(err)
	}
	runner := &process.Runner{Log: process.DiscardLogger(), Timeout: 10 * time.Second, MaxOutput: 16 << 20}
	output := filepath.Join(t.TempDir(), "mirror")
	if _, err := Mirror(context.Background(), runner, ":fork:"+root, "project", output, false, Options{Connections: 2}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"project", "library"} {
		got := fixtureCVS(t, ":fork:"+output, "", "checkout", "-n", "-p", "-kb", "-r", "1.1", source+"/Changes")
		if string(got) != source+"\n" {
			t.Fatalf("incorrect mirrored %s content: %q", source, got)
		}
	}
	info, err := os.Stat(filepath.Join(output, "library", "Changes,v"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 != 0111 {
		t.Fatalf("library executable mode lost: %v", info.Mode())
	}
	if info, err := os.Stat(filepath.Join(output, "project", "empty")); err != nil || !info.IsDir() {
		t.Fatalf("physical empty directory lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "project", "lib")); !os.IsNotExist(err) {
		t.Fatalf("checkout alias appeared in the physical mirror: %v", err)
	}
}

// Historical permission probes are reused while their target is unchanged, so
// an unchanged mirror run makes no per-file checkouts. A stub stands in for the
// CVS client and records every historical checkout it serves.
func TestMirrorReusesHistoricalPermissionProbes(t *testing.T) {
	bin, calls := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	stub := `#!/bin/sh
rev=
while [ $# -gt 0 ]; do
	if [ "$1" = -r ]; then rev=$2; fi
	last=$1
	shift
done
mkdir -p tree
if [ -n "$rev" ]; then
	echo "$last $rev" >> '` + calls + `'
	printf x > "tree/${last##*/}"
	chmod 755 "tree/${last##*/}"
else
	printf x > tree/live
	chmod 644 tree/live
fi
`
	if err := os.WriteFile(filepath.Join(bin, "cvs"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := t.TempDir()
	inventory := func(gone ...*Revision) *Inventory {
		t.Helper()
		in, err := buildMirrorInventory([]*File{
			{SourcePath: "mod/live", Head: "1.1", Revisions: []*Revision{{Number: "1.1", State: "Exp"}}},
			{SourcePath: "mod/gone", Head: gone[len(gone)-1].Number, InAttic: true, Revisions: gone},
		})
		if err != nil {
			t.Fatal(err)
		}
		in.Layout = []ModuleMapping{{Source: "mod"}}
		return in
	}
	// observe runs one tree observation and returns the probes CVS served so far.
	observe := func(in *Inventory, refresh bool) (int, []byte) {
		t.Helper()
		m := &cvsMirror{source: &Repository{Config: Config{CVSRoot: "/cvs", Module: "mod"}, Runner: testRunner()},
			progress: newMirrorProgress(context.Background(), process.DiscardLogger(), 0), state: state,
			modes: map[string]os.FileMode{}, modeKnown: map[string]bool{}, directoryModes: map[string]os.FileMode{}}
		if err := m.observeTree(context.Background(), in, 1, refresh); err != nil {
			t.Fatal(err)
		}
		if !m.modeKnown["mod/gone"] || m.modes["mod/gone"] != 0755 {
			t.Fatalf("historical mode not observed: %v %v", m.modes["mod/gone"], m.modeKnown["mod/gone"])
		}
		served, _ := os.ReadFile(calls)
		cache, err := os.ReadFile(filepath.Join(state, mirrorModeProbesFile))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(served), "\n"), cache
	}
	original := inventory(&Revision{Number: "1.1", State: "Exp"}, &Revision{Number: "1.2", State: "dead"})
	probes, fresh := observe(original, false)
	if probes != 1 {
		t.Fatalf("fresh observation served %d probes", probes)
	}
	probes, cached := observe(original, false)
	if probes != 1 || string(cached) != string(fresh) {
		t.Fatalf("unchanged observation probed again (%d) or changed its cache", probes)
	}
	if probes, _ = observe(original, true); probes != 2 {
		t.Fatalf("refresh did not probe again: %d", probes)
	}
	// A newer live revision is a different probe target.
	changed := inventory(&Revision{Number: "1.1", State: "Exp"}, &Revision{Number: "1.2", State: "Exp"}, &Revision{Number: "1.3", State: "dead"})
	if probes, _ = observe(changed, false); probes != 3 {
		t.Fatalf("changed probe target reused an old result: %d", probes)
	}
}
