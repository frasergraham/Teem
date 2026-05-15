// Package pruner classifies and removes stale `teem/*` branches.
//
// Spawning a worker creates a branch at `teem/<role>-<name>`; stopping
// the worker leaves it behind. Over time the operator accumulates
// dozens of dead branches that have already been integrated into main
// (or that no roster entry claims). The pruner sweeps them.
//
// Classification is pure (input → []Classification) so the daemon's
// periodic sweep, the `teem prune-branches` CLI, and tests share the
// same decision logic. Side-effects — running `git branch -D` and
// `git worktree remove` — live in Sweep, which is git-aware.
//
// Safety rules baked into BranchRE:
//   - Only `^teem/(worker|reviewer|integrator)-[a-z][a-z0-9]*$` is a
//     candidate. main, feature branches, and legacy `worker-3` shapes
//     are filtered out before classification runs.
package pruner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// BranchRE is the strict whitelist for teem-managed branch names. Any
// branch that doesn't match this is left alone — the pruner refuses to
// even classify it.
var BranchRE = regexp.MustCompile(`^teem/(worker|reviewer|integrator)-[a-z][a-z0-9]*$`)

// Reason is why a branch was classified the way it was. Used both in
// the dry-run table and the daemon's log lines.
type Reason string

const (
	// ReasonMerged: branch tip is reachable from main. Safe to drop.
	ReasonMerged Reason = "merged"
	// ReasonSquashMerged: branch tip is not an ancestor of main, but
	// every commit on the branch has a patch-id equivalent already on
	// main (typical squash-merge outcome). Action=delete without
	// --force; the patch-id check in isSquashMerged is the safety
	// gate. Apply uses `git branch -D` here because `branch -d`'s
	// ancestry check would reject the (already-shipped) work.
	ReasonSquashMerged Reason = "squash-merged"
	// ReasonRetired: roster says the agent is no longer in use and
	// hasn't been seen for at least RetiredAge.
	ReasonRetired Reason = "retired"
	// ReasonOrphan: no roster entry exists for the branch's agent id.
	ReasonOrphan Reason = "orphan"
	// ReasonLive: agent for this branch is currently spawned. Skip.
	ReasonLive Reason = "live"
	// ReasonUnmerged: branch isn't merged and isn't dead by any other
	// criterion. Skipped unless --force.
	ReasonUnmerged Reason = "unmerged"
)

// Action describes what Sweep would do for a Classification. "delete"
// branches get a `git branch -d` (merged) or `git branch -D` (force +
// retired/orphan/unmerged) + worktree removal; "skip" branches are
// reported and left alone.
type Action string

const (
	ActionDelete Action = "delete"
	ActionSkip   Action = "skip"
)

// SkipReason discriminates "skipped, action would have been delete"
// outcomes inside SweepResult. Empty when the row was simply Action=skip
// in the Classification.
type SkipReason string

const (
	// SkipNeedsForce: row's classification was retired/orphan/unmerged
	// and the caller did not pass force=true. Surfaced so the CLI can
	// print "skipped N unmerged-but-stale branches (run with --force to
	// remove)".
	SkipNeedsForce SkipReason = "would-force-delete-without-flag"
	// SkipDirtyWorktree: `git worktree remove` refused because the
	// worktree had uncommitted changes and force=false. Branch is left
	// alone too — yanking the branch would orphan the worker's work.
	SkipDirtyWorktree SkipReason = "worktree-dirty-without-force"
	// SkipLiveAtApply: row was Action=delete at Classify time but the
	// liveness recheck at Apply time saw the agent as live (sub-second
	// race: a worker was spawned between Classify and Apply).
	SkipLiveAtApply SkipReason = "live-at-apply-time"
	// SkipExternalWorktree: a worktree outside WorktreeBase is pinned
	// to the branch. For ReasonMerged this is always fatal (refuse and
	// let the operator decide); for the force-deletable family we
	// attempt `git worktree remove --force` first and only land here
	// when even the force-remove fails.
	SkipExternalWorktree SkipReason = "external-worktree-blocks"
)

// Classification is one row of the prune report.
type Classification struct {
	// Name is the local branch name, e.g. "teem/worker-ada".
	Name string
	// AgentID is the worker's canonical id, e.g. "worker-ada".
	AgentID string
	// Reason tags the classification bucket.
	Reason Reason
	// Action is whether Sweep will touch this branch.
	Action Action
	// Merged indicates the branch tip is reachable from main.
	Merged bool
}

// Branch is the input shape Classify consumes — one local branch
// matched by BranchRE plus the metadata git already gave us.
type Branch struct {
	Name    string // refs/heads/teem/worker-ada → "teem/worker-ada"
	AgentID string // "worker-ada"
	Merged  bool   // reachable from main
	// SquashMerged is true when Merged is false but every commit on
	// the branch has a patch-id equivalent on main — i.e. the work
	// was integrated via squash-merge. Same safety semantics as
	// Merged for deletion purposes.
	SquashMerged bool
}

// RosterView is the slice of roster.Entry data Classify needs. We don't
// import internal/roster so the package stays test-isolated (no I/O).
type RosterView struct {
	AgentID    string
	InUse      bool
	LastUsedAt time.Time
}

// Inputs is everything Classify needs to make a decision.
type Inputs struct {
	// Branches is the candidate set already filtered by BranchRE.
	Branches []Branch
	// Roster snapshot, keyed below by AgentID. Nil → every branch is
	// an "orphan" unless live or merged.
	Roster []RosterView
	// Live is the set of agent ids that are currently spawned (i.e.
	// must not be touched).
	Live map[string]bool
	// RetiredAge is the threshold for the "retired" classification: a
	// roster entry with InUse == false and LastUsedAt older than this
	// triggers ReasonRetired. Zero falls back to DefaultRetiredAge.
	RetiredAge time.Duration
	// Force lifts the "needs --force" guard. Without Force, retired,
	// orphan, and unmerged rows are Action=skip (any of those can
	// hide unmerged commits). With Force, they flip to Action=delete
	// and Apply will run `git branch -D`.
	Force bool
	// Now is the reference time for the retired-age comparison.
	Now time.Time
}

// DefaultRetiredAge is the cutoff Inputs.RetiredAge falls back to when
// the caller leaves it unset.
const DefaultRetiredAge = 7 * 24 * time.Hour

// Classify decides for every candidate branch whether it should be
// deleted and why. Pure: no git calls, no I/O. Live wins over every
// other reason — a currently-spawned worker is never touched, even if
// its branch happens to be merged.
func Classify(in Inputs) []Classification {
	retiredAge := in.RetiredAge
	if retiredAge <= 0 {
		retiredAge = DefaultRetiredAge
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	rosterByID := make(map[string]RosterView, len(in.Roster))
	for _, e := range in.Roster {
		rosterByID[e.AgentID] = e
	}
	out := make([]Classification, 0, len(in.Branches))
	for _, b := range in.Branches {
		c := Classification{
			Name:    b.Name,
			AgentID: b.AgentID,
			Merged:  b.Merged,
		}
		switch {
		case in.Live[b.AgentID]:
			c.Reason = ReasonLive
			c.Action = ActionSkip
		case b.Merged:
			c.Reason = ReasonMerged
			c.Action = ActionDelete
		case b.SquashMerged:
			c.Reason = ReasonSquashMerged
			c.Action = ActionDelete
		default:
			entry, ok := rosterByID[b.AgentID]
			switch {
			case !ok:
				c.Reason = ReasonOrphan
			case !entry.InUse && now.Sub(entry.LastUsedAt) >= retiredAge:
				c.Reason = ReasonRetired
			default:
				c.Reason = ReasonUnmerged
			}
			// Retired, orphan, and unmerged rows all carry unmerged
			// commits in the general case (only merged-branch tips are
			// reachable from main). They flip to Action=delete only
			// when the caller passed Force.
			if in.Force {
				c.Action = ActionDelete
			} else {
				c.Action = ActionSkip
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LoadCandidates lists local teem branches whose name matches BranchRE
// and resolves the merged-into-main flag for each. mainRef is typically
// "main" but the caller can override (e.g. "master") via the env knob.
func LoadCandidates(ctx context.Context, repoRoot, mainRef string) ([]Branch, error) {
	return LoadCandidatesVerbose(ctx, repoRoot, mainRef, nil)
}

// LoadCandidatesVerbose is LoadCandidates plus an optional logf hook
// that fires once per refs/heads/teem/* branch filtered out by
// BranchRE. Useful for migrating from the pre-canonicalisation naming
// convention — operators want to know *why* a branch they expected to
// see isn't in the table.
func LoadCandidatesVerbose(ctx context.Context, repoRoot, mainRef string, logf func(format string, args ...any)) ([]Branch, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("pruner: empty repoRoot")
	}
	if mainRef == "" {
		mainRef = "main"
	}
	cmd := exec.CommandContext(ctx,
		"git", "-C", repoRoot, "for-each-ref",
		"--format=%(refname:short)",
		"refs/heads/teem/",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git for-each-ref: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var branches []Branch
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if !BranchRE.MatchString(name) {
			if logf != nil {
				logf("skipped non-matching: %s (doesn't match strict pattern)", name)
			}
			continue
		}
		merged := isMerged(ctx, repoRoot, name, mainRef)
		squashMerged := false
		if !merged {
			squashMerged = isSquashMerged(ctx, repoRoot, name, mainRef)
		}
		branches = append(branches, Branch{
			Name:         name,
			AgentID:      strings.TrimPrefix(name, "teem/"),
			Merged:       merged,
			SquashMerged: squashMerged,
		})
	}
	return branches, nil
}

// isMerged returns true when branch's tip is reachable from mainRef.
// A missing mainRef (fresh repo, or "master" misconfig) returns false
// — we'd rather over-classify as unmerged than wrongly mark a branch
// merged.
func isMerged(ctx context.Context, repoRoot, branch, mainRef string) bool {
	// git merge-base --is-ancestor exits 0 iff branch ⊆ mainRef.
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "merge-base", "--is-ancestor", branch, mainRef)
	return cmd.Run() == nil
}

// isSquashMerged returns true iff every commit on branch has a
// patch-id equivalent already on mainRef — git's signal for "this
// work shipped via squash-merge." Implemented with `git cherry`:
// each line is "+ <sha>" (no equivalent on the other side) or
// "- <sha>" (equivalent found). All "-" → squash-merged; any "+" →
// not. Empty output means the branch has no commits ahead of main,
// which the caller has already handled via isMerged. False on any
// error — we'd rather miss a squash-merged branch (operator falls
// back to --force) than mis-classify a truly unmerged one as safe.
func isSquashMerged(ctx context.Context, repoRoot, branch, mainRef string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "cherry", mainRef, branch)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return false
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			return false
		}
	}
	return true
}

// SweepOpts is the side-effecting half of the pruner. Apply takes a
// Classification slice and runs the destructive git commands.
type SweepOpts struct {
	// RepoRoot is the git working tree the branches live in.
	RepoRoot string
	// WorktreeBase is the directory under which per-agent worktrees are
	// placed (~/.teem/worktrees/<team>/). Empty disables worktree
	// removal — only branches are touched.
	WorktreeBase string
	// Force, when true, allows `git branch -D` for non-merged rows
	// (retired, orphan, unmerged) and `git worktree remove --force`
	// for dirty worktrees. Merged rows always use `git branch -d` —
	// the safe variant — regardless of Force.
	Force bool
	// LiveCheck, when non-nil, is consulted immediately before each
	// branch deletion to catch the narrow race between Classify (which
	// snapshotted the live set N seconds ago) and Apply. Return true
	// to skip the row.
	LiveCheck func(agentID string) bool
	// Logf, when non-nil, is called once per deletion attempt.
	Logf func(format string, args ...any)
}

// SweepResult tallies what Apply actually did. Skipped rows are
// surfaced via the *Skip* slices (one row may appear in only one of
// them); ForceSkipped and DirtySkipped exist so the CLI can print a
// targeted "run with --force to remove" hint.
type SweepResult struct {
	Deleted                 []string
	Skipped                 []string
	ForceSkipped            []string
	DirtySkipped            []string
	LiveSkipped             []string
	ExternalWorktreeSkipped []string
	Errors                  map[string]error
}

// Apply runs Sweep over a Classification slice. Branches with
// Action=delete are removed (branch + worktree dir). Action=skip rows
// are left alone. Returns counts + per-branch errors; an error on one
// branch never aborts the rest.
func Apply(ctx context.Context, cls []Classification, opts SweepOpts) SweepResult {
	res := SweepResult{Errors: map[string]error{}}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	for _, c := range cls {
		if c.Action != ActionDelete {
			res.Skipped = append(res.Skipped, c.Name)
			continue
		}
		// Defence-in-depth: never delete anything that doesn't match
		// the strict regex, even if a caller hands us a hand-crafted
		// Classification.
		if !BranchRE.MatchString(c.Name) {
			res.Errors[c.Name] = fmt.Errorf("refusing to delete %q: does not match strict regex", c.Name)
			logf("pruner: refused %s (regex mismatch)", c.Name)
			continue
		}
		// Defence-in-depth: a hand-crafted Classification could pair
		// ActionDelete with a non-deleting reason. Only merged (safe)
		// and the unmerged-family-with-Force (explicit) reach git.
		switch c.Reason {
		case ReasonMerged:
			// safe: `git branch -d` refuses if not merged.
		case ReasonSquashMerged:
			// Uses `git branch -D` because `branch -d`'s safety
			// check is ancestry-based and would reject the branch
			// even though every patch is already on main. The
			// safety gate for this path is the patch-id check in
			// isSquashMerged — false positives there would drop
			// genuinely-unmerged work, so the bar is "all `git
			// cherry` lines start with `-`".
		case ReasonRetired, ReasonOrphan, ReasonUnmerged:
			if !opts.Force {
				res.ForceSkipped = append(res.ForceSkipped, c.Name)
				logf("pruner: %s would force-delete (%s); pass --force to proceed", c.Name, c.Reason)
				continue
			}
		default:
			res.Errors[c.Name] = fmt.Errorf("refusing to delete %q: unexpected reason %q for delete", c.Name, c.Reason)
			logf("pruner: refused %s (unexpected reason %s)", c.Name, c.Reason)
			continue
		}
		// Race window between Classify and Apply: a worker may have
		// been spawned in the interim. Recheck live immediately before
		// touching the branch.
		if opts.LiveCheck != nil && opts.LiveCheck(c.AgentID) {
			res.LiveSkipped = append(res.LiveSkipped, c.Name)
			logf("pruner: %s skipped: live at apply time", c.Name)
			continue
		}
		// Worktree first — removing the branch while a worktree is
		// checked out on it would fail. Try the safe variant first;
		// retry with --force only when the operator opted in.
		if opts.WorktreeBase != "" {
			worktreeDir := filepath.Join(opts.WorktreeBase, c.AgentID)
			if _, err := os.Stat(worktreeDir); err == nil {
				if err := runGit(ctx, opts.RepoRoot, "worktree", "remove", worktreeDir); err != nil {
					if !opts.Force {
						res.DirtySkipped = append(res.DirtySkipped, c.Name)
						logf("pruner: %s worktree remove refused (likely dirty): %v — pass --force", c.Name, err)
						continue
					}
					if err := runGit(ctx, opts.RepoRoot, "worktree", "remove", "--force", worktreeDir); err != nil {
						// Last resort: maybe the worktree was already
						// unregistered but the dir remains. Prune +
						// rm-rf so the branch delete can proceed.
						_ = runGit(ctx, opts.RepoRoot, "worktree", "prune")
						_ = os.RemoveAll(worktreeDir)
						logf("pruner: %s worktree remove --force failed: %v (continuing)", c.Name, err)
					}
				}
			}
		}
		// External worktrees — anything pinned to this branch from a
		// path outside WorktreeBase. Reviewers occasionally create
		// these (`git worktree add /tmp/foo teem/worker-X`) and never
		// clean them up; a subsequent `git branch -d/-D` then fails
		// with "branch is checked out at /tmp/foo". Detect + decide
		// before we get there. For ReasonMerged (safe path) any block
		// is fatal — the merge is fine, but the operator should
		// resolve the worktree first. For the force-deletable family
		// we try `git worktree remove --force` and only land in the
		// SkipExternalWorktree bucket when even that fails.
		if externals, err := externalWorktreesForBranch(ctx, opts.RepoRoot, c.Name, opts.WorktreeBase); err == nil && len(externals) > 0 {
			blocked := false
			for _, path := range externals {
				if c.Reason == ReasonMerged || c.Reason == ReasonSquashMerged {
					res.Errors[c.Name] = fmt.Errorf("worktree at %s blocks deletion (run 'git worktree remove %s' first)", path, path)
					logf("pruner: cannot delete %s — worktree at %s blocks deletion (run 'git worktree remove %s' first)", c.Name, path, path)
					blocked = true
					break
				}
				if err := runGit(ctx, opts.RepoRoot, "worktree", "remove", "--force", path); err != nil {
					res.ExternalWorktreeSkipped = append(res.ExternalWorktreeSkipped, c.Name)
					res.Errors[c.Name] = fmt.Errorf("force-remove external worktree %s: %w", path, err)
					logf("pruner: %s external worktree at %s — `git worktree remove --force` failed: %v", c.Name, path, err)
					blocked = true
					break
				}
				logf("pruner: %s force-removed external worktree %s", c.Name, path)
			}
			if blocked {
				continue
			}
		}
		flag := "-d"
		if c.Reason != ReasonMerged {
			// ReasonSquashMerged: -d would refuse (ancestry check
			// fails); the patch-id detection in isSquashMerged is
			// the safety gate, not `git branch -d`'s own check.
			// ReasonRetired/Orphan/Unmerged: gated by opts.Force.
			flag = "-D"
		}
		if err := runGit(ctx, opts.RepoRoot, "branch", flag, c.Name); err != nil {
			res.Errors[c.Name] = err
			logf("pruner: %s branch %s failed: %v", c.Name, flag, err)
			continue
		}
		res.Deleted = append(res.Deleted, c.Name)
		logf("pruned branch %s (%s)", c.Name, c.Reason)
	}
	return res
}

// IsMerged is the public entry point for the spawner's
// "delete-on-retire if merged" check. Same semantics as isMerged but
// exported so package agent can call it without copying the command.
func IsMerged(ctx context.Context, repoRoot, branch, mainRef string) bool {
	if mainRef == "" {
		mainRef = "main"
	}
	return isMerged(ctx, repoRoot, branch, mainRef)
}

// DeleteBranch removes a single local branch. Exposed for the
// spawner's auto-cleanup hook on a worker stop. Refuses non-matching
// names. force=false → `git branch -d` (safe: git refuses if not
// merged); force=true → `git branch -D` (drops unmerged commits).
func DeleteBranch(ctx context.Context, repoRoot, branch string, force bool) error {
	if !BranchRE.MatchString(branch) {
		return fmt.Errorf("refusing to delete %q: does not match strict regex", branch)
	}
	flag := "-d"
	if force {
		flag = "-D"
	}
	return runGit(ctx, repoRoot, "branch", flag, branch)
}

// externalWorktreesForBranch returns the on-disk paths of any
// worktrees registered in repoRoot pinned to `refs/heads/<branch>`
// whose location is OUTSIDE worktreeBase. The in-base case is
// already handled by Apply's existing per-agent stat+remove block —
// callers should not double-clean those. worktreeBase == "" means
// "the caller has no per-agent base, treat every pinned worktree as
// external" (safe: Apply will still try the safe-then-force
// progression on each).
func externalWorktreesForBranch(ctx context.Context, repoRoot, branch, worktreeBase string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git worktree list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	wantRef := "refs/heads/" + branch
	canonBase := ""
	if worktreeBase != "" {
		if real, err := filepath.EvalSymlinks(worktreeBase); err == nil {
			canonBase = real
		} else if abs, err := filepath.Abs(worktreeBase); err == nil {
			canonBase = abs
		} else {
			canonBase = worktreeBase
		}
	}
	var external []string
	var currentPath, currentBranch string
	flush := func() {
		if currentPath == "" || currentBranch != wantRef {
			return
		}
		canon := currentPath
		if real, err := filepath.EvalSymlinks(currentPath); err == nil {
			canon = real
		} else if abs, err := filepath.Abs(currentPath); err == nil {
			canon = abs
		}
		if canonBase != "" {
			rel, err := filepath.Rel(canonBase, canon)
			if err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
				// path lies inside worktreeBase — handled by caller's
				// per-agent block, not "external".
				return
			}
		}
		external = append(external, currentPath)
	}
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			currentPath = strings.TrimPrefix(line, "worktree ")
			currentBranch = ""
		case strings.HasPrefix(line, "branch "):
			currentBranch = strings.TrimPrefix(line, "branch ")
		case line == "":
			flush()
			currentPath = ""
			currentBranch = ""
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return external, nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
