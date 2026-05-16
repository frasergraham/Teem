package main

import (
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// hookedSink wraps an audit.Sink and fans every successful Write through
// an auditHook. This is the single source of truth so that leader-side
// callers (MCP record_decision/record_blocker, chat-handler usage
// events, dashboard form posts) hit the same hook chain as worker
// audit POSTs. Without it, those direct Writes silently bypass the
// messaging/channel/archmem/pulse-nudge/usage hooks.
//
// The hook is wired in lazily because it depends on objects (the MCP
// server, the spawner, pulse) that need the wrapped sink to be
// constructed first. Writes that fire before SetHook see a nil hook
// and complete silently.
type hookedSink struct {
	inner audit.Sink

	mu   sync.RWMutex
	hook auditHook
}

func newHookedSink(inner audit.Sink) *hookedSink { return &hookedSink{inner: inner} }

// SetHook replaces the current hook. Safe for concurrent use; in
// practice it is only called once per team during buildTeamServices.
func (s *hookedSink) SetHook(h auditHook) {
	s.mu.Lock()
	s.hook = h
	s.mu.Unlock()
}

func (s *hookedSink) Write(e audit.Event) error {
	if err := s.inner.Write(e); err != nil {
		return err
	}
	s.mu.RLock()
	h := s.hook
	s.mu.RUnlock()
	if h != nil {
		h([]audit.Event{e})
	}
	return nil
}

func (s *hookedSink) Query(agentID string, since time.Time, limit int) ([]audit.Event, error) {
	return s.inner.Query(agentID, since, limit)
}

func (s *hookedSink) Close() error { return s.inner.Close() }
