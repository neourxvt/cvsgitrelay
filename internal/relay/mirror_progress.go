package relay

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Snapshot progress independently of CVS completion so slow requests and large
// files remain visible. Only copied log fields cross the goroutine boundary.
type mirrorProgress struct {
	mu      sync.Mutex
	log     *slog.Logger
	phase   string
	started time.Time
	fields  map[string]any
	stop    chan struct{}
	done    chan struct{}
}

func newMirrorProgress(ctx context.Context, log *slog.Logger, interval time.Duration) *mirrorProgress {
	p := &mirrorProgress{log: log, fields: map[string]any{}, stop: make(chan struct{}), done: make(chan struct{})}
	if interval <= 0 {
		close(p.done)
		return p
	}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.report()
			}
		}
	}()
	return p
}

func (p *mirrorProgress) close() {
	close(p.stop)
	<-p.done
}

// Phase changes are logged immediately even when periodic progress is disabled.
// Repeated stage calls update details without restarting the phase clock.
func (p *mirrorProgress) stage(message string, fields ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase != message {
		p.phase, p.started = message, time.Now()
	}
	p.fields = map[string]any{}
	for i := 0; i+1 < len(fields); i += 2 {
		p.fields[fields[i].(string)] = fields[i+1]
	}
	p.log.Info(message, fields...)
}

func (p *mirrorProgress) update(fields ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i+1 < len(fields); i += 2 {
		p.fields[fields[i].(string)] = fields[i+1]
	}
}

func (p *mirrorProgress) report() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase == "" {
		return
	}
	fields := []any{"phase", p.phase, "elapsed", time.Since(p.started).Round(time.Second)}
	keys := make([]string, 0, len(p.fields))
	for key := range p.fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		fields = append(fields, key, p.fields[key])
	}
	p.log.Info("Mirror progress", fields...)
}
