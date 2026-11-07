package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrQueueTimeout means the request waited longer than queue_timeout_ms for a
// cell and was never granted one.
var ErrQueueTimeout = errors.New("queue timeout")

// Request priorities. Lower wins.
const (
	PriorityMain = iota
	PrioritySubagent
	PriorityAuxiliary
)

type waiter struct {
	key      string
	priority int
	seq      uint64
	ready    chan struct{}
}

// CellScheduler keeps one request in flight per cell key and caps the total
// number of concurrently used cells. Different agent chains get different
// keys, so they no longer collide on a single upstream cell; within a chain,
// requests are serialised, which is what prevents prefill conflicts.
type CellScheduler struct {
	mu            sync.Mutex
	maxConcurrent int
	waiters       []*waiter
	active        int
	cells         map[string]int
	seq           uint64

	queued      atomic.Int64
	queueWaitMS atomic.Int64

	dispatchSignal func()
}

// NewCellScheduler builds a scheduler. A maxConcurrent below 1 is treated as 1.
func NewCellScheduler(maxConcurrent int) *CellScheduler {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &CellScheduler{
		maxConcurrent: maxConcurrent,
		cells:         make(map[string]int),
	}
}

// Acquire blocks until the request may use its cell. The returned release
// function must be called exactly once when the upstream exchange is done.
func (s *CellScheduler) Acquire(ctx context.Context, key string, priority int, timeout time.Duration) (func(), time.Duration, error) {
	start := time.Now()
	w := &waiter{key: key, priority: priority, ready: make(chan struct{})}

	s.mu.Lock()
	s.seq++
	w.seq = s.seq
	s.waiters = append(s.waiters, w)
	s.queued.Add(1)
	s.dispatchLocked()
	s.mu.Unlock()

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case <-w.ready:
		s.queued.Add(-1)
		return s.releaseFunc(key), time.Since(start), nil
	case <-timeoutCh:
		if s.cancelWaiter(w) {
			s.queued.Add(-1)
			return nil, time.Since(start), ErrQueueTimeout
		}
		<-w.ready
		s.queued.Add(-1)
		return s.releaseFunc(key), time.Since(start), nil
	case <-ctx.Done():
		if s.cancelWaiter(w) {
			s.queued.Add(-1)
			return nil, time.Since(start), ctx.Err()
		}
		<-w.ready
		s.queued.Add(-1)
		return s.releaseFunc(key), time.Since(start), nil
	}
}

func (s *CellScheduler) releaseFunc(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.active--
			s.cells[key]--
			if s.cells[key] <= 0 {
				delete(s.cells, key)
			}
			s.dispatchLocked()
			s.mu.Unlock()
		})
	}
}

// cancelWaiter removes w from the queue. It reports false when w was already
// granted a slot, in which case the caller must still release it.
func (s *CellScheduler) cancelWaiter(w *waiter) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.waiters {
		if cur == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return true
		}
	}
	return false
}

func (s *CellScheduler) dispatchLocked() {
	for {
		w := s.bestEligibleLocked()
		if w == nil {
			return
		}
		for i, cur := range s.waiters {
			if cur == w {
				s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
				break
			}
		}
		s.active++
		s.cells[w.key]++
		close(w.ready)
	}
}

// bestEligibleLocked picks the highest-priority, oldest waiter whose cell is
// free while global capacity remains. A cell is used by at most one request.
func (s *CellScheduler) bestEligibleLocked() *waiter {
	if s.active >= s.maxConcurrent {
		return nil
	}
	var best *waiter
	for _, w := range s.waiters {
		if s.cells[w.key] > 0 {
			continue
		}
		if best == nil || w.priority < best.priority || (w.priority == best.priority && w.seq < best.seq) {
			best = w
		}
	}
	return best
}

// ObserveQueueWait records a completed wait for reporting.
func (s *CellScheduler) ObserveQueueWait(d time.Duration) {
	s.queueWaitMS.Add(d.Milliseconds())
}

// Stats reports the number of cells in use and how many requests are queued.
func (s *CellScheduler) Stats() (active, queued int) {
	s.mu.Lock()
	active = s.active
	s.mu.Unlock()
	return active, int(s.queued.Load())
}

// QueueWaitMS is the total time requests spent waiting, for /admin/status.
func (s *CellScheduler) QueueWaitMS() int64 { return s.queueWaitMS.Load() }
