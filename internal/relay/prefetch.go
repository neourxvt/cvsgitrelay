package relay

import (
	"context"
	"sync"

	"cvsgitrelay/internal/diag"
)

type fetchResult[T any] struct {
	data T
	err  error
}

// revisionFetches overlaps read-only CVS requests without touching Git state.
// Each lane holds at most one result and waits for ordered consumption before
// fetching again, bounding read-ahead even when an early request is slow.
type revisionFetches[T any] struct {
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	lanes   []chan fetchResult[T]
	index   int
}

// Mirror acquisition includes dead revisions because annotation exposes them.
// Results can be any per-revision value, such as several related downloads.
func newAllRevisionFetches[T any](ctx context.Context, live []*Revision, connections int, fetch func(context.Context, *Revision) (T, error)) *revisionFetches[T] {
	ctx, cancel := context.WithCancel(ctx)
	f := &revisionFetches[T]{ctx: ctx, cancel: cancel}
	count := min(connections, len(live))
	f.lanes = make([]chan fetchResult[T], count)
	for lane := range count {
		out := make(chan fetchResult[T])
		f.lanes[lane] = out
		f.workers.Add(1)
		go func() {
			defer f.workers.Done()
			for i := lane; i < len(live); i += count {
				if ctx.Err() != nil {
					return
				}
				data, err := fetch(ctx, live[i])
				select {
				case out <- fetchResult[T]{data: data, err: err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	return f
}

func (f *revisionFetches[T]) next() (T, error) {
	select {
	case result := <-f.lanes[f.index%len(f.lanes)]:
		f.index++
		return result.data, result.err
	case <-f.ctx.Done():
		var zero T
		return zero, diag.New("timeout", "cvs.fetch", "revision fetch interrupted", f.ctx.Err())
	}
}

// Cancel and join before releasing the repository lock, including on errors.
func (f *revisionFetches[T]) close() {
	f.cancel()
	f.workers.Wait()
}
