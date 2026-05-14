package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	mcpsrv "github.com/frasergraham/teem/internal/mcp"
)

// dashboardBranch is the per-branch row rendered under "Active branches"
// on the per-team detail page. AgentLive distinguishes branches whose
// owner is still in the registry (clickable jobs link) from orphans
// left over after a worker stops — the operator typically wants to see
// orphans precisely so they can clean them up.
type dashboardBranch struct {
	Name     string
	SHA      string
	AgeAgo   string
	Subject  string
	AgentID  string
	Live     bool
	JobsURL  string
}

// listTeemBranches enumerates refs/heads/teem/* in repoRoot and maps
// each to its current agent registry entry (when present). teamID is
// the canonical routing key used to build per-agent jobs links.
//
// Errors are intentionally swallowed → empty slice. A dashboard page
// must never 500 because the working tree has gone missing or git is
// unavailable; a one-line note to stderr is enough.
func listTeemBranches(repoRoot string, reg *mcpsrv.Registry, teamID string) []dashboardBranch {
	if repoRoot == "" {
		return nil
	}
	cmd := exec.Command(
		"git", "-C", repoRoot, "for-each-ref",
		"--format=%(refname:short)|%(objectname:short)|%(committerdate:unix)|%(contents:subject)",
		"refs/heads/teem/",
	)
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] list-branches %s: %v\n", repoRoot, err)
		return nil
	}

	type row struct {
		b     dashboardBranch
		stamp int64
	}
	var collected []row
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		// Split on the first 3 separators only — subjects can contain
		// "|" perfectly legally.
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		name, sha, tsStr, subject := parts[0], parts[1], parts[2], parts[3]
		if !strings.HasPrefix(name, "teem/") {
			continue
		}
		agentID := strings.TrimPrefix(name, "teem/")
		if agentID == "" {
			continue
		}
		b := dashboardBranch{
			Name:    name,
			SHA:     sha,
			Subject: truncateSubject(subject, 80),
			AgentID: agentID,
		}
		var stamp int64
		if secs, err := strconv.ParseInt(tsStr, 10, 64); err == nil && secs > 0 {
			stamp = secs
			b.AgeAgo = agoShort(time.Unix(secs, 0))
		}
		if _, ok := reg.Get(agentID); ok {
			b.Live = true
			b.JobsURL = fmt.Sprintf("/teams/%s/agents/%s/jobs", teamID, agentID)
		}
		collected = append(collected, row{b: b, stamp: stamp})
	}
	// Newest commit first; the operator usually wants to see in-flight
	// work at the top, parked work below.
	sort.Slice(collected, func(i, j int) bool { return collected[i].stamp > collected[j].stamp })
	out2 := make([]dashboardBranch, len(collected))
	for i, c := range collected {
		out2[i] = c.b
	}
	return out2
}

// truncateSubject clamps a commit subject to a single-line preview
// suitable for a table cell. UTF-8 safe — we back up to a rune
// boundary before tacking on the ellipsis.
func truncateSubject(s string, max int) string {
	if len(s) <= max {
		return s
	}
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}
