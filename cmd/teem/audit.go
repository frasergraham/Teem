package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/team"
)

// runAudit implements the `teem audit` subcommand. It reads the leader's
// JSONL audit log and prints recent events, optionally filtering by agent
// or following the tail.
//
//	teem audit                 # last 50 events
//	teem audit --follow        # tail -f the log
//	teem audit --agent be-1    # filter by agent
//	teem audit --since 2026-05-01T00:00:00Z
//	teem audit --path /custom/audit.jsonl
func runAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	pathFlag := fs.String("path", "", "audit JSONL path (default derived from team YAML)")
	teamFlag := fs.String("team", "", "team YAML (used only to derive default audit path)")
	agent := fs.String("agent", "", "filter to events from this agent id")
	sinceStr := fs.String("since", "", "RFC3339 timestamp; only events at or after")
	limit := fs.Int("limit", 50, "max events to print")
	follow := fs.Bool("follow", false, "tail the log, blocking on new events")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *pathFlag
	if path == "" {
		teamPath, err := resolveTeamPath(*teamFlag)
		if err != nil {
			return fmt.Errorf("%w (or pass --path)", err)
		}
		t, err := team.Load(teamPath)
		if err != nil {
			return fmt.Errorf("load team: %w", err)
		}
		path = defaultAuditPath(t.Name)
	}

	var since time.Time
	if *sinceStr != "" {
		t, err := time.Parse(time.RFC3339, *sinceStr)
		if err != nil {
			return fmt.Errorf("bad --since: %w", err)
		}
		since = t
	}

	if *follow {
		return tailAudit(path, *agent, since)
	}
	return printAuditWindow(path, *agent, since, *limit)
}

func printAuditWindow(path, agent string, since time.Time, limit int) error {
	s := &audit.FileSink{}
	// We don't open for write — Query just reads. Use a wrapper that
	// honors the same query semantics via the public Query API.
	_ = s
	events, err := readJSONL(path)
	if err != nil {
		return err
	}
	out := filterEvents(events, agent, since)
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	for _, e := range out {
		printEvent(os.Stdout, e)
	}
	return nil
}

func tailAudit(path, agent string, since time.Time) error {
	// Print existing events first.
	events, err := readJSONL(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out := filterEvents(events, agent, since)
	for _, e := range out {
		printEvent(os.Stdout, e)
	}

	// Then tail. Open the file at the current end and re-read on
	// EAGAIN-style empty reads.
	f, err := os.Open(path)
	if err != nil {
		// File may not exist yet; poll until it does.
		for {
			time.Sleep(500 * time.Millisecond)
			f, err = os.Open(path)
			if err == nil {
				break
			}
		}
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	rdr := bufio.NewReader(f)
	for {
		line, err := rdr.ReadString('\n')
		if err == io.EOF {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		var e audit.Event
		if err := json.Unmarshal([]byte(strings.TrimRight(line, "\n")), &e); err != nil {
			continue
		}
		if (agent != "" && e.AgentID != agent) || (!since.IsZero() && e.Timestamp.Before(since)) {
			continue
		}
		printEvent(os.Stdout, e)
	}
}

func readJSONL(path string) ([]audit.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []audit.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e audit.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func filterEvents(events []audit.Event, agent string, since time.Time) []audit.Event {
	if agent == "" && since.IsZero() {
		return events
	}
	out := events[:0]
	for _, e := range events {
		if agent != "" && e.AgentID != agent {
			continue
		}
		if !since.IsZero() && e.Timestamp.Before(since) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// printEvent writes a single event in a compact human-friendly form. JSON
// is preserved for the meta block so callers can pipe through jq if they
// want structure.
func printEvent(w io.Writer, e audit.Event) {
	ts := e.Timestamp.UTC().Format(time.RFC3339)
	job := ""
	if e.JobID != "" {
		job = " job=" + e.JobID
	}
	meta := ""
	if len(e.Meta) > 0 {
		b, _ := json.Marshal(e.Meta)
		meta = " " + string(b)
	}
	msg := e.Message
	if msg != "" {
		msg = " " + msg
	}
	fmt.Fprintf(w, "%s [%s]%s %s%s%s\n", ts, e.AgentID, job, e.Kind, msg, meta)
}
