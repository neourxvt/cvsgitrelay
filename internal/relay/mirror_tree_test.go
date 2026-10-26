package relay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
