package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// leaderSession is the per-team Claude Code session-id Teem persists so
// every `teem chat` (and eventually every autonomous Pulse tick)
// resumes the same conversation. Without this, every chat would start
// from a blank context.
type leaderSession struct {
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
}

// leaderSessionPath returns the on-disk location of the session-id
// record for a team. Co-located with the daemon's other per-team state.
func leaderSessionPath(teamName string) string {
	return filepath.Join(defaultStateDir(teamName), "leader-session.json")
}

// loadLeaderSession reads the persisted session record. Returns
// (empty, false, nil) when no session has been created yet.
func loadLeaderSession(teamName string) (leaderSession, bool, error) {
	body, err := os.ReadFile(leaderSessionPath(teamName))
	if err != nil {
		if os.IsNotExist(err) {
			return leaderSession{}, false, nil
		}
		return leaderSession{}, false, err
	}
	var s leaderSession
	if err := json.Unmarshal(body, &s); err != nil {
		return leaderSession{}, false, fmt.Errorf("leader-session: decode: %w", err)
	}
	if s.SessionID == "" {
		return leaderSession{}, false, nil
	}
	return s, true, nil
}

// saveLeaderSession writes the session record atomically.
func saveLeaderSession(teamName string, s leaderSession) error {
	if err := os.MkdirAll(defaultStateDir(teamName), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(leaderSessionPath(teamName), body)
}

// newSessionUUID returns a v4 UUID string suitable for
// `claude --session-id`. Format per RFC 4122 §4.4 (random bits with
// version/variant nibbles set).
func newSessionUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
