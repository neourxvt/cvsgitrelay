package relay

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// phaseTimer logs each phase of a sync or verify when it starts, reports the
// running phase periodically, and collects how long every phase took so slow
// steps are visible after the run. A nil timer does nothing, which keeps
// direct calls from tests and unlogged runners free of checks.
type phaseTimer struct {
	progress *mirrorProgress
	key      string
	started  time.Time
	keys     []string
	totals   map[string]time.Duration
}

func newPhaseTimer(ctx context.Context, log *slog.Logger, interval time.Duration, title string) *phaseTimer {
	if log == nil {
		return nil
	}
	return &phaseTimer{progress: newProgress(ctx, log, interval, title), totals: map[string]time.Duration{}}
}

// begin ends the running phase and starts the one named by key. message and
// fields are logged immediately. Repeated keys add up into one total.
func (t *phaseTimer) begin(key, message string, fields ...any) {
	if t == nil {
		return
	}
	t.end()
	t.key, t.started = key, time.Now()
	t.progress.stage(message, fields...)
}

func (t *phaseTimer) end() {
	if t.key == "" {
		return
	}
	if !slices.Contains(t.keys, t.key) {
		t.keys = append(t.keys, t.key)
	}
	t.totals[t.key] += time.Since(t.started)
	t.key = ""
}

// finish stops periodic progress and logs every phase duration in the order
// the phases first ran. It also runs after failures, up to the failed phase.
func (t *phaseTimer) finish(message string) {
	if t == nil {
		return
	}
	t.end()
	t.progress.close()
	fields := make([]any, 0, 2*len(t.keys))
	for _, key := range t.keys {
		fields = append(fields, key, t.totals[key].Round(time.Millisecond))
	}
	t.progress.log.Info(message, fields...)
}
