package main

import (
	"context"
	"testing"
	"time"
)

func acquire(t *testing.T, s *CellScheduler, key string, prio int, timeout time.Duration) func() {
	t.Helper()
	release, _, err := s.Acquire(context.Background(), key, prio, timeout)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", key, err)
	}
	return release
}

func TestSchedulerSerializesSameKey(t *testing.T) {
	s := NewCellScheduler(4)
	first := acquire(t, s, "cell-a", PriorityMain, time.Second)
	defer first()

	got := make(chan struct{})
	go func() {
		release, _, err := s.Acquire(context.Background(), "cell-a", PriorityMain, time.Second)
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		release()
		close(got)
	}()

	select {
	case <-got:
		t.Fatal("second request entered the same cell while it was busy")
	case <-time.After(30 * time.Millisecond):
	}

	first()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("second request never entered the cell after release")
	}
}

func TestSchedulerGlobalCap(t *testing.T) {
	s := NewCellScheduler(1)
	release := acquire(t, s, "cell-a", PriorityMain, time.Second)
	defer release()

	_, _, err := s.Acquire(context.Background(), "cell-b", PriorityMain, 20*time.Millisecond)
	if err != ErrQueueTimeout {
		t.Fatalf("second cell with cap 1: err = %v, want queue timeout", err)
	}
}

func TestSchedulerPriorityMainFirst(t *testing.T) {
	s := NewCellScheduler(1)
	held := acquire(t, s, "cell-a", PriorityMain, time.Second)

	order := make(chan string, 2)
	go func() {
		rel, _, err := s.Acquire(context.Background(), "cell-b", PriorityAuxiliary, time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		rel()
		order <- "auxiliary"
	}()
	// Let the auxiliary waiter reach the queue before the main one.
	time.Sleep(20 * time.Millisecond)
	go func() {
		rel, _, err := s.Acquire(context.Background(), "cell-c", PriorityMain, time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		rel()
		order <- "main"
	}()
	time.Sleep(20 * time.Millisecond)

	held()

	first := <-order
	second := <-order
	if first != "main" {
		t.Errorf("first granted = %q, want main before auxiliary", first)
	}
	if second != "auxiliary" {
		t.Errorf("second granted = %q, want auxiliary", second)
	}
}

func TestSchedulerReleasesOnContextCancel(t *testing.T) {
	s := NewCellScheduler(1)
	held := acquire(t, s, "cell-a", PriorityMain, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, _, err := s.Acquire(ctx, "cell-b", PriorityMain, time.Second); err == nil {
		t.Fatal("expected the acquire to be cancelled")
	}

	held()
	active, queued := s.Stats()
	if active != 0 || queued != 0 {
		t.Errorf("after release: active=%d queued=%d, want 0/0", active, queued)
	}
	// Capacity must be intact: a new request should get through.
	acquire(t, s, "cell-c", PriorityMain, time.Second)()
}

func TestSchedulerReleaseIsIdempotent(t *testing.T) {
	s := NewCellScheduler(1)
	release := acquire(t, s, "cell-a", PriorityMain, time.Second)
	release()
	release()

	rel := acquire(t, s, "cell-a", PriorityMain, time.Second)
	active, _ := s.Stats()
	if active != 1 {
		t.Errorf("active = %d after one acquire, want 1", active)
	}
	rel()
}

func TestEffectiveKeyRotatesForSubagentAfterWindow(t *testing.T) {
	s := NewCellScheduler(4)
	key := "s:1:a:x"

	if got := s.EffectiveKey(key, PrioritySubagent, false, 50*time.Millisecond); got != key {
		t.Fatalf("healthy cell rotated: %q", got)
	}
	s.ReportFailure(key)
	if got := s.EffectiveKey(key, PrioritySubagent, false, 50*time.Millisecond); got != key {
		t.Fatalf("rotated before the fallback window elapsed: %q", got)
	}
	time.Sleep(70 * time.Millisecond)
	got := s.EffectiveKey(key, PrioritySubagent, false, 50*time.Millisecond)
	if got == key {
		t.Fatal("cell that kept failing was never rotated")
	}
	if len(got) <= len(key) || got[:len(key)] != key {
		t.Fatalf("rotated key %q does not extend %q", got, key)
	}
}

func TestEffectiveKeyLeavesMainAloneByDefault(t *testing.T) {
	s := NewCellScheduler(4)
	key := "s:1"
	s.ReportFailure(key)
	time.Sleep(20 * time.Millisecond)

	if got := s.EffectiveKey(key, PriorityMain, false, 10*time.Millisecond); got != key {
		t.Fatalf("main cell rotated without permission: %q", got)
	}
	if got := s.EffectiveKey(key, PriorityMain, true, 10*time.Millisecond); got == key {
		t.Fatalf("main cell did not rotate when allowed: %q", got)
	}
}

func TestReportSuccessClearsRotationState(t *testing.T) {
	s := NewCellScheduler(4)
	key := "s:1:a:y"
	s.ReportFailure(key)
	time.Sleep(20 * time.Millisecond)
	s.ReportSuccess(key)
	if got := s.EffectiveKey(key, PrioritySubagent, false, 10*time.Millisecond); got != key {
		t.Fatalf("rotated after a success: %q", got)
	}
}
