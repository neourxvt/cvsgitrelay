package relay

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
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

// The token reaches the server as a Basic authorization header on the first
// request, and neither the logged command nor a failure report repeats it.
func TestPushSendsTokenWithoutLeaking(t *testing.T) {
	root, _, _ := fixture(t)
	r := openFixture(t, root, "full")
	const token = "ghp_TESTTOKEN1234567890"
	var header atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		header.CompareAndSwap(nil, req.Header.Get("Authorization"))
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer server.Close()
	var logs strings.Builder
	r.Runner.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	err := r.pushRemote(context.Background(), server.URL+"/owner/repo.git", token)
	if err == nil {
		t.Fatal("the denied push succeeded")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	if got, _ := header.Load().(string); got != want {
		t.Errorf("authorization header %q, want %q", got, want)
	}
	report := fmt.Sprint(diag.Details(err, false)) + logs.String()
	for _, secret := range []string{token, want[len("Basic "):]} {
		if strings.Contains(report, secret) {
			t.Errorf("the token leaked into diagnostics: %s", report)
		}
	}
}

// A token is sent only over https, and only to the configured remote.
func TestTokenEnvScopedToRemote(t *testing.T) {
	if env := tokenEnv("https://github.com/o/r.git", ""); env != nil {
		t.Errorf("environment without a token: %v", env)
	}
	env := strings.Join(tokenEnv("https://github.com/o/r.git", "tok"), "\n")
	if !strings.Contains(env, "GIT_CONFIG_KEY_0=http.https://github.com/o/r.git.extraHeader") {
		t.Errorf("header not scoped to the remote: %s", env)
	}
	for remote, ok := range map[string]bool{"https://github.com/o/r.git": true, "http://github.com/o/r.git": false, "git@github.com:o/r.git": false, "/srv/r.git": false} {
		if got := ValidateRemoteToken(remote) == nil; got != ok {
			t.Errorf("%s: token allowed %v, want %v", remote, got, ok)
		}
	}
}
