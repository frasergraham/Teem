// Package inflight is a per-team durability log for jobs the spawner
// has handed to a worker but hasn't seen complete. The daemon
// consults it at shutdown (to emit interrupted audit events for
// anything still running) and at startup (to reconcile crashes —
// orphaned "start" records get marked interrupted before the next
// chat opens).
//
// Storage is an append-only JSONL log with op=start/end records. The
// "current set of in-flight jobs" is computed by replaying the file
// — same model as audit and plan. After a clean restart-reconcile we
// truncate the file so it doesn't grow unbounded across long-running
// installs.
package inflight

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is a materialised in-flight job — the result of replaying
// start events that have no matching end.
type Record struct {
	JobID         string    `json:"job_id"`
	AgentID       string    `json:"agent_id"`
	PromptPreview string    `json:"prompt_preview"`
	StartedAt     time.Time `json:"started_at"`
}

// Event is one append to the JSONL file. Op is "start" or "end".
type Event struct {
	Op            string    `json:"op"`
	JobID         string    `json:"job_id"`
	AgentID       string    `json:"agent_id,omitempty"`
	PromptPreview string    `json:"prompt_preview,omitempty"`
	Timestamp     time.Time `json:"ts"`
}

// Log is the appender + replayer. Safe for concurrent use; every
// write takes the mutex.
type Log struct {
	path string

	mu sync.Mutex
	f  *os.File
}

// Open creates/opens the in-flight log at path. Replay-on-startup is
// done by callers via Outstanding.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("inflight: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("inflight: open: %w", err)
	}
	return &Log{path: path, f: f}, nil
}

// RecordStart writes a "start" event. promptPreview is typically the
// first ~200 bytes of the prompt — enough that an interrupted-job
// audit event surfaces what the worker was about to do.
func (l *Log) RecordStart(jobID, agentID, promptPreview string) error {
	return l.write(Event{
		Op:            "start",
		JobID:         jobID,
		AgentID:       agentID,
		PromptPreview: promptPreview,
		Timestamp:     time.Now().UTC(),
	})
}

// RecordEnd writes an "end" event closing out a previously-started
// job. End records intentionally omit the agent/preview — the start
// already has them, and the end's purpose is just "no longer
// in-flight."
func (l *Log) RecordEnd(jobID string) error {
	return l.write(Event{
		Op:        "end",
		JobID:     jobID,
		Timestamp: time.Now().UTC(),
	})
}

// Outstanding replays the file and returns every job whose start has
// no matching end. Used at daemon startup to find what was interrupted
// by the last shutdown/crash.
func (l *Log) Outstanding() ([]Record, error) {
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	open := map[string]Record{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Op {
		case "start":
			if _, dup := open[ev.JobID]; dup {
				continue
			}
			open[ev.JobID] = Record{
				JobID:         ev.JobID,
				AgentID:       ev.AgentID,
				PromptPreview: ev.PromptPreview,
				StartedAt:     ev.Timestamp,
			}
		case "end":
			delete(open, ev.JobID)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(open))
	for _, r := range open {
		out = append(out, r)
	}
	return out, nil
}

// Reset truncates the log so the next start begins from a clean
// slate. Callers should do this after the reconcile pass at daemon
// start, since the records have been surfaced as audit events and
// holding them in the in-flight log would re-report them on the next
// restart.
func (l *Log) Reset() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		if err := l.f.Close(); err != nil {
			return err
		}
		l.f = nil
	}
	if err := os.Truncate(l.path, 0); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

// Close releases the file handle.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// ErrClosed is returned by RecordStart/RecordEnd after Close.
var ErrClosed = errors.New("inflight: closed")

func (l *Log) write(ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return ErrClosed
	}
	if _, err := l.f.Write(body); err != nil {
		return err
	}
	return nil
}
