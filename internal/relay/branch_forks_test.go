package relay

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

// checkoutBlobs returns the blob ID of every file in a CVS checkout of the
// given revision selector, keyed by path, skipping CVS administration.
func checkoutBlobs(t *testing.T, root, selector string) map[string]string {
	t.Helper()
	parent := t.TempDir()
	fixtureCVS(t, root, parent, "checkout", "-kb", "-r", selector, "-d", "out", "module")
	blobs := map[string]string{}
	dir := filepath.Join(parent, "out")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "CVS" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		blobs[rel] = canonical.OID("blob", data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return blobs
}

// gitBlobs returns the blob ID of every file in the tree of a commit.
func gitBlobs(t *testing.T, r *Repository, commit string) map[string]string {
	t.Helper()
	entries, err := testTreeEntries(t, r, commit)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	blobs := map[string]string{}
	for path, entry := range entries {
		blobs[path] = entry.OID
	}
	return blobs
}

// Branches whose files fork from different lines are ordinary CVS history:
// a branch of an imported module forks unchanged files from the vendor line,
// and a branch of a branch forks the files unchanged there from the trunk.
// Both sync, match their CVS checkouts, and adding a file on a branch later
// only appends to it.
func TestBranchesForkingFromSeveralLines(t *testing.T) {
	t.Parallel()
	root, _, _ := fixture(t)
	ctx := context.Background()
	imported := filepath.Join(t.TempDir(), "import")
	if err := os.Mkdir(imported, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(imported, "edited"), []byte("imported\n"))
	writeFixture(t, filepath.Join(imported, "untouched"), []byte("imported\n"))
	fixtureCVS(t, root, imported, "import", "-m", "second import", "module/lib", "LIBVENDOR", "LIB_1")
	work := filepath.Join(t.TempDir(), "work")
	fixtureCVS(t, root, filepath.Dir(work), "checkout", "-d", "work", "module")
	writeFixture(t, filepath.Join(work, "lib", "edited"), []byte("edited on trunk\n"))
	fixtureCVS(t, root, work, "commit", "-m", "edit on trunk", "lib/edited")

	// lib/untouched forks from its vendor revision, lib/edited from trunk.
	fixtureCVS(t, root, work, "tag", "-b", "BR")
	fixtureCVS(t, root, work, "update", "-r", "BR")
	writeFixture(t, filepath.Join(work, "lib", "edited"), []byte("edited on BR\n"))
	fixtureCVS(t, root, work, "commit", "-m", "edit on BR", "lib/edited")
	// Only lib/edited changed on BR, so SUB forks the other files elsewhere.
	fixtureCVS(t, root, work, "tag", "-b", "SUB")

	r := openFixture(t, root, "full")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	for _, name := range []string{"BR", "SUB"} {
		ref, _, _ := r.Config.Policy.BranchRef(name)
		if got, want := gitBlobs(t, r, r.Manifest.Refs[ref]), checkoutBlobs(t, root, name); !maps.Equal(got, want) {
			t.Fatalf("branch %s differs from its CVS checkout:\n git %v\n cvs %v", name, got, want)
		}
	}

	brRef, _, _ := r.Config.Policy.BranchRef("BR")
	before := r.Manifest.Refs[brRef]
	writeFixture(t, filepath.Join(work, "lib", "added"), []byte("added on BR\n"))
	fixtureCVS(t, root, work, "add", "lib/added")
	fixtureCVS(t, root, work, "commit", "-m", "add on BR", "lib/added")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	after := r.Manifest.Refs[brRef]
	if before == after {
		t.Fatal("branch did not advance")
	}
	if _, err := r.git(ctx, nil, nil, "merge-base", "--is-ancestor", before, after); err != nil {
		t.Fatal("adding a file on the branch rewrote it instead of appending")
	}
	if got, want := gitBlobs(t, r, after), checkoutBlobs(t, root, "BR"); !maps.Equal(got, want) {
		t.Fatalf("branch BR differs from its CVS checkout:\n git %v\n cvs %v", got, want)
	}
}
