package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// transcriptWatchPollInterval is how often the watch handler reads
// fresh bytes from the transcript file. 300ms keeps the perceived
// latency well under a second while still letting the daemon run
// many tail loops without measurable load.
var transcriptWatchPollInterval = 300 * time.Millisecond

// transcriptWatchKeepalive is the cadence of SSE `:keepalive` comments
// fired when the file is idle. Browsers / proxies will assume a
// streaming connection is dead after ~30s of silence; 15s comfortably
// keeps the connection alive.
var transcriptWatchKeepalive = 15 * time.Second

// splitWatchPath parses "<agent>/<job>/watch" out of the rest of the
// /transcripts/ URL. Returns ok=false if the trailing segment is not
// exactly "watch" (so the existing read endpoint handles the request)
// or if the path doesn't split into exactly three non-empty parts.
func splitWatchPath(rest string) (agent, job string, ok bool) {
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[2] != "watch" || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// handleAPITeamTranscriptWatch serves GET
// /api/teams/<id>/transcripts/<agent>/<job>/watch as a Server-Sent
// Events stream of the agent's live JSONL transcript file.
//
//	On open:    every NDJSON line already in the file is emitted as
//	            one SSE `data:` event.
//	While open: the file is re-read every transcriptWatchPollInterval
//	            and any newly-appended lines are emitted as `data:`
//	            events. A `:keepalive` comment is sent every
//	            transcriptWatchKeepalive to keep idle connections alive.
//	On NDJSON `{"type":"result", ...}`: emits `event: done` then closes.
//	On file vanished mid-stream:        emits `event: error` then closes.
//	404 (before the stream opens) if the file does not exist.
//
// Auth model matches /api/teams: tailnet boundary, no bearer (the
// transcript file is the same one /api/teams/<id>/transcripts/<a>/<j>
// already serves verbatim).
func (d *daemon) handleAPITeamTranscriptWatch(w http.ResponseWriter, r *http.Request, rt *registeredTeam, agentID, jobID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isSafeID(agentID) || !isSafeID(jobID) {
		http.Error(w, "bad agent_id or job_id", http.StatusBadRequest)
		return
	}
	if rt.transcriptsDir == "" {
		http.Error(w, "transcripts not configured", http.StatusInternalServerError)
		return
	}
	path := filepath.Join(rt.transcriptsDir, agentID, jobID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "open: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	streamTranscriptWatch(r.Context(), w, flusher, f, path)
}

// streamTranscriptWatch is the side-effect-free core of the watch
// handler. r is the opened transcript file (or any io.Reader for
// tests); path is the on-disk file used only for the "file vanished"
// probe between polls — pass "" to disable that probe (test mode
// driven by a pipe or pre-filled bytes.Buffer).
func streamTranscriptWatch(ctx context.Context, w io.Writer, flusher http.Flusher, r io.Reader, path string) {
	keepalive := time.NewTicker(transcriptWatchKeepalive)
	defer keepalive.Stop()

	t := newTranscriptTailer(r)

	for {
		done, err := t.pump(w, flusher)
		if err != nil {
			writeSSEEvent(w, flusher, "error", err.Error())
			return
		}
		if done {
			writeSSEEvent(w, flusher, "done", "")
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			writeSSEKeepalive(w, flusher)
		case <-time.After(transcriptWatchPollInterval):
			if path != "" {
				if _, err := os.Stat(path); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						writeSSEEvent(w, flusher, "error", "transcript file removed")
					} else {
						writeSSEEvent(w, flusher, "error", "stat: "+err.Error())
					}
					return
				}
			}
		}
	}
}

// transcriptTailer drains the underlying reader on each pump, splits
// any complete NDJSON lines, and carries trailing partial bytes over
// to the next pump so we never emit a half-line.
type transcriptTailer struct {
	r       io.Reader
	carry   []byte
	scratch [4096]byte
}

func newTranscriptTailer(r io.Reader) *transcriptTailer {
	return &transcriptTailer{r: r}
}

// pump reads every byte currently available from the underlying
// reader, emits complete lines as SSE `data:` events, and reports
// done=true when a `{"type":"result"}` line is encountered.
// io.EOF on a regular file is normal (means "no new bytes yet") and
// is not propagated as an error — the caller polls again on the next
// tick. Any other error is propagated.
func (t *transcriptTailer) pump(w io.Writer, flusher http.Flusher) (bool, error) {
	for {
		n, err := t.r.Read(t.scratch[:])
		if n > 0 {
			t.carry = append(t.carry, t.scratch[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return false, err
		}
		// Loop and keep reading until we hit EOF — the underlying
		// reader may have more buffered (or, in tests, an io.Pipe
		// queues bytes inside a single Write).
	}
	return t.flushLines(w, flusher), nil
}

// flushLines emits every newline-terminated chunk in t.carry as one
// SSE event. Trailing partial bytes (no newline yet) stay in carry
// for the next pump. Returns true when one of the flushed lines is
// a `result` line so the caller can close the stream.
func (t *transcriptTailer) flushLines(w io.Writer, flusher http.Flusher) bool {
	done := false
	for {
		idx := bytes.IndexByte(t.carry, '\n')
		if idx < 0 {
			return done
		}
		line := t.carry[:idx]
		t.carry = t.carry[idx+1:]
		trimmed := bytes.TrimRight(line, "\r")
		if len(trimmed) == 0 {
			continue
		}
		writeSSEEvent(w, flusher, "", string(trimmed))
		if isResultLine(trimmed) {
			done = true
		}
	}
}

// isResultLine reports whether the NDJSON line decodes to an object
// with `"type":"result"`. Malformed lines (truncated writes during
// log rotation) just return false — we keep streaming until the next
// well-formed line decides what to do.
func isResultLine(line []byte) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return false
	}
	return probe.Type == "result"
}

// writeSSEEvent writes one SSE frame. event is the optional event
// name (`event: done`, `event: error`); empty event keeps the
// default `message` (a plain `data:` block, which is what
// EventSource.onmessage handles). data is split on '\n' into one
// `data:` line per chunk so multi-line strings survive transport.
func writeSSEEvent(w io.Writer, flusher http.Flusher, event, data string) {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteString("\n")
	}
	if data == "" {
		// SSE permits an empty data field; emit one so the frame is
		// well-formed for named events (the browser dispatches the
		// event even when the data buffer is empty).
		b.WriteString("data:\n")
	} else {
		for _, chunk := range strings.Split(data, "\n") {
			b.WriteString("data: ")
			b.WriteString(chunk)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	_, _ = fmt.Fprint(w, b.String())
	flusher.Flush()
}

// writeSSEKeepalive emits an SSE comment frame. Comments are ignored
// by EventSource but reset both proxy and browser idle timers.
func writeSSEKeepalive(w io.Writer, flusher http.Flusher) {
	_, _ = fmt.Fprint(w, ":keepalive\n\n")
	flusher.Flush()
}
