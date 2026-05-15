package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
)

// planFile is one file changed on a worker's branch as surfaced under
// an awaiting-approval card's "Plan artifact" header. IsMarkdown is
// pre-computed so the template doesn't need a string-helper for the
// CSS classname.
type planFile struct {
	Path       string
	IsMarkdown bool
}

// awaitingApprovalEvidence is the per-evidence-job view rendered
// inside an awaiting-approval card: the originating job (clickable),
// the worker that produced it, the worker's branch (clickable), and
// the files the branch touched relative to main. PlanShaped is true
// when every changed file is a markdown file under docs/ — i.e. the
// branch is clearly a design-doc artifact, not a code change.
type awaitingApprovalEvidence struct {
	JobID      string
	AgentID    string
	BranchRef  string // "teem/worker-una"
	BranchURL  string // /teams/<id>/agents/<agent>/jobs
	JobURL     string // /teams/<id>/jobs/<jobid>
	PlanFiles  []planFile
	PlanShaped bool
}

// resolveEvidenceRows turns a task's []job_id evidence list into the
// richer per-row data the dashboard card needs:
//
//  1. job_id → agent_id via the audit event stream (first event with
//     a non-empty AgentID wins).
//  2. agent_id → branch ref "teem/<agent_id>" and the team-scoped
//     URLs to the agent-jobs and job-detail pages.
//  3. when repoRoot is set, branch → changed files relative to main.
//     A docs/-only-markdown changeset is flagged PlanShaped.
//
// Failure modes are intentionally swallowed: a missing audit row
// produces a row with an empty AgentID; a git failure (no branch,
// no repo, git missing) leaves PlanFiles empty. The dashboard must
// never 500 because git or audit had a bad day.
func resolveEvidenceRows(events []audit.Event, jobIDs []string, repoRoot, teamID string) []awaitingApprovalEvidence {
	if len(jobIDs) == 0 {
		return nil
	}
	agentForJob := map[string]string{}
	for _, e := range events {
		if e.JobID == "" || e.AgentID == "" {
			continue
		}
		if _, set := agentForJob[e.JobID]; !set {
			agentForJob[e.JobID] = e.AgentID
		}
	}
	out := make([]awaitingApprovalEvidence, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		ev := awaitingApprovalEvidence{
			JobID:  jobID,
			JobURL: fmt.Sprintf("/teams/%s/jobs/%s", teamID, jobID),
		}
		if a, ok := agentForJob[jobID]; ok && isSafeID(a) {
			ev.AgentID = a
			ev.BranchRef = "teem/" + a
			ev.BranchURL = fmt.Sprintf("/teams/%s/agents/%s/jobs", teamID, a)
			if repoRoot != "" {
				if files, ok := changedFilesOnBranch(repoRoot, ev.BranchRef); ok {
					ev.PlanFiles = files
					ev.PlanShaped = isPlanShaped(files)
				}
			}
		}
		out = append(out, ev)
	}
	return out
}

// changedFilesOnBranch lists files changed on branchRef relative to
// the merge base with main (three-dot diff). Returns ok=false when
// git fails — no repo, no branch, git not installed — so the caller
// renders a degraded card instead of nothing.
func changedFilesOnBranch(repoRoot, branchRef string) ([]planFile, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"git", "-C", repoRoot, "diff", "--name-only", "main..."+branchRef)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	files := make([]planFile, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		files = append(files, planFile{
			Path:       line,
			IsMarkdown: strings.HasSuffix(strings.ToLower(line), ".md"),
		})
	}
	return files, true
}

// isPlanShaped returns true when every changed file is a markdown
// file under docs/. A branch with zero changes is *not* plan-shaped
// — there's nothing to surface as the work product.
func isPlanShaped(files []planFile) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "docs/") || !f.IsMarkdown {
			return false
		}
	}
	return true
}
