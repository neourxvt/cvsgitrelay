package relay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

// requireFastForward fails unless the destination kept every ref of an
// earlier generation and only moved refs and state forward from it.
func requireFastForward(t *testing.T, r *Repository, state string, refs map[string]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.git(ctx, nil, nil, "merge-base", "--is-ancestor", state, r.StateOID); err != nil {
		t.Fatalf("state %s does not descend from %s", r.StateOID, state)
	}
	for ref, old := range refs {
		next, ok := r.Manifest.Refs[ref]
		if !ok {
			t.Fatalf("%s was deleted", ref)
		}
		if _, err := r.git(ctx, nil, nil, "merge-base", "--is-ancestor", old, next); err != nil {
			t.Fatalf("%s moved from %s to %s, which does not descend from it", ref, old, next)
		}
	}
}

// Growing history adds a CVS commit as one Git commit on top of its branch,
// corrects changed and removed history with new commits instead of rewriting
// it, and keeps deleted tags, publishing a retagged one under a new name.
// Every sync only moves refs forward and verify accepts every generation.
func TestGrowingHistoryOnlyMovesForward(t *testing.T) {
	t.Parallel()
	root, work, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if !r.Config.Policy.Growing {
		t.Fatal("new destination does not grow history")
	}
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	step := func(name string, opts Options) Summary {
		t.Helper()
		state, refs := r.StateOID, r.Manifest.Refs
		s, err := r.Sync(ctx, opts)
		if err != nil {
			t.Fatalf("%s: %s", name, diag.Details(err, false))
		}
		requireFastForward(t, r, state, refs)
		if len(supersededRefs(t, r)) != 0 {
			t.Fatalf("%s: growing history superseded a generation", name)
		}
		if _, err := r.Verify(ctx, Options{}); err != nil {
			t.Fatalf("%s: verify: %s", name, diag.Details(err, false))
		}
		return s
	}

	// One CVS commit of two files becomes one commit on top of trunk.
	trunk := r.Manifest.Refs[trunkRef]
	fixtureCVS(t, root, work, "update", "-A")
	writeFixture(t, filepath.Join(work, "active"), []byte("third\n"))
	writeFixture(t, filepath.Join(work, "added"), []byte("added\n"))
	fixtureCVS(t, root, work, "add", "added")
	fixtureCVS(t, root, work, "commit", "-m", "two files at once", "active", "added")
	step("new commit", Options{})
	if parent := gitIn(t, r.Path, "rev-parse", trunkRef+"^"); parent != trunk {
		t.Fatalf("the changeset is not one commit on top of trunk: parent %s, want %s", parent, trunk)
	}
	if subject := gitIn(t, r.Path, "log", "-1", "--format=%s", trunkRef); subject != "two files at once" {
		t.Fatalf("changeset subject %q", subject)
	}
	if files := gitIn(t, r.Path, "diff", "--name-only", trunk, trunkRef); files != "active\nadded" {
		t.Fatalf("changeset changes %q", files)
	}

	// Changed history is refused without acceptance, as before.
	archive := filepath.Join(root, "module", "binary file,v")
	editArchive(t, archive, "$Id$", "$XX$")
	state := r.StateOID
	if _, err := r.Sync(ctx, Options{}); err == nil || r.StateOID != state {
		t.Fatal("changed history published without acceptance")
	}
	if s := step("changed content", Options{AcceptHistoryChanges: true}); s.HistoryChanged == 0 || s.Corrections == 0 || s.Superseded != "" {
		t.Fatalf("changed content not corrected: %+v", s)
	}
	if subject := gitIn(t, r.Path, "log", "-1", "--format=%s", trunkRef); subject != "Update files to their current CVS state" {
		t.Fatalf("correction subject %q", subject)
	}

	// A removed archive leaves the file in the published commits and removes
	// it with a correction.
	if err := os.Rename(archive, archive+".missing"); err != nil {
		t.Fatal(err)
	}
	if s := step("removed archive", Options{AcceptHistoryChanges: true}); s.HistoryRemoved == 0 || s.Corrections == 0 {
		t.Fatalf("removed archive not corrected: %+v", s)
	}
	if files := gitIn(t, r.Path, "ls-tree", "--name-only", trunkRef); strings.Contains(files, "binary file") {
		t.Fatalf("removed archive still on trunk: %q", files)
	}
	// Recorded removals are no changes of later syncs.
	if s := step("unchanged", Options{}); s.HistoryRemoved != 0 || s.ObjectWrites != 0 {
		t.Fatalf("unchanged sync after a removal: %+v", s)
	}

	// A deleted tag keeps its ref; a tag of other revisions under the same
	// name becomes its second version.
	release, _ := r.Config.Policy.TagRef("RELEASE")
	second, _ := r.Config.Policy.TagVersionRef("RELEASE", 2)
	tag := r.Manifest.Refs[release]
	// The working copy still has the file of the removed archive, so the
	// tag commands name the others.
	fixtureCVS(t, root, work, "tag", "-d", "RELEASE", "active")
	step("deleted tag", Options{})
	if r.Manifest.Refs[release] != tag {
		t.Fatal("deleted tag lost its ref")
	}
	fixtureCVS(t, root, work, "tag", "RELEASE", "active", "added")
	step("retagged", Options{})
	if r.Manifest.Refs[release] != tag || r.Manifest.Refs[second] == "" {
		t.Fatalf("retagged RELEASE not published as a second version: %v", r.Manifest.Refs)
	}
	if tree := gitIn(t, r.Path, "rev-parse", second+"^{tree}"); tree != gitIn(t, r.Path, "rev-parse", trunkRef+"^{tree}") {
		t.Fatal("second tag version does not show the tagged files")
	}
}

// The push of growing history forces nothing: a remote whose branch moved
// elsewhere rejects the push instead of losing that work, and accepts the
// next one once the branch is back.
func TestGrowingPushNeverForces(t *testing.T) {
	t.Parallel()
	for _, spec := range pushRefspecs(true) {
		if strings.HasPrefix(spec, "+") {
			t.Fatalf("growing history forces %s", spec)
		}
	}
	root, work, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	remote := t.TempDir()
	gitIn(t, remote, "init", "--bare", "--quiet", remote)
	if s, err := r.Sync(ctx, Options{Remote: remote}); err != nil || !s.Pushed {
		t.Fatalf("first push: pushed=%v, %v", s.Pushed, diag.Details(err, false))
	}
	published := r.Manifest.Refs[trunkRef]
	diverged := gitIn(t, remote, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit-tree", "-m", "diverged", gitIn(t, remote, "rev-parse", trunkRef+"^{tree}"))
	gitIn(t, remote, "update-ref", trunkRef, diverged)

	fixtureCVS(t, root, work, "update", "-A")
	writeFixture(t, filepath.Join(work, "active"), []byte("third\n"))
	fixtureCVS(t, root, work, "commit", "-m", "third", "active")
	if s, err := r.Sync(ctx, Options{Remote: remote}); err == nil || s.Pushed {
		t.Fatal("push replaced a diverged remote branch")
	}
	if remoteRefs(t, remote)[trunkRef] != diverged {
		t.Fatal("rejected atomic push changed the remote")
	}

	gitIn(t, remote, "update-ref", trunkRef, published)
	if s, err := r.Sync(ctx, Options{Remote: remote}); err != nil || !s.Pushed {
		t.Fatalf("push after repair: pushed=%v, %v", s.Pushed, diag.Details(err, false))
	}
	pushed := remoteRefs(t, remote)
	for ref, oid := range r.Manifest.Refs {
		if pushed[ref] != oid {
			t.Errorf("%s: remote has %q, want %q", ref, pushed[ref], oid)
		}
	}
}

// The publication of growing history refuses a generation that would delete
// a ref or move one sideways, whatever built it.
func TestGrowingPublicationRequiresFastForward(t *testing.T) {
	t.Parallel()
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	sideways := canonical.Generation{State: r.StateOID, Manifest: canonical.Manifest{Refs: map[string]string{}}}
	for ref, oid := range r.Manifest.Refs {
		sideways.Manifest.Refs[ref] = oid
	}
	sideways.Manifest.Refs[trunkRef] = r.Manifest.Refs["refs/heads/feature"]
	if err := r.checkFastForward(ctx, sideways); diag.Code(err) != "integrity" || !strings.HasPrefix(err.Error(), "publish.fast-forward:") {
		t.Fatalf("sideways move accepted: %v", err)
	}
	delete(sideways.Manifest.Refs, trunkRef)
	if err := r.checkFastForward(ctx, sideways); diag.Code(err) != "integrity" {
		t.Fatalf("deleted ref accepted: %v", err)
	}
}
