package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// outbox is the worker-side audit event channel with disk backing and
// offline tolerance. Each call to Emit appends an event to a JSONL file
// and signals the sender goroutine. The sender batches unsent events and
// POSTs them to the leader's /audit endpoint; on failure it backs off
// exponentially and retries — events stay on disk until the leader is
// reachable again.
//
// Storage layout under dir:
//
//	outbox.jsonl   append-only event log
//	outbox.cursor  byte offset of the next-to-send event
//
// We never rotate or compact in v1. The events are small; we can revisit
// when the file size becomes a problem in practice.
type outbox struct {
	dir       string
	leaderURL string
	token     string
	client    *http.Client
	agentID   string

	mu       sync.Mutex
	f        *os.File
	cursor   int64
	notifyCh chan struct{}
}

// newOutbox opens (creating if needed) the outbox files under dir and
// returns a ready-to-use outbox. Start() launches the sender goroutine.
func newOutbox(dir, leaderURL, token, agentID string, client *http.Client) (*outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("outbox: mkdir: %w", err)
	}
	path := filepath.Join(dir, "outbox.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("outbox: open: %w", err)
	}
	cursor, err := readCursor(filepath.Join(dir, "outbox.cursor"))
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &outbox{
		dir:       dir,
		leaderURL: leaderURL,
		token:     token,
		client:    client,
		agentID:   agentID,
		f:         f,
		cursor:    cursor,
		notifyCh:  make(chan struct{}, 1),
	}, nil
}

// Emit appends an event. AgentID is filled in if missing.
func (o *outbox) Emit(e audit.Event) error {
	if e.AgentID == "" {
		e.AgentID = o.agentID
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	o.mu.Lock()
	_, err = o.f.Write(body)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	o.notify()
	return nil
}

func (o *outbox) notify() {
	select {
	case o.notifyCh <- struct{}{}:
	default:
	}
}

// Start runs the sender goroutine until ctx is cancelled. It is safe to
// call once per outbox.
func (o *outbox) Start(ctx context.Context) {
	if o.leaderURL == "" {
		// No leader configured — outbox is write-only on disk. Useful for
		// --no-tailnet smoke tests when no leader is reachable.
		return
	}
	go o.run(ctx)
	// Kick once so any pre-existing on-disk events drain on startup.
	o.notify()
}

func (o *outbox) run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 5 * time.Minute
	for {
		// Wait for a signal or a periodic retry tick (caps backoff).
		select {
		case <-ctx.Done():
			return
		case <-o.notifyCh:
		case <-time.After(backoff):
		}
		err := o.drain(ctx)
		if err == nil {
			backoff = time.Second
			continue
		}
		// Soft failure: leader unreachable. Don't log every retry; the
		// event remains queued and we'll try again.
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// drain reads from cursor to EOF, batches the events, and POSTs them.
// On success it advances the cursor. If there are no unsent events it
// returns nil immediately.
func (o *outbox) drain(ctx context.Context) error {
	o.mu.Lock()
	cursor := o.cursor
	o.mu.Unlock()

	path := filepath.Join(o.dir, "outbox.jsonl")
	rf, err := os.Open(path)
	if err != nil {
		return err
	}
	defer rf.Close()
	stat, err := rf.Stat()
	if err != nil {
		return err
	}
	if stat.Size() <= cursor {
		return nil
	}
	if _, err := rf.Seek(cursor, io.SeekStart); err != nil {
		return err
	}

	var events []audit.Event
	sc := bufio.NewScanner(rf)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e audit.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		events = append(events, e)
		if len(events) >= 256 {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}

	body, err := json.Marshal(events)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.leaderURL+"/audit", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("audit POST: %s", resp.Status)
	}

	// Advance the cursor by the bytes we just read.
	consumed, _ := rf.Seek(0, io.SeekCurrent)
	o.mu.Lock()
	o.cursor = consumed
	o.mu.Unlock()
	if err := writeCursor(filepath.Join(o.dir, "outbox.cursor"), consumed); err != nil {
		return err
	}

	// If there's still more to send, kick again.
	if consumed < stat.Size() {
		o.notify()
	}
	return nil
}

func (o *outbox) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.f.Close()
}

func readCursor(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.ParseInt(string(bytes.TrimSpace(b)), 10, 64)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func writeCursor(path string, n int64) error {
	// Write then rename for atomicity — a torn cursor file would replay
	// or drop events at restart.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
