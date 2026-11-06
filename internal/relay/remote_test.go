package relay

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"cvsgitrelay/internal/diag"
)

// remoteRefs lists the refs of a bare repository as a name to OID map.
func remoteRefs(t *testing.T, path string) map[string]string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+path, "for-each-ref", "--format=%(refname) %(objectname)").Output()
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, oid, ok := strings.Cut(line, " "); ok {
			refs[name] = oid
		}
	}
	return refs
}

func gitIn(t *testing.T, path string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"--git-dir=" + path}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Sync pushes the state ref and every projection to the remote, forces the
// remote back to the local generation, prunes refs the local generation does
// not have, and keeps local bookkeeping refs to itself.
func TestSyncPushesToRemote(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	remote := t.TempDir()
	gitIn(t, remote, "init", "--bare", "--quiet", remote)

	s, err := r.Sync(ctx, Options{Remote: remote})
	if err != nil || !s.Pushed {
		t.Fatalf("sync with remote: pushed=%v, %v", s.Pushed, diag.Details(err, false))
	}
	pushed := remoteRefs(t, remote)
	for ref, oid := range r.Manifest.Refs {
		if pushed[ref] != oid {
			t.Errorf("%s: remote has %q, want %q", ref, pushed[ref], oid)
		}
	}
	if pushed[stateRef] != r.StateOID {
		t.Fatalf("remote state %q, want %q", pushed[stateRef], r.StateOID)
	}
	if _, err := exec.Command("git", "--git-dir="+remote, "fsck", "--no-dangling").CombinedOutput(); err != nil {
		t.Fatal("remote is incomplete:", err)
	}

	// Someone changes the remote, and the local destination gains a backup
	// ref that must stay local. An unchanged sync repairs the remote.
	gitIn(t, remote, "update-ref", "refs/tags/stray", r.StateOID)
	// A diverged branch: a commit that is not an ancestor of the local one.
	diverged := gitIn(t, remote, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit-tree", "-m", "diverged", gitIn(t, remote, "rev-parse", stateRef+"^{tree}"))
	gitIn(t, remote, "update-ref", trunkRef, diverged)
	gitIn(t, r.Path, "update-ref", "refs/cvsgitrelay/backup/local", r.StateOID)
	if s, err := r.Sync(ctx, Options{Remote: remote}); err != nil || !s.Pushed {
		t.Fatalf("repair sync: pushed=%v, %v", s.Pushed, diag.Details(err, false))
	}
	pushed = remoteRefs(t, remote)
	if _, ok := pushed["refs/tags/stray"]; ok {
		t.Error("a ref the local generation lacks was not pruned")
	}
	if _, ok := pushed["refs/cvsgitrelay/backup/local"]; ok {
		t.Error("a local backup ref was pushed")
	}
	for ref, oid := range r.Manifest.Refs {
		if pushed[ref] != oid {
			t.Errorf("after repair %s: remote has %q, want %q", ref, pushed[ref], oid)
		}
	}
}

// A remote that cannot be reached fails the sync after the local generation
// is published, and the next sync pushes again.
func TestSyncReportsFailedPush(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	remote := t.TempDir()
	if _, err := r.Sync(ctx, Options{Remote: remote + "/missing"}); err == nil || !strings.Contains(err.Error(), "published locally") {
		t.Fatalf("a failed push must fail the sync visibly, got %v", err)
	}
	if r.StateOID == "" || remoteRefs(t, r.Path)[stateRef] != r.StateOID {
		t.Fatal("the local generation was not published")
	}
	gitIn(t, remote, "init", "--bare", "--quiet", remote)
	if s, err := r.Sync(ctx, Options{Remote: remote}); err != nil || !s.Pushed || remoteRefs(t, remote)[stateRef] != r.StateOID {
		t.Fatalf("retry after failure: pushed=%v, %v", s.Pushed, err)
	}
}

func TestValidateRemote(t *testing.T) {
	for _, ok := range []string{"", "git@github.com:owner/repo.git", "ssh://git@github.com/owner/repo.git", "https://github.com/owner/repo.git", "/srv/repo.git"} {
		if err := ValidateRemote(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://user:token@github.com/o/r.git", "-oProxyCommand=x", "a b", "x\ny"} {
		if err := ValidateRemote(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
