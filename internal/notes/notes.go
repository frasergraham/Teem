// Package notes is the leader's "messages for the user" inbox. Pulse
// ticks can call write_user_note to leave something the human should
// see when they're next at the chat. `teem chat` reads unread notes
// at launch and prints them as a banner before exec'ing Claude Code.
//
// Storage is append-only JSONL — same shape as audit and plan — with
// a sidecar cursor file recording the last-shown timestamp.
package notes

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Note is one message from the leader to the user.
type Note struct {
	Timestamp time.Time `json:"ts"`
	Text      string    `json:"text"`
	// Source records who wrote the note (always "leader" today; left
	// open so worker-level annotations could land here later).
	Source string `json:"source,omitempty"`
}

// Inbox is a per-team notes log.
type Inbox struct {
	path   string
	cursor string // sidecar cursor file path

	mu sync.Mutex
	f  *os.File
}

// Open opens (creating if needed) the inbox at path. The cursor file
// sits next to it.
func Open(path string) (*Inbox, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("notes: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("notes: open: %w", err)
	}
	return &Inbox{path: path, cursor: path + ".cursor", f: f}, nil
}

// Write appends a note. Timestamp defaults to time.Now() if unset.
func (i *Inbox) Write(n Note) error {
	if n.Text == "" {
		return errors.New("notes: text is required")
	}
	if n.Timestamp.IsZero() {
		n.Timestamp = time.Now().UTC()
	}
	if n.Source == "" {
		n.Source = "leader"
	}
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	i.mu.Lock()
	defer i.mu.Unlock()
	_, err = i.f.Write(body)
	return err
}

// Unread returns notes whose timestamps are after the recorded
// cursor. A nil/zero cursor means "everything".
func (i *Inbox) Unread() ([]Note, error) {
	cursor, _ := i.readCursor()
	return i.read(cursor)
}

// MarkAllRead advances the cursor past the latest note in the log.
// Idempotent.
func (i *Inbox) MarkAllRead() error {
	all, err := i.read(time.Time{})
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	latest := all[len(all)-1].Timestamp
	return i.writeCursor(latest)
}

// Close closes the underlying file.
func (i *Inbox) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.f == nil {
		return nil
	}
	err := i.f.Close()
	i.f = nil
	return err
}

func (i *Inbox) read(since time.Time) ([]Note, error) {
	f, err := os.Open(i.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var out []Note
	for sc.Scan() {
		var n Note
		if err := json.Unmarshal(sc.Bytes(), &n); err != nil {
			continue
		}
		if !since.IsZero() && !n.Timestamp.After(since) {
			continue
		}
		out = append(out, n)
	}
	return out, sc.Err()
}

func (i *Inbox) readCursor() (time.Time, error) {
	body, err := os.ReadFile(i.cursor)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(body)))
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

func (i *Inbox) writeCursor(t time.Time) error {
	tmp := i.cursor + ".tmp"
	if err := os.WriteFile(tmp, []byte(t.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, i.cursor)
}
