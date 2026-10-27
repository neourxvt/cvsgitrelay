package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
	"cvsgitrelay/internal/process"
)

func testRunner() *process.Runner {
	return &process.Runner{Log: process.DiscardLogger(), Timeout: time.Minute, MaxOutput: 32 << 20}
}
func openFixture(t testing.TB, root, verify string) *Repository {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "mirror.git")
	runner := testRunner()
	if err := Init(context.Background(), runner, dest, root, "module", "test-source", "include", verify, nil); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	r, err := Open(context.Background(), runner, dest)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	t.Cleanup(r.Close)
	return r
}

type testTreeEntry struct{ OID string }

func testTreeEntries(t *testing.T, r *Repository, oid string) (map[string]testTreeEntry, error) {
	t.Helper()
	data, err := r.git(context.Background(), nil, nil, "ls-tree", "-rz", oid)
	if err != nil {
		return nil, err
	}
	entries := map[string]testTreeEntry{}
	for _, row := range strings.Split(string(data), "\x00") {
		if row == "" {
			continue
		}
		head, path, ok := strings.Cut(row, "\t")
		f := strings.Fields(head)
		if !ok || len(f) != 3 || f[0] != "100644" {
			return nil, fmt.Errorf("invalid tree")
		}
		entries[path] = testTreeEntry{f[2]}
	}
	return entries, nil
}

func TestV2RealCVSBytesAndNoop(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, ":fork:"+root, "full")
	before := r.StateOID
	s, err := r.Sync(ctx, Options{Scope: "all", Connections: 4})
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if len(r.Manifest.Revisions) != 9 || s.Imported != 9 || before == r.StateOID {
		t.Fatalf("coverage: %+v", s)
	}
	in, _, err := r.readSourceScope(ctx, 1)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	for _, rev := range in.Records {
		if rev.State != "live" {
			continue
		}
		got := fixtureCVS(t, ":fork:"+root, "", "checkout", "-n", "-p", "-kb", "-r", rev.Key.Number, rev.Key.Archive)
		if !bytes.Equal(got, rev.Content) {
			t.Fatalf("import and CVS -kb disagree: %s %s", rev.Key.Archive, rev.Key.Number)
		}
	}
	state := r.StateOID
	s, err = r.Sync(ctx, Options{Scope: "all", Connections: 1})
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if state != r.StateOID || s.Published != 0 || s.ObjectWrites != 0 || s.SourceRequests == 0 {
		t.Fatal("no-op failed to verify source or changed generation")
	}
	if _, err := r.Sync(ctx, Options{Scope: "trunk"}); err == nil {
		t.Fatal("partial publication allowed")
	}
	if _, err := r.Verify(ctx, Options{Scope: "all"}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if other, err := Open(ctx, testRunner(), r.Path); err == nil {
		other.Close()
		t.Fatal("writer lock bypassed")
	}
	r.Close()
	again, err := Open(ctx, testRunner(), r.Path)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	defer again.Close()
	if again.StateOID != state {
		t.Fatal("restart changed canonical state")
	}
}

func TestV2FailedFirstScanAndLostState(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if r.StateOID != "" {
		t.Fatal("initialization published an unscanned inventory")
	}
	r.sourceReader = func(context.Context) (canonical.Inventory, sourceStats, error) {
		return canonical.Inventory{}, sourceStats{}, diag.New("source", "fixture.scan", "failed first scan", nil)
	}
	if _, err := r.Sync(ctx, Options{}); err == nil {
		t.Fatal("failed first scan succeeded")
	}
	refs, err := r.allRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatal("failed first scan wrote refs")
	}
	r.sourceReader = nil
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	for ref, oid := range r.Manifest.Refs {
		if err := r.cas(ctx, ref, oid, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.cas(ctx, stateRef, r.StateOID, ""); err != nil {
		t.Fatal(err)
	}
	path := r.Path
	r.Close()
	if other, err := Open(ctx, testRunner(), path); err == nil {
		other.Close()
		t.Fatal("lost published state was treated as a fresh destination")
	}
}

func TestV2FreshIncrementalConvergenceAndSymbols(t *testing.T) {
	root, work, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	// Late members, aliases, tag movement and symbol deletion are mutable
	// current-state changes; none may freeze an observation-time branch seed.
	writeFixture(t, filepath.Join(work, "late"), []byte("late\n"))
	fixtureCVS(t, root, work, "add", "late")
	fixtureCVS(t, root, work, "commit", "-m", "late branch file", "late")
	fixtureCVS(t, root, work, "admin", "-nalias:feature")
	fixtureCVS(t, root, work, "tag", "-F", "-r", "feature", "RELEASE")
	fixtureCVS(t, root, work, "tag", "-B", "-d", "feature")
	if _, err := r.Sync(ctx, Options{Connections: 1}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	copyRoot := filepath.Join(t.TempDir(), "aliased-source")
	if err := os.CopyFS(copyRoot, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	fresh := openFixture(t, ":fork:"+copyRoot, "full")
	if _, err := fresh.Sync(ctx, Options{Connections: 4}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if fresh.StateOID != r.StateOID || !maps.Equal(fresh.Manifest.Refs, r.Manifest.Refs) || !maps.Equal(fresh.Manifest.Revisions, r.Manifest.Revisions) {
		t.Fatal("incremental and fresh differ")
	}
	ref, _ := canonical.EncodedRef("refs/tags/cvs/", "RELEASE")
	if r.Manifest.Refs[ref] == "" {
		t.Fatal("tag snapshot absent")
	}
	fixtureCVS(t, root, work, "tag", "-d", "RELEASE")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if r.Manifest.Refs[ref] != "" {
		t.Fatal("deleted tag retained")
	}
}

func TestV2SourceMutationMissingRaceAndTrust(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	strict := openFixture(t, root, "full")
	trust := openFixture(t, root, "trust-append-only")
	for _, r := range []*Repository{strict, trust} {
		if _, err := r.Sync(ctx, Options{}); err != nil {
			t.Fatal(diag.Details(err, false))
		}
	}
	archive := filepath.Join(root, "module", "binary file,v")
	original, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if err := os.Chmod(archive, 0600); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(original, []byte("$Id$"), []byte("$XX$"), 1)
	if bytes.Equal(changed, original) {
		t.Fatal("fixture payload missing")
	}
	if err := os.WriteFile(archive, changed, 0600); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	state := strict.StateOID
	if _, err := strict.Sync(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "SOURCE_MUTATED") {
		t.Fatalf("silent mutation accepted: %v", err)
	}
	if strict.StateOID != state {
		t.Fatal("mutation published")
	}
	// The opt-in mode explicitly demonstrates its weaker old-byte detection.
	trustedState := trust.StateOID
	if _, err := trust.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if trust.StateOID != trustedState {
		t.Fatal("trusted old content changed")
	}
	if err := os.WriteFile(archive, original, 0600); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	// Cache corruption is nonauthoritative and recovers from the source.
	entries, err := os.ReadDir(filepath.Join(trust.Path, "cvsgitrelay-cache"))
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	for _, entry := range entries {
		if err := os.WriteFile(filepath.Join(trust.Path, "cvsgitrelay-cache", entry.Name()), []byte("corrupt"), 0600); err != nil {
			t.Fatal(diag.Details(err, false))
		}
	}
	if _, err := trust.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if trust.StateOID != trustedState {
		t.Fatal("corrupt cache affected state")
	}
	if err := os.Rename(archive, archive+".missing"); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if _, err := strict.Sync(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "SOURCE_MISSING") {
		t.Fatalf("disappearance: %v", err)
	}
	if err := os.Rename(archive+".missing", archive); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	base, _, err := strict.readSourceScope(ctx, 1)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	calls := 0
	strict.sourceReader = func(context.Context) (canonical.Inventory, sourceStats, error) {
		calls++
		in := base
		if calls == 2 {
			in.Symbols = nil
		}
		return in, sourceStats{}, nil
	}
	if _, err := strict.Sync(ctx, Options{}); err == nil {
		t.Fatal("unstable source accepted")
	}
	if strict.StateOID != state {
		t.Fatal("race published")
	}
}

func TestV2ImmutableMetadataAndMutableAdministration(t *testing.T) {
	root, _, _ := fixture(t)
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
	for _, field := range []string{"author", "log", "timestamp", "state", "parent"} {
		t.Run(field, func(t *testing.T) {
			in := canonical.Inventory{Records: slices.Clone(base.Records), Symbols: base.Symbols}
			revision := &in.Records[0]
			switch field {
			case "author":
				revision.Author = "changed"
			case "log":
				revision.Log = []byte("changed log")
			case "timestamp":
				revision.Time++
			case "state":
				if revision.State == "live" {
					revision.State = "dead"
				} else {
					revision.State = "live"
				}
			case "parent":
				parent := revision.Key
				revision.Parent = &parent
			}
			r.sourceReader = func(context.Context) (canonical.Inventory, sourceStats, error) { return in, sourceStats{}, nil }
			if _, err := r.Sync(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "SOURCE_MUTATED") {
				t.Fatalf("metadata mutation accepted: %v", err)
			}
			if r.StateOID != state {
				t.Fatal("metadata failure published")
			}
		})
	}
	r.sourceReader = nil
	// A file's current DefaultBranch is administrative state, independent of
	// immutable numeric vendor ancestry and historical revision provenance.
	path := filepath.Join(root, "module", "active,v")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := r.scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	access := bytes.Index(data, []byte("access"))
	if access < 0 {
		t.Fatal("fixture administration missing")
	}
	changed := append([]byte("head "+listed.Files["active"].Head+";\nbranch 1.1.1;\n"), data[access:]...)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if r.StateOID != state {
		t.Fatal("DefaultBranch entered canonical state")
	}
}

func TestV2BinaryCheckoutSemantics(t *testing.T) {
	root, work, binary := fixture(t)
	next := append(slices.Clone(binary), []byte("\x00\xff\r\nunterminated")...)
	writeFixture(t, filepath.Join(work, "binary file"), next)
	fixtureCVS(t, root, work, "commit", "-m", "binary branch delta", "binary file")
	r := openFixture(t, root, "full")
	in, _, err := r.readSourceScope(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, record := range in.Records {
		if record.Key.Archive != "module/binary file" || record.State != "live" {
			continue
		}
		count++
		served := fixtureCVS(t, root, "", "checkout", "-n", "-p", "-kb", "-r", record.Key.Number, record.Key.Archive)
		if !bytes.Equal(served, record.Content) {
			t.Fatalf("binary delta differs at %s", record.Key.Number)
		}
	}
	if count != 2 {
		t.Fatal("binary line coverage")
	}
}

func TestV2StateAloneSurvivesGC(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	for ref, oid := range r.Manifest.Refs {
		if err := r.cas(ctx, ref, oid, ""); err != nil {
			t.Fatal(diag.Details(err, false))
		}
	}
	if _, err := r.git(ctx, nil, nil, "reflog", "expire", "--expire=now", "--all"); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if _, err := r.git(ctx, nil, nil, "gc", "--prune=now"); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if _, err := r.readState(ctx, r.StateOID); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	for _, entry := range r.Manifest.Revisions {
		if _, err := r.git(ctx, nil, nil, "cat-file", "commit", entry.Event); err != nil {
			t.Fatal(diag.Details(err, false))
		}
	}
	for ref, oid := range r.Manifest.Refs {
		if err := r.cas(ctx, ref, "", oid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Verify(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
}

func TestV2PublicationRecoveryAndDivergence(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	reference := openFixture(t, root, "full")
	if _, err := reference.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	expectedState := reference.StateOID
	for _, stage := range []string{"fetch-complete", "scan-complete", "commit-written", "objects-written", "before-journal", "journal-written", "before-publish", "after-publish", "projection-updated:1", "projection-updated:3", "before-journal-cleanup"} {
		t.Run(stage, func(t *testing.T) {
			r := openFixture(t, root, "full")
			old := r.StateOID
			if _, err := r.Sync(ctx, Options{Failpoint: stage}); diag.Code(err) != "injected" {
				t.Fatalf("fault did not trigger: %v", err)
			}
			refs, err := r.allRefs(ctx)
			if err != nil {
				t.Fatal(diag.Details(err, false))
			}
			// A state-based reader can resolve the complete new manifest even
			// while ordinary projections still expose a mixture of generations.
			if _, err := r.readState(ctx, refs[stateRef]); err != nil {
				t.Fatal(diag.Details(err, false))
			}
			if stage == "scan-complete" && refs[stateRef] != old {
				t.Fatal("failed scan advanced state")
			}
			path := r.Path
			r.Close()
			r, err = Open(ctx, testRunner(), path)
			if err != nil {
				t.Fatal(diag.Details(err, false))
			}
			defer r.Close()
			if _, err := r.Sync(ctx, Options{}); err != nil {
				t.Fatal(diag.Details(err, false))
			}
			if r.StateOID != expectedState {
				t.Fatal("recovery differs from uninterrupted canonical generation")
			}
			want := r.StateOID
			if err := r.recoverPublication(ctx); err != nil {
				t.Fatal(diag.Details(err, false))
			}
			if r.StateOID != want {
				t.Fatal("recovery not idempotent")
			}
		})
	}
	t.Run("unknown projection", func(t *testing.T) {
		r := openFixture(t, root, "full")
		if _, err := r.Sync(ctx, Options{Failpoint: "after-publish"}); diag.Code(err) != "injected" {
			t.Fatal(diag.Details(err, false))
		}
		refs, err := r.allRefs(ctx)
		if err != nil {
			t.Fatal(diag.Details(err, false))
		}
		foreign := refs[journalRef]
		if err := r.cas(ctx, trunkRef, "", foreign); err != nil {
			t.Fatal(diag.Details(err, false))
		}
		path := r.Path
		r.Close()
		if other, err := Open(ctx, testRunner(), path); err == nil {
			other.Close()
			t.Fatal("external update overwritten")
		}
	})
}

func TestV2RejectPolicyDrift(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	path := r.Path
	refs, err := r.allRefs(ctx)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	r.Close()
	configPath := filepath.Join(path, "cvsgitrelay.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	config.Policy.Verify = "trust-append-only"
	modified, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, modified, 0600); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if other, err := Open(ctx, testRunner(), path); err == nil {
		other.Close()
		t.Fatal("policy drift accepted")
	}
	// Reject policy changes before publication can modify any refs.
	cmd := exec.Command("git", "--git-dir="+path, "show-ref")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	for ref, oid := range refs {
		if !strings.Contains(string(out), oid+" "+ref) {
			t.Fatal("ref rewritten during rejection")
		}
	}
}
