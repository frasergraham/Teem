package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// planFileSizeCap bounds how much of a plan-artifact markdown file the
// dashboard reads from a worker branch and feeds into goldmark. Larger
// files are truncated to the cap with a "... truncated" note so the
// rendered HTML still closes its tags cleanly and the card stays a
// reasonable size in the operator's browser.
const planFileSizeCap = 50 * 1024

// planFile is one file changed on a worker's branch as surfaced under
// an awaiting-approval card's "Plan artifact" header.
//
// Rendered carries the goldmark-rendered HTML for IsMarkdown files
// read off the worker's branch (via `git show`); Truncated is true
// when the source exceeded planFileSizeCap and was clipped before
// rendering. PathSlug is a DOM-safe id token derived from Path so each
// expanded <details> persists independently across the dashboard's
// 10s auto-refresh.
type planFile struct {
	Path       string `json:"path"`
	IsMarkdown bool   `json:"is_markdown"`
	PathSlug   string `json:"path_slug"` // "docs/foo.md" → "docs-foo-md"; used as a <details> id only
	Rendered   string `json:"rendered"`  // pre-rendered (sanitized) markdown body, blank if read/render failed
	Truncated  bool   `json:"truncated"` // true if source was clipped at planFileSizeCap
}

// awaitingApprovalEvidence is the per-evidence-job view rendered
// inside an awaiting-approval card: the originating job (clickable),
// the worker that produced it, the worker's branch (clickable), and
// the files the branch touched relative to main. PlanShaped is true
// when every changed file is a markdown file under docs/ — i.e. the
// branch is clearly a design-doc artifact, not a code change.
type awaitingApprovalEvidence struct {
	JobID      string     `json:"job_id"`
	AgentID    string     `json:"agent_id"`
	BranchRef  string     `json:"branch_ref"` // "teem/worker-una"
	BranchURL  string     `json:"branch_url"` // /teams/<id>/agents/<agent>/jobs
	JobURL     string     `json:"job_url"`    // /teams/<id>/jobs/<jobid>
	PlanFiles  []planFile `json:"plan_files"`
	PlanShaped bool       `json:"plan_shaped"`
}

// resolveEvidenceRows turns a task's []job_id evidence list into the
// richer per-row data the dashboard card needs:
//
//  1. job_id → agent_id via the audit event stream (first event with
//     a non-empty AgentID wins).
//  2. agent_id → branch ref "teem/<agent_id>" and the team-scoped
//     URLs to the agent-jobs and job-detail pages.
//  3. when repoRoot is set, branch → changed files relative to main.
//     A docs/-only-markdown changeset is flagged PlanShaped, and each
//     markdown file has its content read off the branch (via `git
//     show <ref>:<path>`) and rendered to HTML for inline display.
//
// Failure modes are intentionally swallowed: a missing audit row
// produces a row with an empty AgentID; a git failure (no branch,
// no repo, git missing) leaves PlanFiles empty; a per-file `git
// show` failure leaves Rendered blank. The dashboard must never 500
// because git or audit had a bad day.
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
			JobID: jobID,
		}
		if a, ok := agentForJob[jobID]; ok && isSafeID(a) {
			ev.AgentID = a
			ev.BranchRef = "teem/" + a
			if repoRoot != "" {
				if files, ok := changedFilesOnBranch(repoRoot, ev.BranchRef); ok {
					for i := range files {
						files[i].PathSlug = pathToSlug(files[i].Path)
						if files[i].IsMarkdown {
							rendered, truncated, ok := renderBranchMarkdown(repoRoot, ev.BranchRef, files[i].Path)
							if ok {
								files[i].Rendered = rendered
								files[i].Truncated = truncated
							}
						}
					}
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

// renderBranchMarkdown reads path off branchRef via `git show
// <branchRef>:<path>` (no checkout needed) and renders the content as
// HTML using goldmark. The output is sanitized — raw HTML in the
// source is escaped, not passed through, because the markdown comes
// from a worker-controlled branch we don't fully trust.
//
// Files larger than planFileSizeCap are truncated to the cap with a
// trailing "[truncated — N more bytes…]" note so the rendered HTML
// still closes its tags cleanly. Returns ok=false on read or render
// failure so the caller can skip the file without breaking the card.
func renderBranchMarkdown(repoRoot, branchRef, path string) (string, bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"git", "-C", repoRoot, "show", branchRef+":"+path)
	raw, err := cmd.Output()
	if err != nil {
		return "", false, false
	}
	truncated := false
	if len(raw) > planFileSizeCap {
		remaining := len(raw) - planFileSizeCap
		raw = append([]byte{}, raw[:planFileSizeCap]...)
		raw = append(raw, []byte(fmt.Sprintf("\n\n... [truncated — %d more bytes; open the file to read in full]\n", remaining))...)
		truncated = true
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(html.WithXHTML()),
		// WithUnsafe is explicitly NOT set — goldmark escapes raw HTML
		// by default, which is what we want when the source comes from
		// a worker branch.
	)
	var buf bytes.Buffer
	if err := md.Convert(raw, &buf); err != nil {
		return "", false, false
	}
	return buf.String(), truncated, true
}

// pathToSlug converts a file path into a DOM-safe id fragment: lower-
// case, with `/` and `.` collapsed into single `-`. The output is used
// only as a <details id> for the t-be1d96c5 sessionStorage script and
// doesn't need to be reversible. e.g. "docs/usage-monitor.md" →
// "docs-usage-monitor-md".
func pathToSlug(p string) string {
	p = strings.ToLower(p)
	var b strings.Builder
	b.Grow(len(p))
	lastDash := false
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' && !lastDash:
			b.WriteRune('-')
			lastDash = true
		case (r == '/' || r == '.' || r == '_' || r == ' ') && !lastDash:
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
