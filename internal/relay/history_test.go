package relay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cvsgitrelay/internal/canonical"
	"cvsgitrelay/internal/diag"
)

// supersededRefs returns the superseded refs of a destination.
func supersededRefs(t *testing.T, r *Repository) map[string]string {
	t.Helper()
	refs, err := r.allRefs(context.Background())
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	kept := map[string]string{}
	for ref, oid := range refs {
		if strings.HasPrefix(ref, supersededPrefix) {
			kept[ref] = oid
		}
	}
	return kept
}

// editArchive replaces old with new in an RCS archive of the fixture, as an
// edit on the CVS server would, and returns the original bytes.
func editArchive(t *testing.T, archive, old, new string) []byte {
	t.Helper()
	original, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(original, []byte(old), []byte(new), 1)
	if bytes.Equal(changed, original) {
		t.Fatalf("%q not found in %s", old, archive)
	}
	if err := os.Chmod(archive, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, changed, 0600); err != nil {
		t.Fatal(err)
	}
	return original
}

// A changed or removed revision is published when acceptance is on, and the
// replaced generation stays reachable under one superseded ref, even through
// GC. When the revision comes back, the original generation returns exactly.
func TestAcceptedHistoryChangesKeepTheOldGeneration(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	original := r.StateOID
	archive := filepath.Join(root, "module", "binary file,v")
	editArchive(t, archive, "$Id$", "$XX$")

	s, err := r.Sync(ctx, Options{AcceptHistoryChanges: true})
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if s.HistoryChanged == 0 || s.HistoryRemoved != 0 || r.StateOID == original {
		t.Fatalf("changed content not accepted: %+v", s)
	}
	if kept := supersededRefs(t, r); len(kept) != 1 || kept[s.Superseded] != original {
		t.Fatalf("replaced generation not kept: %v, summary %q", kept, s.Superseded)
	}
	edited := r.StateOID

	// The superseded ref is not a projection: later syncs accept it as is.
	if s, err := r.Sync(ctx, Options{AcceptHistoryChanges: true}); err != nil || s.HistoryChanged != 0 || s.Superseded != "" || s.ObjectWrites != 0 {
		t.Fatalf("unchanged sync after acceptance: %+v, %v", s, err)
	}

	if err := os.Rename(archive, archive+".missing"); err != nil {
		t.Fatal(err)
	}
	s, err = r.Sync(ctx, Options{AcceptHistoryChanges: true, MaintenanceInterval: time.Hour})
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if s.HistoryRemoved == 0 || s.Maintenance != "completed" {
		t.Fatalf("removed archive not accepted: %+v", s)
	}
	kept := supersededRefs(t, r)
	if len(kept) != 2 || kept[s.Superseded] != edited {
		t.Fatalf("second replaced generation not kept: %v", kept)
	}
	// GC ran with both old generations unreachable from every projection.
	for _, state := range []string{original, edited} {
		if _, err := r.git(ctx, nil, nil, "fsck", "--no-dangling", state); err != nil {
			t.Fatalf("superseded generation %s damaged by GC: %v", state, diag.Details(err, false))
		}
	}

	if err := os.Rename(archive+".missing", archive); err != nil {
		t.Fatal(err)
	}
	if s, err := r.Sync(ctx, Options{AcceptHistoryChanges: true}); err != nil || s.HistoryChanged+s.HistoryRemoved != 0 || r.StateOID != edited {
		t.Fatalf("restored archive did not restore its generation: %+v, %v", s, err)
	}
}

// A source that lost most of its accepted history looks damaged, not edited,
// and is refused even with acceptance on.
func TestMostlyMissingHistoryIsRefused(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	if _, err := r.Sync(ctx, Options{}); err != nil {
		t.Fatal(diag.Details(err, false))
	}
	state := r.StateOID
	r.sourceReader = func(context.Context) (canonical.Inventory, sourceStats, error) {
		return canonical.Inventory{}, sourceStats{}, nil
	}
	_, err := r.Sync(ctx, Options{AcceptHistoryChanges: true})
	if diag.Code(err) != "source" || !strings.Contains(err.Error(), "SOURCE_MISSING") || !strings.Contains(err.Error(), "not accepted automatically") {
		t.Fatalf("empty source accepted: %v", err)
	}
	if r.StateOID != state || len(supersededRefs(t, r)) != 0 {
		t.Fatal("refused change published something")
	}
}

// A trust-append-only destination misses byte-only edits of old revisions
// until a scheduled content check reads every revision again.
func TestScheduledContentCheckFindsEditedContent(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "trust-append-only")
	month := 30 * 24 * time.Hour
	options := Options{AcceptHistoryChanges: true, ContentCheckInterval: month}
	if s, err := r.Sync(ctx, options); err != nil || !s.ContentChecked {
		t.Fatalf("first sync: %+v, %v", s, err)
	}
	trusted := r.StateOID
	editArchive(t, filepath.Join(root, "module", "binary file,v"), "$Id$", "$XX$")
	if s, err := r.Sync(ctx, options); err != nil || s.ContentChecked || r.StateOID != trusted {
		t.Fatalf("content check repeated within its interval: %+v, %v", s, err)
	}

	state := r.readMaintenance()
	state.LastContentCheck = state.LastContentCheck.Add(-month)
	if err := r.writeMaintenance(state); err != nil {
		t.Fatal(err)
	}
	s, err := r.Sync(ctx, options)
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	if !s.ContentChecked || s.HistoryChanged == 0 || r.StateOID == trusted {
		t.Fatalf("content check missed the edit: %+v", s)
	}
	// The cache now holds the accepted contents again.
	if s, err := r.Sync(ctx, options); err != nil || s.ContentChecked || s.HistoryChanged != 0 || s.ObjectWrites != 0 {
		t.Fatalf("sync after the content check: %+v, %v", s, err)
	}
}
