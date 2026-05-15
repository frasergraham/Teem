package messaging

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReplyToken_IssueLookupRoundtrip(t *testing.T) {
	s, err := NewReplyTokenStore(filepath.Join(t.TempDir(), "tok.json"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Issue(ReplyContext{TeamID: "team-a", TaskID: "t-1", AgentID: "worker-una"})
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	got, ok := s.Lookup(tok)
	if !ok {
		t.Fatal("lookup miss after issue")
	}
	if got.TaskID != "t-1" || got.TeamID != "team-a" || got.AgentID != "worker-una" {
		t.Errorf("ctx round-trip mismatch: %+v", got)
	}
}

func TestReplyToken_ExpiresAfterTTL(t *testing.T) {
	s, err := NewReplyTokenStore(filepath.Join(t.TempDir(), "tok.json"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	tok, _ := s.Issue(ReplyContext{TeamID: "x", TaskID: "t-1"})
	// 23h later: still valid
	s.SetClock(func() time.Time { return now.Add(23 * time.Hour) })
	if _, ok := s.Lookup(tok); !ok {
		t.Fatal("token should be valid at 23h")
	}
	// 25h later: expired
	s.SetClock(func() time.Time { return now.Add(25 * time.Hour) })
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("token should be expired after 24h TTL")
	}
}

func TestReplyToken_UnknownToken(t *testing.T) {
	s, _ := NewReplyTokenStore(filepath.Join(t.TempDir(), "tok.json"), time.Hour)
	if _, ok := s.Lookup("nope"); ok {
		t.Fatal("unknown token returned hit")
	}
}

func TestReplyToken_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok.json")
	s1, _ := NewReplyTokenStore(path, time.Hour)
	tok, _ := s1.Issue(ReplyContext{TeamID: "x", TaskID: "t-1"})
	// Reload — token should still resolve.
	s2, err := NewReplyTokenStore(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Lookup(tok); !ok {
		t.Fatal("token did not survive reload")
	}
}
