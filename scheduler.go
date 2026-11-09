package main

import (
	"context"
	"errors"
	"strconv"
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

const (
	// minCellLimit is the floor the adaptive ceiling never goes below: one or
	// two streams can always run, even while the gateway is refusing prefills.
	minCellLimit = 2
	// cellRecoveryInterval is how long the gateway must stop refusing prefills
	// before one cell is added back to the ceiling.
	cellRecoveryInterval = 2 * time.Minute
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
	// softLimit is the ceiling actually enforced. It starts at maxConcurrent
	// and steps down when the gateway refuses a prefill, then recovers one
	// slot per cellRecoveryInterval while requests succeed.
	softLimit int
	lastStep  time.Time
	now       func() time.Time

	waiters []*waiter
	active  int
	cells   map[string]int
	seq     uint64

	queued      atomic.Int64
	queueWaitMS atomic.Int64

	// health tracks how long a cell has been refusing, so a stuck chain can be
	// moved to a fresh cell instead of waiting forever.
	health map[string]*cellHealth
}

type cellHealth struct {
	firstFailure time.Time
	rotation     int
}

// ReportFailure records that a cell answered with a retryable status.
func (s *CellScheduler) ReportFailure(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.health[key]
	if h == nil {
		h = &cellHealth{firstFailure: time.Now()}
		s.health[key] = h
	}
}

// ResetCell drops a cell's failure history and rotation. Compaction rewrites
// the prompt prefix, so a rotated key's cached prefill is stale; the chain
// starts over from the base key with a fresh cache.
func (s *CellScheduler) ResetCell(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.health, key)
}

// ReportSuccess clears a cell's failure history and, after a quiet spell,
// gives the ceiling one slot back.
func (s *CellScheduler) ReportSuccess(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.health, key)
	s.recoverLocked()
}

// recoverLocked raises the ceiling by one when nothing has been refused for
// cellRecoveryInterval. Recovery needs a success, so a busy gateway is never
// probed back up while it is struggling.
func (s *CellScheduler) recoverLocked() {
	floor := minCellLimit
	if s.maxConcurrent < floor {
		floor = s.maxConcurrent
	}
	if s.softLimit >= s.maxConcurrent || s.now().Sub(s.lastStep) < cellRecoveryInterval {
		return
	}
	s.softLimit++
	s.lastStep = s.now()
	s.dispatchLocked()
}

// NotePrefillRefusal lowers the ceiling by one because the gateway refused a
// prefill, which is its signal that too many cells are being prefilled. It
// exists because the Go docs publish no concurrency number: the limit is
// learned from the gateway instead of guessed.
func (s *CellScheduler) NotePrefillRefusal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	floor := minCellLimit
	if s.maxConcurrent < floor {
		floor = s.maxConcurrent
	}
	if s.softLimit > floor {
		s.softLimit--
	}
	s.lastStep = s.now()
}

// EffectiveKey returns the key a request should use. When the cell has been
// refusing for longer than fallbackAfter, a rotating suffix is added so the
// chain gets a different cell. The main chain only rotates when allowMain is
// set, because a new cell means a cold prompt cache.
func (s *CellScheduler) EffectiveKey(key string, priority int, allowMain bool, fallbackAfter time.Duration) string {
	if fallbackAfter <= 0 {
		return key
	}
	if priority == PriorityMain && !allowMain {
		return key
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.health[key]
	if h == nil || s.now().Sub(h.firstFailure) < fallbackAfter {
		return key
	}
	h.rotation++
	h.firstFailure = time.Now()
	return key + ":r" + strconv.Itoa(h.rotation+1)
}

// NewCellScheduler builds a scheduler. A maxConcurrent below 1 is treated as 1.
func NewCellScheduler(maxConcurrent int) *CellScheduler {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &CellScheduler{
		maxConcurrent: maxConcurrent,
		softLimit:     maxConcurrent,
		now:           time.Now,
		cells:         make(map[string]int),
		health:        make(map[string]*cellHealth),
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
	if s.active >= s.softLimit {
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

// Limit is the ceiling currently enforced, which may be below maxConcurrent
// after the gateway refused a prefill, for /admin/status.
func (s *CellScheduler) Limit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.softLimit
}

// QueueWaitMS is the total time requests spent waiting, for /admin/status.
func (s *CellScheduler) QueueWaitMS() int64 { return s.queueWaitMS.Load() }
