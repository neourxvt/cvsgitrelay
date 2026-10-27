package relay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

// A separate process is essential: returned fault errors cannot establish that
// kernel-released writer locks and rooted journals survive an actual SIGKILL.
func TestV2KillHelper(t *testing.T) {
	path := os.Getenv("CVSGITRELAY_KILL_REPO")
	if path == "" {
		t.Skip("subprocess helper")
	}
	r, err := Open(context.Background(), testRunner(), path)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	defer r.Close()
	_, err = r.Sync(context.Background(), Options{Failpoint: "kill:" + os.Getenv("CVSGITRELAY_KILL_STAGE")})
	t.Fatalf("kill failpoint returned: %v", err)
}

func TestV2ProcessCrashRecovery(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	reference := openFixture(t, root, "full")
	if _, err := reference.Sync(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	want := reference.StateOID
	for _, stage := range []string{"fetch-complete", "commit-written", "objects-written", "before-journal", "journal-written", "before-publish", "after-publish", "projection-updated:1", "projection-updated:2", "before-journal-cleanup"} {
		t.Run(stage, func(t *testing.T) {
			r := openFixture(t, root, "full")
			path := r.Path
			r.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestV2KillHelper$")
			cmd.Env = append(os.Environ(), "CVSGITRELAY_KILL_REPO="+path, "CVSGITRELAY_KILL_STAGE="+stage, "TZ=Pacific/Auckland", "LC_ALL=de_DE.UTF-8", "GIT_AUTHOR_NAME=ignored", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.autocrlf", "GIT_CONFIG_VALUE_0=true")
			out, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not killed at %s: %v\n%s", stage, err, out)
			}
			r, err = Open(ctx, testRunner(), path)
			if err != nil {
				t.Fatal(diag.Details(err, false))
			}
			defer r.Close()
			if _, err := r.Sync(ctx, Options{}); err != nil {
				t.Fatal(diag.Details(err, false))
			}
			if r.StateOID != want {
				t.Fatal("crash/poll history affected final generation")
			}
		})
	}
}

func TestV2DamagedRecoveryEvidence(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	for _, failure := range []string{"lost-journal", "missing-journal-object", "corrupt-object", "changed-state"} {
		t.Run(failure, func(t *testing.T) {
			r := openFixture(t, root, "full")
			old := r.StateOID
			if _, err := r.Sync(ctx, Options{Failpoint: "after-publish"}); diag.Code(err) != "injected" {
				t.Fatal(err)
			}
			refs, err := r.allRefs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "lost-journal":
				if err := r.cas(ctx, journalRef, refs[journalRef], ""); err != nil {
					t.Fatal(err)
				}
			case "missing-journal-object":
				oid := refs[journalRef]
				if err := os.Remove(filepath.Join(r.Path, "objects", oid[:2], oid[2:])); err != nil {
					t.Fatal(err)
				}
			case "corrupt-object":
				oid := refs[stateRef]
				path := filepath.Join(r.Path, "objects", oid[:2], oid[2:])
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-state":
				if err := r.cas(ctx, stateRef, refs[stateRef], old); err != nil {
					t.Fatal(err)
				}
				if err := r.cas(ctx, trunkRef, "", refs[journalRef]); err != nil {
					t.Fatal(err)
				}
			}
			path := r.Path
			r.Close()
			if other, err := Open(ctx, testRunner(), path); err == nil {
				other.Close()
				t.Fatal("damaged evidence guessed a recovery")
			}
		})
	}
}

func TestV2FailedReadsAndProjectionDeletion(t *testing.T) {
	root, work, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	state := r.StateOID
	base, _, err := r.readSourceScope(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{1, 2} {
		calls := 0
		r.sourceReader = func(context.Context) (canonical.Inventory, sourceStats, error) {
			calls++
			if calls == at {
				return canonical.Inventory{}, sourceStats{}, diag.New("source", "fixture.read", "failed/partial fetch", nil)
			}
			return base, sourceStats{}, nil
		}
		if _, err := r.Sync(ctx, Options{}); err == nil {
			t.Fatal("failed read published")
		}
		refs, err := r.allRefs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if refs[stateRef] != state {
			t.Fatal("failed scan advanced state")
		}
	}
	r.sourceReader = nil
	// Deletions are recorded in the same recovery evidence as creations.
	fixtureCVS(t, root, work, "tag", "-d", "RELEASE")
	if _, err := r.Sync(ctx, Options{Failpoint: "after-publish"}); diag.Code(err) != "injected" {
		t.Fatal(err)
	}
	path := r.Path
	r.Close()
	r, err = Open(ctx, testRunner(), path)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	defer r.Close()
	if ref, _ := r.Config.Policy.TagRef("RELEASE"); r.Manifest.Refs[ref] != "" {
		t.Fatal("deleted tag survived recovery")
	}
}
