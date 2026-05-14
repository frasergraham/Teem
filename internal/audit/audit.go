// Package audit is the worker→leader event channel.
//
// Workers emit structured Events for job lifecycle (job_received,
// job_complete, job_error) and for free-form notes during a run; the
// leader receives them over HTTP /audit and writes them to a JSONL Sink.
//
// The model:
//
//	worker  ── POST /audit ───►  leader.Sink (JSONL file)
//	  │                              │
//	  └─ buffers on disk while       └─ readable by:
//	      leader is unreachable          - `teem audit` (operator)
//	                                     - MCP query_audit (the Leader Claude)
//
// Storage in v1 is JSONL on disk — greppable, tailable, and trivially
// backed up. The Sink interface leaves room for a SQLite implementation
// later without touching callers.
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Kind enumerates the canonical event kinds. The set is open — the kind
// field is a string and callers may emit custom kinds; constants here
// cover the cases Teem itself produces.
type Kind string

const (
	KindJobReceived Kind = "job_received"
	KindJobComplete Kind = "job_complete"
	KindJobError    Kind = "job_error"
	KindNote        Kind = "note"
	// KindHeartbeat is emitted on a fixed interval by workers so the
	// leader can prove liveness (worker is alive and idle) and so the
	// registry can compute a LastSeen per agent. Meta carries in_flight
	// and uptime_s.
	KindHeartbeat Kind = "heartbeat"
	// KindJobInterrupted is emitted by the daemon at startup for jobs
	// that were in flight when the previous run shut down (graceful
	// drain expired or crash). Distinguishes "job failed inside
	// claude" from "the daemon killed it mid-execution." Meta carries
	// prompt_preview + started_at.
	KindJobInterrupted Kind = "job_interrupted"
	// KindJobTranscriptReady is emitted by the worker after it has
	// pushed a job's full stream-json transcript to the leader. Meta
	// carries {path, bytes, event_count, summary}.
	KindJobTranscriptReady Kind = "job_transcript_ready"
	// KindWorkerStopped is emitted when a worker subprocess terminates
	// (clean shutdown or otherwise). Used as a leader-wake signal.
	KindWorkerStopped Kind = "worker_stopped"
)

// Event is one entry on the audit channel. Meta is a free-form bag for
// kind-specific extra fields (output size, file paths touched, etc.).
type Event struct {
	Timestamp time.Time      `json:"ts"`
	AgentID   string         `json:"agent_id"`
	JobID     string         `json:"job_id,omitempty"`
	Kind      Kind           `json:"kind"`
	Message   string         `json:"message,omitempty"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// Sink is the leader-side storage abstraction. Implementations must be
// safe for concurrent Write calls (the leader fans audit posts in from
// many workers).
type Sink interface {
	Write(e Event) error
	// Query returns up to limit events, optionally filtered by agentID
	// and to events at or after since. Implementations may scan from
	// the end of the log for newest-first results.
	Query(agentID string, since time.Time, limit int) ([]Event, error)
	io.Closer
}

// FileSink appends JSON-encoded Events to a file, one per line.
// Concurrent Writes are serialized through an internal mutex; reads
// happen on a fresh file handle so callers can tail while writes
// continue.
type FileSink struct {
	path string

	mu sync.Mutex
	f  *os.File
}

// OpenFile opens (creating if needed) the JSONL file at path. The parent
// directory is created with 0o700 since audit logs may contain sensitive
// command output.
func OpenFile(path string) (*FileSink, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open: %w", err)
	}
	return &FileSink{path: path, f: f}, nil
}

// Path returns the on-disk path the FileSink is writing to.
func (s *FileSink) Path() string { return s.path }

func (s *FileSink) Write(e Event) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	body = append(body, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(body); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}

// Query reads the file from the start and returns events matching the
// filter. It's intentionally simple — JSONL is not a database. For v1
// scans are O(file); we can revisit when log files grow huge.
func (s *FileSink) Query(agentID string, since time.Time, limit int) ([]Event, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: open for read: %w", err)
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if agentID != "" && e.AgentID != agentID {
			continue
		}
		if !since.IsZero() && e.Timestamp.Before(since) {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
