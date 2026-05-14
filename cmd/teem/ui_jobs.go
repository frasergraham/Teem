package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// agentJobsView is the data passed to the agent_jobs template.
type agentJobsView struct {
	Team         string
	AgentID      string
	NowFormatted string
	Jobs         []agentJobRow
}

// agentJobRow is one row of the per-agent jobs table.
type agentJobRow struct {
	JobID         string
	Status        string
	StartedAgo    string
	StartedShort  string
	Duration      string
	Prompt        string
	Summary       string
	JobURL        string
	HasTranscript bool
}

// jobDetailView is the data passed to the job_detail template. Turns
// is populated from the on-disk transcript when available; the
// MaterializedJob is the audit-derived metadata.
type jobDetailView struct {
	Team            string
	Job             audit.MaterializedJob
	JobsBackURL     string
	NowFormatted    string
	Duration        string
	TranscriptBytes string
	HaveTranscript  bool
	Turns           []transcriptTurn
	TranscriptError string
}

// transcriptTurn is one card in the rendered transcript. Kind drives
// CSS class and card title; Text is the body (verbatim, escaped by the
// template engine).
type transcriptTurn struct {
	Kind  string // "assistant" / "tool_use" / "tool_result" / "result" / "system" / "user"
	Title string
	Text  string
	// Collapsed is true for tool_result / system cards we want to hide
	// behind a <details> by default.
	Collapsed bool
}

// renderAgentJobs writes the per-agent jobs page. The set is built by
// scanning the last 72h of audit events and materializing them.
func (d *daemon) renderAgentJobs(w http.ResponseWriter, _ *http.Request, rt *registeredTeam, agentID string) {
	if !isSafeID(agentID) {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return
	}
	view := agentJobsView{
		Team:         rt.team.Name,
		AgentID:      agentID,
		NowFormatted: time.Now().Local().Format("Mon Jan 2 15:04:05"),
	}

	if rt.auditSink != nil {
		events, err := rt.auditSink.Query("", time.Now().Add(-72*time.Hour), 5000)
		if err == nil {
			scoped := make([]audit.Event, 0, len(events))
			for _, e := range events {
				if e.AgentID == agentID {
					scoped = append(scoped, e)
				}
			}
			jobs := audit.MaterializeJobs(scoped)
			// Newest first — MaterializeJobs sorts ascending by StartedAt.
			sort.Slice(jobs, func(i, j int) bool {
				return jobs[i].StartedAt.After(jobs[j].StartedAt)
			})
			if len(jobs) > 50 {
				jobs = jobs[:50]
			}
			for _, j := range jobs {
				view.Jobs = append(view.Jobs, agentJobRow{
					JobID:         j.JobID,
					Status:        j.Status,
					StartedAgo:    agoShort(j.StartedAt),
					StartedShort:  timeShort(j.StartedAt),
					Duration:      durShort(j.Duration()),
					Prompt:        j.Prompt,
					Summary:       j.Summary,
					JobURL:        fmt.Sprintf("/teams/%s/jobs/%s", rt.team.ID, j.JobID),
					HasTranscript: j.TranscriptBytes > 0,
				})
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := uiTemplates.ExecuteTemplate(w, "agent_jobs", view); err != nil {
		fmt.Printf("[teemd] agent_jobs render: %v\n", err)
	}
}

// renderJobDetail writes a single job's detail page: audit-derived
// metadata + the rendered transcript when one exists on disk.
func (d *daemon) renderJobDetail(w http.ResponseWriter, _ *http.Request, rt *registeredTeam, jobID string) {
	if !isSafeID(jobID) {
		http.Error(w, "bad job id", http.StatusBadRequest)
		return
	}
	if rt.auditSink == nil {
		http.Error(w, "audit unavailable", http.StatusInternalServerError)
		return
	}
	// Wider window than agent-jobs: detail page is reached by direct
	// link and the operator may want to look at older runs.
	events, err := rt.auditSink.Query("", time.Time{}, 0)
	if err != nil {
		http.Error(w, "query audit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	job, ok := audit.MaterializeJob(events, jobID)
	if !ok {
		http.NotFound(w, nil)
		return
	}

	view := jobDetailView{
		Team:         rt.team.Name,
		Job:          job,
		NowFormatted: time.Now().Local().Format("Mon Jan 2 15:04:05"),
		Duration:     durShort(job.Duration()),
	}
	if job.AgentID != "" {
		view.JobsBackURL = fmt.Sprintf("/teams/%s/agents/%s/jobs", rt.team.ID, job.AgentID)
	}

	// Try to load the on-disk transcript. If absent, the template
	// falls back to rendering the audit-derived prompt/output.
	if rt.transcriptsDir != "" && job.AgentID != "" && isSafeID(job.AgentID) {
		path := filepath.Join(rt.transcriptsDir, job.AgentID, jobID+".jsonl")
		body, rerr := os.ReadFile(path)
		switch {
		case rerr == nil:
			view.HaveTranscript = true
			view.TranscriptBytes = bytesShort(len(body))
			turns, perr := parseTranscriptTurns(body)
			if perr != nil {
				view.TranscriptError = perr.Error()
			}
			view.Turns = turns
		case errors.Is(rerr, os.ErrNotExist):
			// Leave HaveTranscript=false so template shows the fallback.
		default:
			view.TranscriptError = rerr.Error()
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := uiTemplates.ExecuteTemplate(w, "job_detail", view); err != nil {
		fmt.Printf("[teemd] job_detail render: %v\n", err)
	}
}

// parseTranscriptTurns walks the NDJSON stream-json body and emits one
// card per relevant event. This is intentionally a thin local parser:
// the canonical parser lives in internal/mcp/transcript.go (renderTranscriptText),
// which produces a flat string rendering instead of cards — different
// caller, different output shape.
func parseTranscriptTurns(body []byte) ([]transcriptTurn, error) {
	type contentBlock struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name,omitempty"`
		Input   json.RawMessage `json:"input,omitempty"`
		Content json.RawMessage `json:"content,omitempty"` // tool_result body
	}
	type msg struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype,omitempty"`
		Result  string `json:"result"`
		Message msg    `json:"message"`
	}

	var turns []transcriptTurn
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		var e ev
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// Skip malformed lines; the worker writes them only on
			// process-tear-down truncation which is rare.
			continue
		}
		switch e.Type {
		case "system":
			// Render as a single muted, collapsed card. Body is short
			// (init metadata) so we don't bother extracting fields.
			turns = append(turns, transcriptTurn{
				Kind:      "system",
				Title:     "System",
				Text:      string(sc.Bytes()),
				Collapsed: true,
			})
		case "assistant", "user":
			for _, c := range e.Message.Content {
				switch c.Type {
				case "text":
					if c.Text == "" {
						continue
					}
					turns = append(turns, transcriptTurn{
						Kind:  e.Type,
						Title: titleForRole(e.Type),
						Text:  c.Text,
					})
				case "tool_use":
					turns = append(turns, transcriptTurn{
						Kind:  "tool_use",
						Title: "Tool call · " + c.Name,
						Text:  prettyJSON(c.Input),
					})
				case "tool_result":
					text := c.Text
					if text == "" {
						text = string(c.Content)
					}
					turns = append(turns, transcriptTurn{
						Kind:      "tool_result",
						Title:     "Tool result",
						Text:      text,
						Collapsed: true,
					})
				}
			}
		case "result":
			turns = append(turns, transcriptTurn{
				Kind:  "result",
				Title: "Result",
				Text:  e.Result,
			})
		}
	}
	if err := sc.Err(); err != nil {
		return turns, err
	}
	if line == 0 {
		return turns, nil
	}
	return turns, nil
}

func titleForRole(role string) string {
	switch role {
	case "assistant":
		return "Assistant"
	case "user":
		return "User"
	default:
		return role
	}
}

// prettyJSON re-indents a JSON blob for legibility. Falls back to the
// raw bytes when parsing fails so we still show something.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return out.String()
}

// resolveAgentJobsRoute parses /teams/<team>/agents/<agentID>/jobs.
// Returns the agent_id when the suffix matches the pattern; "" means
// not this route.
func resolveAgentJobsRoute(suffix string) (string, bool) {
	const prefix = "/agents/"
	const tail = "/jobs"
	if !strings.HasPrefix(suffix, prefix) {
		return "", false
	}
	rest := suffix[len(prefix):]
	if !strings.HasSuffix(rest, tail) {
		return "", false
	}
	id := rest[:len(rest)-len(tail)]
	if id == "" {
		return "", false
	}
	return id, true
}

// resolveJobDetailRoute parses /teams/<team>/jobs/<jobID>.
func resolveJobDetailRoute(suffix string) (string, bool) {
	const prefix = "/jobs/"
	if !strings.HasPrefix(suffix, prefix) {
		return "", false
	}
	id := suffix[len(prefix):]
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}
