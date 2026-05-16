package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/wsbus"
)

// dialEvents opens a WebSocket to /api/teams/<id>/events on srv with
// the given since_seq (0 omits the param) and returns the conn plus a
// per-call cancel function. Caller defers cancel and conn.Close.
func dialEvents(t *testing.T, srv *httptest.Server, teamID string, sinceSeq string) (*websocket.Conn, context.CancelFunc) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/teams/" + teamID + "/events"
	if sinceSeq != "" {
		wsURL += "?since_seq=" + sinceSeq
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	return c, cancel
}

// teamWithWSBus builds a minimal registeredTeam wired with a wsbus
// suitable for the events endpoint. The daemon's handler() dispatches
// /api/teams/<id>/events through the same code path as production.
func teamWithWSBus(t *testing.T, id string, ringSize int) (*daemon, *registeredTeam, *httptest.Server) {
	t.Helper()
	rt := newFullTestTeam(t, id)
	rt.wsbus = wsbus.New(ringSize)
	d := &daemon{teams: map[string]*registeredTeam{id: rt}, baseCtx: context.Background()}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	return d, rt, srv
}

func TestAPIEvents_HandshakeAndBackfill(t *testing.T) {
	_, rt, srv := teamWithWSBus(t, "alpha", 2000)

	// Publish 5 envelopes BEFORE the client connects so they all sit
	// in the ring buffer for backfill.
	for i := 0; i < 5; i++ {
		rt.wsbus.Publish(wsbus.Envelope{
			Kind:  "audit",
			Seq:   rt.wsbus.NextSeq(),
			TS:    time.Now().UTC(),
			Event: &audit.Event{Kind: audit.KindHeartbeat, AgentID: fmt.Sprintf("worker-%d", i)},
		})
	}

	c, cancel := dialEvents(t, srv, "alpha", "0")
	defer cancel()
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	for i := 0; i < 5; i++ {
		var env wsbus.Envelope
		if err := wsjson.Read(ctx, c, &env); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if env.Kind != "audit" {
			t.Errorf("envelope %d: kind=%q want audit", i, env.Kind)
		}
		if env.Seq != uint64(i+1) {
			t.Errorf("envelope %d: seq=%d want %d", i, env.Seq, i+1)
		}
	}
}

func TestAPIEvents_SnapshotInvalidateWhenSinceSeqTooOld(t *testing.T) {
	// Tiny ring (4 slots) so we can overflow it without publishing
	// thousands of envelopes.
	_, rt, srv := teamWithWSBus(t, "alpha", 4)

	// Publish 10 envelopes; ring retains seqs 7..10. since_seq=1 is
	// older than the ring head (7), so we expect a snapshot_invalidate.
	for i := 0; i < 10; i++ {
		rt.wsbus.Publish(wsbus.Envelope{
			Kind: "audit",
			Seq:  rt.wsbus.NextSeq(),
			TS:   time.Now().UTC(),
		})
	}

	c, cancel := dialEvents(t, srv, "alpha", "1")
	defer cancel()
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	var env wsbus.Envelope
	if err := wsjson.Read(ctx, c, &env); err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Kind != "snapshot_invalidate" {
		t.Fatalf("first envelope: kind=%q want snapshot_invalidate (reason=%q)", env.Kind, env.Reason)
	}
	if env.Reason == "" {
		t.Errorf("snapshot_invalidate: empty reason")
	}
}

func TestAPIEvents_LivePush(t *testing.T) {
	_, rt, srv := teamWithWSBus(t, "alpha", 2000)

	c, cancel := dialEvents(t, srv, "alpha", "0")
	defer cancel()
	defer c.Close(websocket.StatusNormalClosure, "")

	// Backfill is empty (no prior Publishes); first read should pick
	// up the post-connect envelope. Subscribe is registered on the
	// server side as part of the handler — but there's a small window
	// between Accept and Subscribe. Sleep briefly so the server
	// finishes its backfill (no-op here) and Subscribes.
	time.Sleep(50 * time.Millisecond)

	rt.wsbus.Publish(wsbus.Envelope{
		Kind:  "audit",
		Seq:   rt.wsbus.NextSeq(),
		TS:    time.Now().UTC(),
		Event: &audit.Event{Kind: audit.KindHeartbeat, AgentID: "worker-live"},
	})

	ctx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	var env wsbus.Envelope
	if err := wsjson.Read(ctx, c, &env); err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Kind != "audit" {
		t.Fatalf("kind=%q want audit", env.Kind)
	}
	if env.Event == nil || env.Event.AgentID != "worker-live" {
		t.Fatalf("event agent_id=%v want worker-live", env.Event)
	}
}

func TestAPIEvents_PingHeartbeat(t *testing.T) {
	prev := apiEventsPingIntervalNS.Load()
	apiEventsPingIntervalNS.Store(int64(50 * time.Millisecond))
	t.Cleanup(func() { apiEventsPingIntervalNS.Store(prev) })

	_, _, srv := teamWithWSBus(t, "alpha", 2000)

	c, cancel := dialEvents(t, srv, "alpha", "0")
	defer cancel()
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	// First non-audit envelope must be a ping; ring is empty so no
	// backfill envelopes are sent first.
	var env wsbus.Envelope
	if err := wsjson.Read(ctx, c, &env); err != nil {
		t.Fatalf("read: %v", err)
	}
	if env.Kind != "ping" {
		t.Fatalf("first envelope: kind=%q want ping", env.Kind)
	}
}

func TestAPIEvents_UnknownTeam(t *testing.T) {
	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: context.Background()}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/teams/missing/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
}
