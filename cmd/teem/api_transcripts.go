package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleAPITeamTranscriptGet serves GET
// /api/teams/<id>/transcripts/<agent>/<job> as raw NDJSON.
//
// Auth model: tailnet-boundary unauth (same as /api/teams/<id>/state).
// The bearer-gated SSR endpoint at /teams/<id>/transcripts/... is
// still the only writer — POST is not exposed here. The split keeps
// the SPA modal's "open transcript" link clickable from a browser
// session that has no token, without weakening the upload path.
func (d *daemon) handleAPITeamTranscriptGet(w http.ResponseWriter, r *http.Request, rt *registeredTeam, rest string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.Error(w, "want /transcripts/<agent_id>/<job_id>", http.StatusBadRequest)
		return
	}
	agentID, jobID := rest[:slash], rest[slash+1:]
	if !isSafeID(agentID) || !isSafeID(jobID) {
		http.Error(w, "bad agent_id or job_id", http.StatusBadRequest)
		return
	}
	if rt.transcriptsDir == "" {
		http.Error(w, "transcripts not configured", http.StatusInternalServerError)
		return
	}
	path := filepath.Join(rt.transcriptsDir, agentID, jobID+".jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	// X-Content-Type-Options stops a browser from sniffing the NDJSON
	// as HTML when the operator opens the link directly.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}
