// pkg/rag/search_batcher.go — HNSW query batching coalescer. [367.C]
// Amortizes per-query overhead (LockOSThread, SIMD dispatch init, pool fetch)
// for burst workloads by draining a channel with a 2ms window (configurable).
package rag

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/tensorx"
)

// searchReq is a single batched HNSW query ticket.
type searchReq struct {
	ctx   context.Context
	query []float32
	k     int
	resp  chan<- searchResp
}

// searchResp carries the result back to the caller.
type searchResp struct {
	ids []uint32
	err error
}

// QueryBatcher coalesces concurrent Graph.Search calls into timed micro-batches.
// The dispatcher goroutine is pinned (LockOSThread) for the duration, amortizing
// the OS-thread lock and SIMD dispatch across all queries in the window.
//
// Enable via Graph.EnableBatcher; disable the global affinity pin in Graph.Search
// when the batcher is active (batcher goroutine owns the pin).
type QueryBatcher struct {
	graph    *Graph
	cpu      tensorx.ComputeDevice
	queue    chan *searchReq
	window   time.Duration
	maxSize  int
	quit     chan struct{}
	metricN  atomic.Uint64 // total requests processed (denominator for avg batch size)
	metricBs atomic.Uint64 // sum of batch sizes (numerator for avg batch size)
}

// newQueryBatcher allocates a batcher and starts its background dispatcher goroutine.
func newQueryBatcher(g *Graph, cpu tensorx.ComputeDevice, windowMS, maxSize int) *QueryBatcher {
	if windowMS <= 0 {
		windowMS = 2
	}
	if maxSize <= 0 {
		maxSize = 32
	}
	b := &QueryBatcher{
		graph:   g,
		cpu:     cpu,
		queue:   make(chan *searchReq, maxSize*2),
		window:  time.Duration(windowMS) * time.Millisecond,
		maxSize: maxSize,
		quit:    make(chan struct{}),
	}
	go b.processLoop()
	return b
}

// processLoop is the singleton dispatcher goroutine. It pins itself to an OS
// thread for the full duration (amortizing LockOSThread across all batches),
// drains the queue on a timer or when maxSize is reached, and executes each
// query in the batch sequentially on the same warm-cache goroutine.
func (b *QueryBatcher) processLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	timer := time.NewTimer(b.window)
	defer timer.Stop()

	batch := make([]*searchReq, 0, b.maxSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		b.metricBs.Add(uint64(len(batch)))
		b.metricN.Add(uint64(len(batch)))
		for _, req := range batch {
			// Call searchCore directly (bypasses affinity-pin — batcher is already pinned).
			ids, err := b.graph.searchCore(req.ctx, req.query, req.k, b.cpu)
			select {
			case req.resp <- searchResp{ids: ids, err: err}:
			default:
				// Caller context expired — discard response (chan is already abandoned).
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case req := <-b.queue:
			batch = append(batch, req)
			if len(batch) >= b.maxSize {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				flush()
				timer.Reset(b.window)
			}
		case <-timer.C:
			flush()
			timer.Reset(b.window)
		case <-b.quit:
			// Drain any remaining requests before exiting.
			for {
				select {
				case req := <-b.queue:
					batch = append(batch, req)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Submit enqueues a search request and blocks until the dispatcher completes it.
// Returns ctx.Err() if the context expires before the result arrives.
func (b *QueryBatcher) Submit(ctx context.Context, query []float32, k int) ([]uint32, error) {
	resp := make(chan searchResp, 1)
	req := &searchReq{ctx: ctx, query: query, k: k, resp: resp}
	select {
	case b.queue <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-resp:
		return r.ids, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Stop signals the dispatcher to finish pending work and exit.
func (b *QueryBatcher) Stop() {
	close(b.quit)
}

// AvgBatchSize returns the running average number of queries coalesced per flush.
// Used for the neo_hnsw_batch_size_avg metric exported via HUD_STATE.
func (b *QueryBatcher) AvgBatchSize() float64 {
	n := b.metricN.Load()
	if n == 0 {
		return 0
	}
	return float64(b.metricBs.Load()) / float64(n)
}
