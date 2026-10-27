package relay

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Results arrive in request order while at most the connection limit runs.
func TestRevisionFetchesOrderAndLimit(t *testing.T) {
	var revisions []*Revision
	for i := 0; i < 40; i++ {
		revisions = append(revisions, &Revision{Number: fmt.Sprintf("1.%d", i+1)})
	}
	var running, peak atomic.Int32
	fetches := newAllRevisionFetches(context.Background(), revisions, 4, func(ctx context.Context, rev *Revision) (string, error) {
		now := running.Add(1)
		for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
		}
		time.Sleep(time.Duration(len(rev.Number)%3) * time.Millisecond)
		running.Add(-1)
		return rev.Number, nil
	})
	defer fetches.close()
	for _, rev := range revisions {
		got, err := fetches.next()
		if err != nil || got != rev.Number {
			t.Fatalf("got %q, %v; want %q", got, err, rev.Number)
		}
	}
	if peak.Load() > 4 || peak.Load() < 2 {
		t.Fatalf("peak concurrency %d outside 2..4", peak.Load())
	}
}

// A failed fetch is delivered at its own position and stops further work.
func TestRevisionFetchesError(t *testing.T) {
	revisions := []*Revision{{Number: "1.1"}, {Number: "1.2"}, {Number: "1.3"}}
	failure := errors.New("transport failed")
	fetches := newAllRevisionFetches(context.Background(), revisions, 2, func(ctx context.Context, rev *Revision) (int, error) {
		if rev.Number == "1.2" {
			return 0, failure
		}
		return 1, nil
	})
	defer fetches.close()
	if _, err := fetches.next(); err != nil {
		t.Fatal(err)
	}
	if _, err := fetches.next(); !errors.Is(err, failure) {
		t.Fatalf("error not delivered in order: %v", err)
	}
}
