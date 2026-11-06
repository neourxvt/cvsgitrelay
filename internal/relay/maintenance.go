package relay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"cvsgitrelay/internal/diag"
)

// Scheduled maintenance keeps a long-running destination compact. Every sync
// that writes objects adds one incremental pack, and a rewritten generation
// leaves its old objects unreachable; automatic Git GC stays disabled so that
// it never runs outside the writer lock. When enabled, sync runs one full GC
// under its lock once the configured interval has passed.

// maintenanceRecord is the file in the destination that remembers when GC last
// ran. It is local bookkeeping and never enters canonical objects.
const maintenanceRecord = "cvsgitrelay-maintenance.json"

// maintenanceRetry is how long a failed GC waits before a later sync tries it
// again, so a GC that keeps failing does not slow down every run.
const maintenanceRetry = 24 * time.Hour

// maintenanceTimeout is the least time one GC may take. A full repack of a
// large history can need far longer than an ordinary command.
const maintenanceTimeout = time.Hour

// maintenancePrune is the age after which GC deletes unreachable objects. Two
// weeks is Git's default grace period: a reader that resolved an older
// generation before a rewrite can still finish reading it meanwhile.
const maintenancePrune = "2.weeks.ago"

type maintenanceState struct {
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastFailure time.Time `json:"last_failure,omitzero"`
	// LastContentCheck is the last sync that read every revision from the
	// source instead of the trust-append-only content cache.
	LastContentCheck time.Time `json:"last_content_check,omitzero"`
}

// passed reports whether at least d has passed since t. Never is long ago, and
// so are times after now, left by a clock that was set wrong, so a corrected
// clock cannot postpone a scheduled task indefinitely.
func passed(t, now time.Time, d time.Duration) bool {
	return t.IsZero() || t.After(now) || now.Sub(t) >= d
}

// due reports whether GC should run now: the last success is at least interval
// old and no failure happened within maintenanceRetry.
func (m maintenanceState) due(now time.Time, interval time.Duration) bool {
	return passed(m.LastSuccess, now, interval) && passed(m.LastFailure, now, maintenanceRetry)
}

// contentCheckDue reports whether the next sync should read all contents.
func (m maintenanceState) contentCheckDue(now time.Time, interval time.Duration) bool {
	return passed(m.LastContentCheck, now, interval)
}

// recordContentCheck notes a successful sync that read every revision. A lost
// record only repeats the check on the next sync.
func (r *Repository) recordContentCheck(now time.Time) {
	state := r.readMaintenance()
	state.LastContentCheck = now
	if err := r.writeMaintenance(state); err != nil && r.Runner.Log != nil {
		r.Runner.Log.Warn("Maintenance record could not be saved; the next sync checks contents again", "error", err.Error())
	}
}

// readMaintenance returns the recorded state. A missing or unreadable record
// counts as never maintained.
func (r *Repository) readMaintenance() maintenanceState {
	var m maintenanceState
	data, err := os.ReadFile(filepath.Join(r.Path, maintenanceRecord))
	if err != nil || decodeJSON(data, &m) != nil {
		return maintenanceState{}
	}
	return m
}

// writeMaintenance replaces the record atomically.
func (r *Repository) writeMaintenance(m maintenanceState) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(r.Path, ".cvsgitrelay-maintenance-")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	_, err = f.Write(append(data, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(r.Path, maintenanceRecord))
}

// gc repacks all objects into one pack with a bitmap index for serving
// fetches, keeps recently unreachable objects in a cruft pack and deletes
// those unreachable for longer than maintenancePrune. Git writes and fsyncs
// the new packs before it deletes the old ones, so an interruption leaves the
// destination complete. The caller holds the writer lock.
func (r *Repository) gc(ctx context.Context) error {
	runner := *r.Runner
	runner.Timeout = max(runner.Timeout, maintenanceTimeout)
	_, err := runner.Run(ctx, "", nil, nil, "git", r.gitArgs("-c", "core.fsync=loose-object,reference,pack,pack-metadata",
		"gc", "--quiet", "--cruft", "--prune="+maintenancePrune)...)
	return err
}

// maintain runs GC when it is due and records the outcome. The generation is
// already published, so a failure only leaves the destination less compact; it
// is logged and reported in the summary instead of failing the sync. It
// returns "completed", "failed", or "" when GC was not due.
func (r *Repository) maintain(ctx context.Context, interval time.Duration) string {
	state := r.readMaintenance()
	now := time.Now().UTC()
	if !state.due(now, interval) {
		return ""
	}
	r.phases.begin("maintenance", "Running Git maintenance", "interval", interval)
	start := time.Now()
	err := r.gc(ctx)
	if ctx.Err() != nil {
		// An interrupted GC proves nothing about the next attempt.
		return "failed"
	}
	if err != nil {
		state.LastFailure = now
	} else {
		state.LastSuccess, state.LastFailure = now, time.Time{}
	}
	recordErr := r.writeMaintenance(state)
	if r.Runner.Log != nil {
		if err != nil {
			r.Runner.Log.Warn("Git maintenance failed; a sync after "+maintenanceRetry.String()+" tries again", "code", diag.Code(err), "causes", diag.Details(err, false))
		} else {
			r.Runner.Log.Info("Git maintenance completed", "elapsed_ms", time.Since(start).Milliseconds())
		}
		if recordErr != nil {
			r.Runner.Log.Warn("Maintenance record could not be saved; the next sync runs maintenance again", "error", recordErr.Error())
		}
	}
	if err != nil {
		return "failed"
	}
	return "completed"
}
