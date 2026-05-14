package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// transcriptBodyCap is the maximum response size for get_job_transcript
// when format=raw and no head limit was supplied. Matches the cap
// chosen for assistant-text bodies in audit events.
const transcriptBodyCap = 200 * 1024

var transcriptIDRegexp = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (s *Server) handleGetJobTranscript(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.transcriptsDir == "" {
		return mcpgo.NewToolResultError("transcripts directory is not configured"), nil
	}
	agentID, err := req.RequireString("agent_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if !transcriptIDRegexp.MatchString(agentID) || !transcriptIDRegexp.MatchString(jobID) {
		return mcpgo.NewToolResultError("agent_id and job_id must match [A-Za-z0-9._-]+"), nil
	}
	format := strings.ToLower(req.GetString("format", "text"))
	if format != "raw" && format != "text" {
		return mcpgo.NewToolResultErrorf("bad format %q (want 'raw' or 'text')", format), nil
	}
	head := 0
	if v := req.GetString("head", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return mcpgo.NewToolResultErrorf("bad head %q", v), nil
		}
		head = n
	}
	path := filepath.Join(s.transcriptsDir, agentID, jobID+".jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return mcpgo.NewToolResultErrorf("no transcript for %s/%s", agentID, jobID), nil
		}
		return mcpgo.NewToolResultErrorFromErr("read transcript", err), nil
	}
	totalBytes := len(body)
	totalEvents := bytes.Count(body, []byte{'\n'})

	if head > 0 {
		body = headNLines(body, head)
	}
	truncated := false
	if head == 0 && len(body) > transcriptBodyCap {
		body = body[:transcriptBodyCap]
		truncated = true
	}

	type response struct {
		AgentID     string `json:"agent_id"`
		JobID       string `json:"job_id"`
		Format      string `json:"format"`
		TotalBytes  int    `json:"total_bytes"`
		TotalEvents int    `json:"total_events"`
		Truncated   bool   `json:"truncated,omitempty"`
		Content     string `json:"content"`
	}
	resp := response{
		AgentID:     agentID,
		JobID:       jobID,
		Format:      format,
		TotalBytes:  totalBytes,
		TotalEvents: totalEvents,
		Truncated:   truncated,
	}
	if format == "raw" {
		resp.Content = string(body)
	} else {
		resp.Content = renderTranscriptText(body)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("marshal transcript", err), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

// renderTranscriptText converts the raw stream-json body into a flat
// "[role] text\n" rendering, pulling text blocks out of assistant
// messages and the bare 'result' string out of result events. Lines
// that fail to parse are skipped.
func renderTranscriptText(body []byte) string {
	type contentBlock struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Name  string `json:"name,omitempty"`
		Input any    `json:"input,omitempty"`
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
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e ev
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		switch e.Type {
		case "system":
			// Skip — usually just session init metadata.
		case "user", "assistant":
			role := e.Type
			for _, c := range e.Message.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						fmt.Fprintf(&out, "[%s] %s\n", role, c.Text)
					}
				case "tool_use":
					fmt.Fprintf(&out, "[%s tool_use] %s\n", role, c.Name)
				case "tool_result":
					if c.Text != "" {
						fmt.Fprintf(&out, "[%s tool_result] %s\n", role, c.Text)
					}
				}
			}
		case "result":
			if e.Result != "" {
				fmt.Fprintf(&out, "[result] %s\n", e.Result)
			}
		}
	}
	return out.String()
}

// headNLines returns the first n lines of body (each line including
// its trailing newline). When body has fewer than n newlines, returns
// the whole body unchanged.
func headNLines(body []byte, n int) []byte {
	if n <= 0 {
		return body
	}
	count := 0
	for i, c := range body {
		if c == '\n' {
			count++
			if count == n {
				return body[:i+1]
			}
		}
	}
	return body
}
