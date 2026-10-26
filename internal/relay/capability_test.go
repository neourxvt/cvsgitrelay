package relay

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixture uses the real CVS client, including its remote server protocol via
// :fork:, so parser and retrieval assumptions are grounded in actual output.
func fixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	if _, err := exec.LookPath("cvs"); err != nil {
		t.Skip("CVS is required")
	}
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureCVS(t, root, "", "init")
	w := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(w, 0700); err != nil {
		t.Fatal(err)
	}
	// Import establishes a vendor line as well as trunk and static tags.
	writeFixture(t, filepath.Join(w, "active"), []byte("initial\n"))
	fixtureCVS(t, root, w, "import", "-m", "vendor import", "module", "VENDOR", "RELEASE")
	w = filepath.Join(t.TempDir(), "checkout")
	fixtureCVS(t, root, filepath.Dir(w), "checkout", "-d", "checkout", "module")
	writeFixture(t, filepath.Join(w, "active"), []byte("second\n"))
	fixtureCVS(t, root, w, "commit", "-m", "second\n----------------------------\nseparator in message", "active")
	writeFixture(t, filepath.Join(w, "removed"), []byte("removed\n"))
	fixtureCVS(t, root, w, "add", "removed")
	fixtureCVS(t, root, w, "commit", "-m", "add removed", "removed")
	fixtureCVS(t, root, w, "remove", "-f", "removed")
	fixtureCVS(t, root, w, "commit", "-m", "delete removed", "removed")
	binary := []byte("\x00\r\n$Id$ $Revision$\xff\n\n")
	writeFixture(t, filepath.Join(w, "binary file"), binary)
	fixtureCVS(t, root, w, "add", "-kb", "binary file")
	fixtureCVS(t, root, w, "commit", "-m", "binary", "binary file")
	fixtureCVS(t, root, w, "tag", "-b", "feature")
	fixtureCVS(t, root, w, "update", "-r", "feature")
	writeFixture(t, filepath.Join(w, "active"), []byte("branch\n"))
	writeFixture(t, filepath.Join(w, "branch-only"), []byte("branch only\n"))
	fixtureCVS(t, root, w, "add", "branch-only")
	fixtureCVS(t, root, w, "commit", "-m", "branch change")
	return root, w, binary
}

func fixtureCVS(t *testing.T, root, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("cvs", append([]string{"-f", "-Q", "-d", root}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cvs %v: %v\n%s", args, err, stderr.Bytes())
	}
	return out
}

func writeFixture(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCVSCapability(t *testing.T) {
	root, _, binary := fixture(t)
	out := fixtureCVS(t, ":fork:"+root, "", "rlog", "module")
	for _, want := range []string{"Attic/removed,v", "revision 1.2", "state: dead;", "branch-only,v", "feature: 1.2.0.2", "revision 1.1.1.1", "symbolic names:"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in real rlog:\n%s", want, out)
		}
	}
	got := fixtureCVS(t, ":fork:"+root, "", "checkout", "-n", "-p", "-kb", "-r", "1.1", "module/binary file")
	if !bytes.Equal(got, binary) {
		t.Fatalf("binary mismatch: got %x want %x", got, binary)
	}
	if os.Getenv("CVSGITRELAY_CAPTURE") == "1" {
		// Normalize only the disposable server prefix; retain all parser bytes.
		writeFixture(t, "../../testdata/gnu-cvs-1.11.23.rlog", bytes.ReplaceAll(out, []byte(root), []byte("/fixture/cvsroot")))
	}
}
