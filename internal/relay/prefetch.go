package relay

import (
	"context"
	"sync"

	"cvsgitrelay/internal/diag"
)

type fetchResult struct {
	data []byte
	err  error
}

// revisionFetches overlaps read-only CVS requests without touching Git state.
// Each lane holds at most one result and waits for ordered consumption before
// fetching again, bounding read-ahead even when an early request is slow.
type revisionFetches struct {
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	lanes   []chan fetchResult
	index   int
}

// Mirror acquisition includes dead revisions because annotation exposes them.
func newAllRevisionFetches(ctx context.Context, live []*Revision, connections int, fetch func(context.Context, *Revision) ([]byte, error)) *revisionFetches {
	ctx, cancel := context.WithCancel(ctx)
	f := &revisionFetches{ctx: ctx, cancel: cancel}
	count := min(connections, len(live))
	f.lanes = make([]chan fetchResult, count)
	for lane := range count {
		out := make(chan fetchResult)
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
				case out <- fetchResult{data: data, err: err}:
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

func (f *revisionFetches) next() ([]byte, error) {
	select {
	case result := <-f.lanes[f.index%len(f.lanes)]:
		f.index++
		return result.data, result.err
	case <-f.ctx.Done():
		return nil, diag.New("timeout", "cvs.fetch", "revision fetch interrupted", f.ctx.Err())
	}
}

// Cancel and join before releasing the repository lock, including on errors.
func (f *revisionFetches) close() {
	f.cancel()
	f.workers.Wait()
}
