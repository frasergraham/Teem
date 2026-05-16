package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/frasergraham/teem/internal/wsbus"
)

// Heartbeat cadence (nanoseconds). atomic so tests can shrink it
// without racing the handler goroutines that read it.
var apiEventsPingIntervalNS atomic.Int64

func init() {
	apiEventsPingIntervalNS.Store(int64(30 * time.Second))
}

func pingInterval() time.Duration {
	return time.Duration(apiEventsPingIntervalNS.Load())
}

// backfillDefault is how many recent envelopes we replay when a client
// connects without a since_seq (?since_seq=0 or missing) — enough to
// paint a first frame from the ring buffer without forcing a /state
// fetch immediately.
const backfillDefault = 50

// handleAPITeamEvents handles GET /api/teams/<id>/events. It upgrades
// to a WebSocket, optionally backfills missed envelopes from the
// per-team wsbus ring buffer (?since_seq=N), and streams live
// envelopes until the client disconnects.
//
// Auth model matches the rest of /api/teams: tailnet boundary, no
// bearer. Origin check is permissive (the dashboard origin is the same
// hostname as the API).
func (d *daemon) handleAPITeamEvents(w http.ResponseWriter, r *http.Request, rt *registeredTeam) {
	if rt == nil || rt.wsbus == nil {
		http.Error(w, "team not ready", http.StatusServiceUnavailable)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		// Accept already wrote a response.
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sinceSeq := parseSinceSeq(r.URL.Query().Get("since_seq"))

	if err := backfillEnvelopes(ctx, c, rt.wsbus, sinceSeq); err != nil {
		// Client gone or backfill write failed — bail. CloseNow runs
		// via the outer defer.
		return
	}

	_, ch, unsub := rt.wsbus.Subscribe()
	defer unsub()

	// Live-push goroutine and heartbeat goroutine share ctx; the first
	// one to error/close cancels the other through ctx.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case env, ok := <-ch:
				if !ok {
					return
				}
				if err := wsjson.Write(ctx, c, env); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		t := time.NewTicker(pingInterval())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ping := wsbus.Envelope{
					Kind: "ping",
					Seq:  rt.wsbus.NextSeq(),
					TS:   time.Now().UTC(),
				}
				if err := wsjson.Write(ctx, c, ping); err != nil {
					return
				}
			}
		}
	}()
	wg.Wait()
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// parseSinceSeq parses the ?since_seq= query parameter. Malformed or
// missing → 0 (request a default backfill window).
func parseSinceSeq(raw string) uint64 {
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// backfillEnvelopes writes the catch-up window for a freshly-connected
// client. The three branches:
//
//   - sinceSeq == 0          → replay up to backfillDefault recent envelopes
//   - sinceSeq < oldest.Seq  → send snapshot_invalidate (client must refetch /state)
//   - otherwise              → replay every retained envelope with Seq > sinceSeq
func backfillEnvelopes(ctx context.Context, c *websocket.Conn, bus *wsbus.Bus, sinceSeq uint64) error {
	recent := bus.Recent(0) // full ring
	if sinceSeq == 0 {
		// Default backfill: last N from the ring.
		if len(recent) > backfillDefault {
			recent = recent[len(recent)-backfillDefault:]
		}
		return writeEnvelopes(ctx, c, recent)
	}
	if len(recent) == 0 {
		return nil
	}
	if sinceSeq < recent[0].Seq {
		inv := wsbus.Envelope{
			Kind:   "snapshot_invalidate",
			Seq:    bus.NextSeq(),
			TS:     time.Now().UTC(),
			Reason: fmt.Sprintf("since_seq=%d older than ring head=%d", sinceSeq, recent[0].Seq),
		}
		return wsjson.Write(ctx, c, inv)
	}
	// Replay everything after sinceSeq. Linear scan is fine — ring is
	// O(2000).
	for _, env := range recent {
		if env.Seq <= sinceSeq {
			continue
		}
		if err := wsjson.Write(ctx, c, env); err != nil {
			return err
		}
	}
	return nil
}

func writeEnvelopes(ctx context.Context, c *websocket.Conn, envs []wsbus.Envelope) error {
	for _, env := range envs {
		if err := wsjson.Write(ctx, c, env); err != nil {
			return err
		}
	}
	return nil
}
