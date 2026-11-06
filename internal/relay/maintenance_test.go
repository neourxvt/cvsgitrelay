package relay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cvsgitrelay/internal/diag"
)

// GC is due once the interval has passed since the last success, waits a day
// after a failure, and is not postponed by a record from a clock set ahead.
func TestMaintenanceDue(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	month := 30 * 24 * time.Hour
	for name, tc := range map[string]struct {
		state maintenanceState
		want  bool
	}{
		"never":           {maintenanceState{}, true},
		"recent":          {maintenanceState{LastSuccess: now.Add(-month + time.Hour)}, false},
		"interval passed": {maintenanceState{LastSuccess: now.Add(-month)}, true},
		"failed recently": {maintenanceState{LastSuccess: now.Add(-2 * month), LastFailure: now.Add(-time.Hour)}, false},
		"failed long ago": {maintenanceState{LastSuccess: now.Add(-2 * month), LastFailure: now.Add(-maintenanceRetry)}, true},
		"future success":  {maintenanceState{LastSuccess: now.Add(month)}, true},
	} {
		if got := tc.state.due(now, month); got != tc.want {
			t.Errorf("%s: due %v, want %v", name, got, tc.want)
		}
	}
}

// looseObjects returns the number of loose objects in a destination.
func looseObjects(t *testing.T, r *Repository) string {
	t.Helper()
	out, err := r.git(context.Background(), nil, nil, "count-objects", "-v")
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "count: "); ok {
			return v
		}
	}
	return ""
}

// writeLoose stores an unreachable blob and backdates it by age.
func writeLoose(t *testing.T, r *Repository, body string, age time.Duration) string {
	t.Helper()
	out, err := r.git(context.Background(), []byte(body), nil, "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(diag.Details(err, false))
	}
	oid := strings.TrimSpace(string(out))
	old := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(r.Path, "objects", oid[:2], oid[2:]), old, old); err != nil {
		t.Fatal(err)
	}
	return oid
}

// Scheduled maintenance runs during the sync that finds it due, leaves no
// loose objects, deletes only long unreachable objects, and
// does not run again before the interval has passed.
func TestSyncRunsScheduledMaintenance(t *testing.T) {
	root, _, _ := fixture(t)
	ctx := context.Background()
	r := openFixture(t, root, "full")
	month := 30 * 24 * time.Hour
	if s, err := r.Sync(ctx, Options{}); err != nil || s.Maintenance != "" {
		t.Fatalf("sync without maintenance: %q, %v", s.Maintenance, err)
	}
	stale := writeLoose(t, r, "unreachable for a month\n", month)
	recent := writeLoose(t, r, "unreachable since today\n", time.Minute)
	if _, err := os.Stat(filepath.Join(r.Path, maintenanceRecord)); !os.IsNotExist(err) {
		t.Fatal("maintenance ran although it was not enabled")
	}

	s, err := r.Sync(ctx, Options{MaintenanceInterval: month})
	if err != nil || s.Maintenance != "completed" {
		t.Fatalf("first scheduled sync: %q, %v", s.Maintenance, err)
	}
	if loose := looseObjects(t, r); loose != "0" {
		t.Fatalf("%s loose objects remain after maintenance", loose)
	}
	if _, err := r.git(ctx, nil, nil, "cat-file", "-e", stale); err == nil {
		t.Fatal("an object unreachable for a month was kept")
	}
	if _, err := r.git(ctx, nil, nil, "cat-file", "-e", recent); err != nil {
		t.Fatal("a recently unreachable object was deleted")
	}
	if _, err := r.git(ctx, nil, nil, "fsck", "--no-dangling"); err != nil {
		t.Fatal(diag.Details(err, false))
	}

	if s, err := r.Sync(ctx, Options{MaintenanceInterval: month}); err != nil || s.Maintenance != "" || s.ObjectWrites != 0 {
		t.Fatalf("maintenance repeated within its interval: %q, %d writes, %v", s.Maintenance, s.ObjectWrites, err)
	}
	state := r.readMaintenance()
	state.LastSuccess = state.LastSuccess.Add(-month)
	if err := r.writeMaintenance(state); err != nil {
		t.Fatal(err)
	}
	if s, err := r.Sync(ctx, Options{MaintenanceInterval: month}); err != nil || s.Maintenance != "completed" {
		t.Fatalf("maintenance did not run after its interval: %q, %v", s.Maintenance, err)
	}
}
