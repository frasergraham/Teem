package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Handler returns an http.Handler that serves the leader-side audit
// endpoints, gated by a bearer token (the shared worker token).
//
//	POST /audit   — body: JSON array of Events; appends each to sink.
//	GET  /audit   — query params: agent, since (RFC3339), limit
//	                returns JSON array of matching Events.
//
// Both routes require Authorization: Bearer <token>.
func Handler(sink Sink, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/audit", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			handlePost(w, r, sink)
		case http.MethodGet:
			handleGet(w, r, sink)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func handlePost(w http.ResponseWriter, r *http.Request, sink Sink) {
	var events []Event
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	var firstErr error
	written := 0
	for _, e := range events {
		if e.AgentID == "" || e.Kind == "" {
			if firstErr == nil {
				firstErr = errors.New("event missing agent_id or kind")
			}
			continue
		}
		if err := sink.Write(e); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		written++
	}
	if firstErr != nil && written == 0 {
		http.Error(w, firstErr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	resp := map[string]any{"written": written, "total": len(events)}
	if firstErr != nil {
		resp["partial_error"] = firstErr.Error()
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func handleGet(w http.ResponseWriter, r *http.Request, sink Sink) {
	agent := r.URL.Query().Get("agent")
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			http.Error(w, "bad since: "+err.Error(), http.StatusBadRequest)
			return
		}
		since = t
	}
	limit := 100
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			http.Error(w, "bad limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	events, err := sink.Query(agent, since, limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("query: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(events)
}
