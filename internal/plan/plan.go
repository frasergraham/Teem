// Package plan persists the leader's task list — its canonical view of
// "what we're trying to do." Survives daemon restart and chat sessions
// so an autonomous leader (or a returning human) can see open work and
// recent progress without re-deriving it from audit history.
//
// Storage is an append-only JSONL file of mutation events
// (~/.teem/state/<team>/plan.jsonl) and a snapshot rebuilt by replaying
// events at open time. Same shape as the audit log; events are the
// source of truth, the in-memory snapshot is the fast read.
package plan

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Status is the lifecycle stage of a task. Open-ended in the sense
// that the leader can use intermediate states meaningfully, but the
// daemon special-cases pending/in_progress/blocked when answering
// "is there anything to do?" questions.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusDone       Status = "done"
	StatusAbandoned  Status = "abandoned"
)

// IsOpen reports whether the task still needs leader attention.
func (s Status) IsOpen() bool {
	return s == StatusPending || s == StatusInProgress || s == StatusBlocked
}

// Task is the materialised view of a task assembled by replaying
// events. The leader sees this shape via list_tasks; the daemon uses
// it to decide whether to keep ticking.
type Task struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Status     Status    `json:"status"`
	AssignedTo string    `json:"assigned_to,omitempty"`
	ParentID   string    `json:"parent_id,omitempty"`
	DependsOn  []string  `json:"depends_on,omitempty"`
	Notes      string    `json:"notes,omitempty"`
	Evidence   []string  `json:"evidence,omitempty"` // job_ids
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Event is one mutation written to the JSONL file. Op is either
// "create" or "update"; update events carry only the fields they
// change. AddEvidence is additive; everything else is replace-or-keep.
type Event struct {
	Op         string    `json:"op"`
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"ts"`
	Title      string    `json:"title,omitempty"`
	ParentID   string    `json:"parent_id,omitempty"`
	Status     Status    `json:"status,omitempty"`
	AssignedTo *string   `json:"assigned_to,omitempty"`
	DependsOn  *[]string `json:"depends_on,omitempty"`
	Notes      *string   `json:"notes,omitempty"`
	AddEvidence []string `json:"add_evidence,omitempty"`
}

// Plan is the live snapshot + appender. Safe for concurrent calls;
// every write takes the mutex.
type Plan struct {
	path string

	mu    sync.Mutex
	f     *os.File
	tasks map[string]*Task
	order []string // insertion order for stable listing
}

// ErrTaskNotFound is returned by UpdateTask when no task has the
// supplied id. Distinct from a generic error so callers can react.
var ErrTaskNotFound = errors.New("plan: task not found")

// Open opens (creating if needed) the JSONL file at path. Replays
// every event into the snapshot so callers can immediately list/get.
func Open(path string) (*Plan, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("plan: mkdir: %w", err)
	}
	p := &Plan{path: path, tasks: map[string]*Task{}}
	if err := p.replay(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("plan: open append: %w", err)
	}
	p.f = f
	return p, nil
}

// replay scans the file from the start and folds every event into the
// in-memory snapshot. Lines that fail to parse are skipped (forward
// compat) — we never reject the whole file because of one bad line.
func (p *Plan) replay() error {
	f, err := os.Open(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("plan: open for read: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		p.apply(ev)
	}
	return sc.Err()
}

// apply folds an event into the snapshot. Caller must hold p.mu (or
// be initialising before exposing the *Plan).
func (p *Plan) apply(ev Event) {
	switch ev.Op {
	case "create":
		if _, exists := p.tasks[ev.ID]; exists {
			return
		}
		t := &Task{
			ID:        ev.ID,
			Title:     ev.Title,
			Status:    StatusPending,
			ParentID:  ev.ParentID,
			CreatedAt: ev.Timestamp,
			UpdatedAt: ev.Timestamp,
		}
		if ev.Status != "" {
			t.Status = ev.Status
		}
		if ev.DependsOn != nil {
			t.DependsOn = append([]string(nil), (*ev.DependsOn)...)
		}
		if ev.Notes != nil {
			t.Notes = *ev.Notes
		}
		if ev.AssignedTo != nil {
			t.AssignedTo = *ev.AssignedTo
		}
		p.tasks[ev.ID] = t
		p.order = append(p.order, ev.ID)
	case "update":
		t, ok := p.tasks[ev.ID]
		if !ok {
			return
		}
		if ev.Status != "" {
			t.Status = ev.Status
		}
		if ev.AssignedTo != nil {
			t.AssignedTo = *ev.AssignedTo
		}
		if ev.DependsOn != nil {
			t.DependsOn = append([]string(nil), (*ev.DependsOn)...)
		}
		if ev.Notes != nil {
			t.Notes = *ev.Notes
		}
		if len(ev.AddEvidence) > 0 {
			t.Evidence = append(t.Evidence, ev.AddEvidence...)
		}
		t.UpdatedAt = ev.Timestamp
	}
}

// AddTask creates a new task and returns the materialised view.
func (p *Plan) AddTask(in NewTaskInput) (Task, error) {
	if in.Title == "" {
		return Task{}, errors.New("plan: title is required")
	}
	now := time.Now().UTC()
	id := newID()
	ev := Event{
		Op:        "create",
		ID:        id,
		Timestamp: now,
		Title:     in.Title,
		ParentID:  in.ParentID,
	}
	if len(in.DependsOn) > 0 {
		deps := append([]string(nil), in.DependsOn...)
		ev.DependsOn = &deps
	}
	if in.Notes != "" {
		notes := in.Notes
		ev.Notes = &notes
	}
	if err := p.write(ev); err != nil {
		return Task{}, err
	}
	t, _ := p.Get(id)
	return t, nil
}

// NewTaskInput is the user-facing shape for AddTask. Avoids a giant
// positional argument list.
type NewTaskInput struct {
	Title     string
	ParentID  string
	DependsOn []string
	Notes     string
}

// UpdateInput is a sparse mutation; nil pointers mean "leave alone".
type UpdateInput struct {
	Status      Status
	AssignedTo  *string
	Notes       *string
	DependsOn   *[]string
	AddEvidence []string
}

// UpdateTask applies a sparse mutation. ErrTaskNotFound when the id
// doesn't exist.
func (p *Plan) UpdateTask(id string, in UpdateInput) (Task, error) {
	if id == "" {
		return Task{}, errors.New("plan: id is required")
	}
	p.mu.Lock()
	_, ok := p.tasks[id]
	p.mu.Unlock()
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	ev := Event{
		Op:          "update",
		ID:          id,
		Timestamp:   time.Now().UTC(),
		Status:      in.Status,
		AssignedTo:  in.AssignedTo,
		DependsOn:   in.DependsOn,
		Notes:       in.Notes,
		AddEvidence: in.AddEvidence,
	}
	if err := p.write(ev); err != nil {
		return Task{}, err
	}
	t, _ := p.Get(id)
	return t, nil
}

// LinkJob adds jobID to the task's evidence list — shortcut for
// UpdateTask with AddEvidence.
func (p *Plan) LinkJob(taskID, jobID string) (Task, error) {
	return p.UpdateTask(taskID, UpdateInput{AddEvidence: []string{jobID}})
}

// Get returns the task with the given id and whether it existed.
func (p *Plan) Get(id string) (Task, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.tasks[id]
	if !ok {
		return Task{}, false
	}
	return cloneTask(*t), true
}

// Filter narrows List results.
type Filter struct {
	Status   Status // "" = any
	ParentID string // "" = any
	OpenOnly bool   // skip done/abandoned
}

// List returns tasks matching the filter, in insertion order.
func (p *Plan) List(f Filter) []Task {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Task, 0, len(p.order))
	for _, id := range p.order {
		t := p.tasks[id]
		if t == nil {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		if f.ParentID != "" && t.ParentID != f.ParentID {
			continue
		}
		if f.OpenOnly && !t.Status.IsOpen() {
			continue
		}
		out = append(out, cloneTask(*t))
	}
	// Stable secondary sort by created_at so insertion order ties
	// resolve predictably.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Close shuts down the appender; future writes will fail.
func (p *Plan) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return nil
	}
	err := p.f.Close()
	p.f = nil
	return err
}

// write serialises an event to the JSONL file and folds it into the
// snapshot. Single critical section so disk-vs-memory state can't
// drift if two writers race.
func (p *Plan) write(ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("plan: marshal: %w", err)
	}
	body = append(body, '\n')
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return errors.New("plan: closed")
	}
	if _, err := p.f.Write(body); err != nil {
		return fmt.Errorf("plan: write: %w", err)
	}
	p.apply(ev)
	return nil
}

// cloneTask returns a deep-enough copy that the caller can hold onto
// without seeing later mutations. Slices are copied; primitives are
// value types already.
func cloneTask(t Task) Task {
	if len(t.DependsOn) > 0 {
		t.DependsOn = append([]string(nil), t.DependsOn...)
	}
	if len(t.Evidence) > 0 {
		t.Evidence = append([]string(nil), t.Evidence...)
	}
	return t
}

// newID returns a short hex id with a "t-" prefix so it's easy to
// distinguish from agent ids and job ids at a glance.
func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable; using time as a
		// fallback would create predictable ids.
		panic("plan: read random: " + err.Error())
	}
	return "t-" + hex.EncodeToString(b[:])
}
