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

	// Subscribe FIRST so any envelope published between now and the
	// Recent() snapshot inside backfillEnvelopes lands on liveCh.
	// Backfill drains from the ring; the live loop dedups by Seq
	// against lastWritten so we don't deliver the overlap twice.
	_, ch, unsub := rt.wsbus.Subscribe()
	defer unsub()

	lastWritten, err := backfillEnvelopes(ctx, c, rt.wsbus, sinceSeq)
	if err != nil {
		// Client gone or backfill write failed — bail. CloseNow runs
		// via the outer defer.
		return
	}

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
				// Dedup: envelopes published between Subscribe and the
				// Recent() snapshot land in both the ring and on ch.
				// We delivered them via backfill already.
				if env.Seq <= lastWritten {
					continue
				}
				if err := wsjson.Write(ctx, c, env); err != nil {
					return
				}
				lastWritten = env.Seq
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
// client and returns the highest Seq it wrote (0 if nothing). The three
// branches:
//
//   - sinceSeq == 0          → replay up to backfillDefault recent envelopes
//   - sinceSeq < oldest.Seq  → send snapshot_invalidate (client must refetch /state)
//   - otherwise              → replay every retained envelope with Seq > sinceSeq
func backfillEnvelopes(ctx context.Context, c *websocket.Conn, bus *wsbus.Bus, sinceSeq uint64) (uint64, error) {
	if sinceSeq == 0 {
		// Default backfill: last N from the ring.
		return writeEnvelopes(ctx, c, bus.Recent(backfillDefault))
	}
	recent := bus.Recent(0) // full ring
	if len(recent) == 0 {
		return 0, nil
	}
	if sinceSeq < recent[0].Seq {
		inv := wsbus.Envelope{
			Kind:   "snapshot_invalidate",
			Seq:    bus.NextSeq(),
			TS:     time.Now().UTC(),
			Reason: fmt.Sprintf("since_seq=%d older than ring head=%d", sinceSeq, recent[0].Seq),
		}
		if err := wsjson.Write(ctx, c, inv); err != nil {
			return 0, err
		}
		return inv.Seq, nil
	}
	// Replay everything after sinceSeq. Linear scan is fine — ring is
	// O(2000).
	filtered := recent[:0]
	for _, env := range recent {
		if env.Seq > sinceSeq {
			filtered = append(filtered, env)
		}
	}
	return writeEnvelopes(ctx, c, filtered)
}

func writeEnvelopes(ctx context.Context, c *websocket.Conn, envs []wsbus.Envelope) (uint64, error) {
	var last uint64
	for _, env := range envs {
		if err := wsjson.Write(ctx, c, env); err != nil {
			return last, err
		}
		if env.Seq > last {
			last = env.Seq
		}
	}
	return last, nil
}
