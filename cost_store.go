package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// costFile is the on-disk shape of the session cost store.
type costFile struct {
	Version  int                     `json:"version"`
	Sessions map[string]*SessionCost `json:"sessions"`
}

const costFileVersion = 1

// CostStore accumulates per-session cost and, when given a path, survives a
// restart. An empty path keeps it in memory only.
//
// There is deliberately no TTL: a session left overnight must still show its
// cost in the morning. Memory is bounded by evicting the least recently
// active sessions once the count exceeds maxSessions.
type CostStore struct {
	path string

	mu       sync.Mutex
	sessions map[string]*SessionCost
	dirty    bool
}

// NewCostStore loads path when it exists. A missing file is not an error.
func NewCostStore(path string) (*CostStore, error) {
	s := &CostStore{path: path, sessions: make(map[string]*SessionCost)}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cost: read %s: %w", path, err)
	}
	var f costFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("cost: parse %s: %w", path, err)
	}
	for id, sc := range f.Sessions {
		if sc == nil {
			continue
		}
		sc.Session = id
		if sc.ByModel == nil {
			sc.ByModel = make(map[string]ModelCost)
		}
		s.sessions[id] = sc
	}
	return s, nil
}

// Add records one successful response against a session.
func (s *CostStore) Add(session, model string, usd, listUSD float64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sc := s.sessions[session]
	if sc == nil {
		sc = &SessionCost{Session: session, FirstAt: at, ByModel: make(map[string]ModelCost)}
		s.sessions[session] = sc
	}
	sc.USD += usd
	sc.ListUSD += listUSD
	sc.Requests++
	sc.LastAt = at

	if model != "" {
		mc := sc.ByModel[model]
		mc.USD += usd
		mc.ListUSD += listUSD
		mc.Requests++
		sc.ByModel[model] = mc
	}

	for len(s.sessions) > maxSessions {
		oldestID := ""
		var oldest time.Time
		for id, v := range s.sessions {
			if oldestID == "" || v.LastAt.Before(oldest) {
				oldestID, oldest = id, v.LastAt
			}
		}
		delete(s.sessions, oldestID)
	}
	s.dirty = true
}

// Get returns a copy of one session's record. An unknown session yields a
// zero record rather than an error.
func (s *CostStore) Get(session string) SessionCost {
	s.mu.Lock()
	defer s.mu.Unlock()

	sc, ok := s.sessions[session]
	if !ok {
		return SessionCost{Session: session, ByModel: map[string]ModelCost{}}
	}
	out := *sc
	out.ByModel = make(map[string]ModelCost, len(sc.ByModel))
	for model, mc := range sc.ByModel {
		out.ByModel[model] = mc
	}
	return out
}

// Flush writes the store atomically. It is a no-op for an in-memory store and
// while nothing has changed since the last successful write.
func (s *CostStore) Flush() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snapshot := costFile{Version: costFileVersion, Sessions: make(map[string]*SessionCost, len(s.sessions))}
	for id, sc := range s.sessions {
		cp := *sc
		cp.ByModel = make(map[string]ModelCost, len(sc.ByModel))
		for model, mc := range sc.ByModel {
			cp.ByModel[model] = mc
		}
		snapshot.Sessions[id] = &cp
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("cost: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("cost: write %s: %w", s.path, err)
	}

	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// StartFlusher writes the store every interval while it has changed, and one
// last time when ctx is cancelled. The returned channel closes once that last
// write is finished, so a caller on its way out can wait for it instead of
// exiting mid-write and leaving a half-written temp file behind.
func (s *CostStore) StartFlusher(ctx context.Context, every time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = s.Flush()
				return
			case <-ticker.C:
				_ = s.Flush()
			}
		}
	}()
	return done
}
