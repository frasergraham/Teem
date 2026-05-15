package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/pruner"
	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/team"
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

// runPruneBranches implements `teem prune-branches`. Dry-runs by
// default; --yes deletes merged branches; --force additionally allows
// `branch -D` on retired/orphan/unmerged rows and `worktree remove
// --force` on dirty worktrees.
//
// Operates on the team's git repo (resolved the same way runChat does)
// and the same on-disk roster the daemon writes. The daemon need not be
// running — the roster file is authoritative.
func runPruneBranches(args []string) error {
	fs := flag.NewFlagSet("prune-branches", flag.ExitOnError)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	yes := fs.Bool("yes", false, "actually delete; default is dry-run")
	force := fs.Bool("force", false, "with --yes: also delete retired/orphan/unmerged branches via `git branch -D` and force-remove dirty worktrees")
	verbose := fs.Bool("verbose", false, "log every refs/heads/teem/* branch filtered out by the strict regex")
	retiredAge := fs.Duration("retired-age", pruner.DefaultRetiredAge, "minimum age for in_use=false roster entries to be classified retired")
	mainRef := fs.String("main", "main", "main-branch ref to test merged-ness against")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolved, err := resolveTeamPath(*teamPath)
	if err != nil {
		return err
	}
	t, err := team.Load(resolved)
	if err != nil {
		return err
	}
	repoRoot, err := provisioner.ResolveRepoRoot("")
	if err != nil {
		return fmt.Errorf("repo root: %w", err)
	}

	// Live set: the daemon's in-memory registry isn't available from
	// this process, but the on-disk roster's InUse flag is — the
	// spawner maintains it via Allocate / ReserveNamed / Release /
	// MarkInUse and persists on every change. Treat InUse=true as
	// "currently spawned, don't touch" so the CLI is safe to run with
	// the daemon up.
	rost, err := roster.Open(defaultRosterPath(t.ID))
	if err != nil {
		return fmt.Errorf("roster: %w", err)
	}
	rosterEntries := rost.Snapshot()

	live := map[string]bool{}
	rosterView := make([]pruner.RosterView, 0, len(rosterEntries))
	for _, e := range rosterEntries {
		rosterView = append(rosterView, pruner.RosterView{
			AgentID: e.ID, InUse: e.InUse, LastUsedAt: e.LastUsedAt,
		})
		if e.InUse {
			live[e.ID] = true
		}
	}

	ctx := context.Background()
	var verboseLog func(format string, a ...any)
	if *verbose {
		verboseLog = func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "[teem prune] "+format+"\n", a...)
		}
	}
	branches, err := pruner.LoadCandidatesVerbose(ctx, repoRoot, *mainRef, verboseLog)
	if err != nil {
		return err
	}

	cls := pruner.Classify(pruner.Inputs{
		Branches:   branches,
		Roster:     rosterView,
		Live:       live,
		RetiredAge: *retiredAge,
		Force:      *force,
		Now:        time.Now(),
	})

	printClassificationTable(os.Stdout, cls)

	if !*yes {
		fmt.Println("\n(dry run — pass --yes to actually delete)")
		return nil
	}

	// Heads-up: an automated `--yes --force` run can drop unmerged
	// work. Print the warning before the destructive call (no
	// confirmation prompt — that breaks `--yes` automation).
	if *force {
		var unmerged int
		for _, c := range cls {
			if c.Action == pruner.ActionDelete && c.Reason != pruner.ReasonMerged {
				unmerged++
			}
		}
		if unmerged > 0 {
			fmt.Fprintf(os.Stderr, "WARNING: --force will delete %d unmerged branch(es) — proceeding\n", unmerged)
		}
	}

	res := pruner.Apply(ctx, cls, pruner.SweepOpts{
		RepoRoot:     repoRoot,
		WorktreeBase: defaultWorktreeBase(t.ID),
		Force:        *force,
		Logf: func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "[teem] "+format+"\n", a...)
		},
	})
	fmt.Printf("\ndeleted: %d  skipped: %d  errors: %d\n",
		len(res.Deleted), len(res.Skipped)+len(res.ForceSkipped)+len(res.DirtySkipped)+len(res.LiveSkipped), len(res.Errors))
	if n := len(res.ForceSkipped); n > 0 {
		fmt.Fprintf(os.Stderr, "  skipped %d unmerged-but-stale branch(es) (run with --force to remove)\n", n)
	}
	if n := len(res.DirtySkipped); n > 0 {
		fmt.Fprintf(os.Stderr, "  skipped %d branch(es) with dirty worktree (run with --force to discard)\n", n)
	}
	if n := len(res.LiveSkipped); n > 0 {
		fmt.Fprintf(os.Stderr, "  skipped %d branch(es) whose worker became live during the sweep\n", n)
	}
	if len(res.Errors) > 0 {
		for name, err := range res.Errors {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", name, err)
		}
	}
	return nil
}

func printClassificationTable(w *os.File, cls []pruner.Classification) {
	if len(cls) == 0 {
		fmt.Fprintln(w, "no teem/* branches found")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BRANCH\tREASON\tACTION\tMERGED")
	for _, c := range cls {
		merged := "no"
		if c.Merged {
			merged = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, c.Reason, c.Action, merged)
	}
	tw.Flush()
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
