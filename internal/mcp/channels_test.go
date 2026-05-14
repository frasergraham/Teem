package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/team"
)

// channelTestSession is a minimal ClientSession used to capture
// notifications PushChannel emits without standing up an HTTP
// transport.
//
// We bypass the real initialize handshake: the underlying
// SendNotificationToAllClients (mcp-go server/session.go:172) filters
// recipients on session.Initialized(), so we return true unconditionally
// to receive every broadcast. If mcp-go ever adds a stricter recipient
// filter (e.g. tracking a `notifications/initialized` ack), this fake
// will silently drop the notification and these tests will need to
// drive the real handshake — register the session, then hand it the
// `initialize` request through srv.core.HandleMessage.
type channelTestSession struct {
	id string
	ch chan mcpgo.JSONRPCNotification
}

func (s *channelTestSession) SessionID() string                                     { return s.id }
func (s *channelTestSession) NotificationChannel() chan<- mcpgo.JSONRPCNotification { return s.ch }
func (s *channelTestSession) Initialize()                                           {}
func (s *channelTestSession) Initialized() bool                                     { return true }

func newChannelTestServer(t *testing.T) *Server {
	t.Helper()
	tm := &team.Team{Name: "chan-test", Leader: team.LeaderSpec{SystemPrompt: "p"}}
	srv, err := New(Config{
		Bus:      bus.NewMemBus(),
		Team:     tm,
		Registry: NewRegistry(),
		Spawner:  &fakeSpawner{running: map[string]bool{}, roles: map[string]string{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// TestInitialize_AdvertisesChannelCapability checks the initialize
// response includes experimental.claude/channel so a leader launched
// with --channels server:teem actually subscribes.
func TestInitialize_AdvertisesChannelCapability(t *testing.T) {
	srv := newChannelTestServer(t)
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	resp := srv.core.HandleMessage(context.Background(), json.RawMessage(req))
	if resp == nil {
		t.Fatal("nil response from initialize")
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var parsed struct {
		Result struct {
			Capabilities struct {
				Experimental map[string]any `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if _, ok := parsed.Result.Capabilities.Experimental["claude/channel"]; !ok {
		t.Fatalf("expected experimental.claude/channel in initialize response; got %s", string(raw))
	}
}

// TestPushChannel_FansOutToSessions registers a fake session, calls
// PushChannel, and asserts the notification's method/params shape
// matches what Claude Code expects on the channels stream.
func TestPushChannel_FansOutToSessions(t *testing.T) {
	srv := newChannelTestServer(t)
	sess := &channelTestSession{
		id: "test-session",
		ch: make(chan mcpgo.JSONRPCNotification, 4),
	}
	if err := srv.core.RegisterSession(context.Background(), sess); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}

	srv.PushChannel("worker-ada finished job j-1234", map[string]string{
		"agent_id": "worker-ada",
		"job_id":   "j-1234",
		"kind":     "job_complete",
	})

	select {
	case n := <-sess.ch:
		if n.Method != "notifications/claude/channel" {
			t.Fatalf("method = %q, want notifications/claude/channel", n.Method)
		}
		content, _ := n.Params.AdditionalFields["content"].(string)
		if content != "worker-ada finished job j-1234" {
			t.Fatalf("content = %q, want worker-ada finished job j-1234", content)
		}
		metaAny, ok := n.Params.AdditionalFields["meta"]
		if !ok {
			t.Fatalf("missing meta in notification; fields=%v", n.Params.AdditionalFields)
		}
		meta, ok := metaAny.(map[string]any)
		if !ok {
			t.Fatalf("meta is not a map: %T", metaAny)
		}
		if meta["agent_id"] != "worker-ada" || meta["job_id"] != "j-1234" || meta["kind"] != "job_complete" {
			t.Fatalf("meta mismatch: %v", meta)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive notification")
	}
}

// TestPushChannel_NoSessionsIsSafe ensures PushChannel doesn't panic
// or error when called before any leader has connected. Channels are
// fire-and-forget — silence is correct.
func TestPushChannel_NoSessionsIsSafe(t *testing.T) {
	srv := newChannelTestServer(t)
	srv.PushChannel("noone listening", nil)
}
