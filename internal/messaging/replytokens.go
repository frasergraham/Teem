package messaging

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultReplyTokenTTL is how long a reply token (issued alongside an
// outbound message) remains valid for an inbound /reply command. After
// the TTL elapses the token is dropped from the store on the next Lookup
// or Issue call. 24h matches the operator-grade "I saw the ping at the
// end of the day" window.
const DefaultReplyTokenTTL = 24 * time.Hour

// ReplyContext is the payload a successfully-matched reply token resolves
// to. The webhook handler uses these fields to spawn a leader chat
// subprocess scoped to the originating task.
type ReplyContext struct {
	TeamID    string    `json:"team_id"`
	TaskID    string    `json:"task_id"`
	AgentID   string    `json:"agent_id"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ReplyTokenStore maps short hex tokens to ReplyContext entries with an
// expiry. Persistence is best-effort to the configured path — a write
// failure does not block issuance. Concurrent-safe.
type ReplyTokenStore struct {
	mu     sync.Mutex
	path   string
	ttl    time.Duration
	now    func() time.Time
	tokens map[string]ReplyContext
}

// NewReplyTokenStore constructs a store backed by path (json), loading
// any prior tokens. A missing file is fine. A bad file is reported but
// the store still functions in-memory.
func NewReplyTokenStore(path string, ttl time.Duration) (*ReplyTokenStore, error) {
	if ttl <= 0 {
		ttl = DefaultReplyTokenTTL
	}
	s := &ReplyTokenStore{
		path:   path,
		ttl:    ttl,
		now:    time.Now,
		tokens: map[string]ReplyContext{},
	}
	if path == "" {
		return s, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, fmt.Errorf("messaging: read reply tokens: %w", err)
	}
	if len(body) == 0 {
		return s, nil
	}
	var stored map[string]ReplyContext
	if err := json.Unmarshal(body, &stored); err != nil {
		return s, fmt.Errorf("messaging: parse reply tokens: %w", err)
	}
	now := s.now()
	for k, v := range stored {
		if v.ExpiresAt.IsZero() || v.ExpiresAt.After(now) {
			s.tokens[k] = v
		}
	}
	return s, nil
}

// SetClock is the test seam.
func (s *ReplyTokenStore) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Issue allocates and stores a fresh token for ctx, returning the
// token string the caller embeds in the outbound message body. The
// token is 16 hex chars (8 bytes of entropy) — short enough to type
// on a phone, long enough to resist guessing inside the TTL.
func (s *ReplyTokenStore) Issue(ctx ReplyContext) (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	ctx.IssuedAt = now
	ctx.ExpiresAt = now.Add(s.ttl)
	s.tokens[tok] = ctx
	s.pruneLocked(now)
	s.persistLocked()
	return tok, nil
}

// Lookup returns the ReplyContext for token, or (zero, false) when the
// token is unknown or expired. Expired tokens are dropped as a side
// effect.
func (s *ReplyTokenStore) Lookup(token string) (ReplyContext, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	ctx, ok := s.tokens[token]
	if !ok {
		return ReplyContext{}, false
	}
	if !ctx.ExpiresAt.IsZero() && !ctx.ExpiresAt.After(now) {
		delete(s.tokens, token)
		s.persistLocked()
		return ReplyContext{}, false
	}
	return ctx, true
}

// pruneLocked drops any token whose ExpiresAt has passed.
func (s *ReplyTokenStore) pruneLocked(now time.Time) {
	for k, v := range s.tokens {
		if !v.ExpiresAt.IsZero() && !v.ExpiresAt.After(now) {
			delete(s.tokens, k)
		}
	}
}

func (s *ReplyTokenStore) persistLocked() {
	if s.path == "" {
		return
	}
	body, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// newToken returns 16 hex chars of crypto/rand entropy.
func newToken() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
