package team

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// DefaultLeaderBrief is the out-of-the-box system prompt for the Leader
// when the operator accepts the wizard defaults. It frames the leader as
// a delegator, not a doer, and explains how to use each default
// archetype.
const DefaultLeaderBrief = `You are leading a small team. Your job is to break work into pieces, delegate,
and integrate the results — not to do the work yourself. Default to dispatching
unless a task is genuinely 1-shot trivial.

How to use the archetypes:

- worker — Your hands. Spawn a worker for any concrete implementation,
  investigation, or debugging task. Give it a tight, self-contained brief
  (goal, constraints, files of interest, definition of done). Workers run
  in isolated git worktrees, so multiple can run in parallel on independent
  pieces. Don't spawn one worker for a sprawling task — split it first.

- reviewer — Your second pair of eyes. Spawn a reviewer after a worker
  reports "done" and before you declare anything shipped. Reviewers read
  diffs against main, flag correctness/design/security risks, and don't
  write code. Re-spawn a fresh reviewer per round; don't reuse one across
  unrelated changes.

- integrator — Your release hand. Spawn an integrator once review is clean.
  Integrators work ONLY on their own teem/integrator-<name> branch: they
  squash- or rebase-merge the worker branch into their own, run final
  checks (build, tests), and report done. They do NOT advance main. You
  fast-forward main yourself in the operator's primary worktree with
  "git merge --ff-only teem/integrator-<name>". If that ff fails,
  something diverged — investigate, never force. One integrator per
  merge train; never run two in parallel against the same branch.

Operating rules:
- Plan first, dispatch second. State the plan in chat before spawning.
- Each delegated brief must be self-contained — workers don't see this
  conversation.
- Always review before integrating. Never integrate your own worker's
  output without a reviewer in between.
- Keep your own messages concise; long-form thinking belongs in the
  briefs you hand to workers.
`

// IntegratorContract is the standing rule-of-engagement block every
// integrator sees in its base prompt and the leader is reminded of in
// its own. Phrasing is shared so the leader and the worker can't drift
// out of sync.
const IntegratorContract = `Integrator contract:
- Work happens only on your own branch (teem/integrator-<name>).
- Squash- or rebase-merge the target worker branch into your own.
- Run final checks (build, tests), commit, report done.
- DO NOT advance main. The leader fast-forwards main from your branch
  in the operator's primary worktree after reviewing your work.`

// IntegratorForbiddenOps lists git operations integrators (and any
// worker) must never run. Quoted verbatim into the integrator's base
// prompt; the leader is reminded of the list in summary form. The
// list exists because a previous integrator workaround
// (git update-ref refs/heads/main HEAD, after a failed `git checkout
// main` in a worktree that didn't own main) corrupted the operator's
// primary worktree and cost ~10 minutes to recover.
const IntegratorForbiddenOps = `Forbidden git operations (an integrator or worker must NEVER run these):
  - git update-ref refs/heads/main …          (writes the main ref directly)
  - git branch -f main …                      (force-moves the main branch)
  - git push -f origin main                   (force-pushes main upstream)
  - git push --force origin main              (same)
  - git push origin HEAD:main                 (non-current-branch push to main)
  - git push origin <sha>:main                (same; also <sha>:refs/heads/main)
  - git push origin +HEAD:refs/heads/main     (forced via "+" refspec, no -f flag)
  - git fetch . HEAD:refs/heads/main          (any fetch writing to refs/heads/main)
  - git fetch <remote> +<sha>:refs/heads/main (same; "+" refspec forces the write)
  - git symbolic-ref HEAD refs/heads/main     (redirecting HEAD into main)
  - git symbolic-ref refs/heads/main …        (redirecting main itself)
  - git checkout main --force                 (or git checkout -f main)
  - Any direct write to .git/refs/heads/main or .git/packed-refs
If you find yourself wanting main to be at a particular SHA, stop and
report it to the leader. The leader moves main from the primary
worktree; you do not.

The only ref you may move is refs/heads/teem/integrator-<your-name>.`

// DefaultArchetypes is the set of archetypes the wizard appends when the
// operator accepts the defaults. Roles are deliberately generic
// (worker/reviewer/integrator) so the same template covers most projects.
var DefaultArchetypes = []ArchetypeSpec{
	{
		Role:          "worker",
		Description:   "Implements features, fixes bugs, and investigates code. Runs in an isolated git worktree per instance so multiple workers can work in parallel without stepping on each other.",
		Placement:     "local",
		MaxConcurrent: 5,
	},
	{
		Role:          "reviewer",
		Description:   "Independent code reviewer. Reads diffs against main, flags correctness/design/security risks, does not write code. Spawn one per review round.",
		Placement:     "local",
		MaxConcurrent: 3,
	},
	{
		Role:          "integrator",
		Description:   "Merges reviewed worker branches into its own teem/integrator-<name> branch, resolves conflicts, runs final checks. The leader fast-forwards main from there. Never advances main directly. Run at most one at a time per merge train.",
		Placement:     "local",
		MaxConcurrent: 1,
	},
}

// maxClaudeMDBytes caps how much of a discovered CLAUDE.md we paste into
// the leader brief. Project briefs are read by the leader on every chat
// turn — keeping this bounded keeps token cost predictable.
const maxClaudeMDBytes = 16 * 1024

// BuildDefaultLeaderPrompt returns the default brief, optionally with a
// CLAUDE.md project-specifics section appended. claudeMD is treated as
// already-trimmed contents; if it exceeds maxClaudeMDBytes, it is
// truncated and a warning is emitted on stderr by the caller (the helper
// returns the truncated text so the caller can decide what to warn
// about).
func BuildDefaultLeaderPrompt(claudeMD string) string {
	claudeMD = strings.TrimSpace(claudeMD)
	if claudeMD == "" {
		return DefaultLeaderBrief
	}
	var b strings.Builder
	b.WriteString(DefaultLeaderBrief)
	if !strings.HasSuffix(DefaultLeaderBrief, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n--- Project specifics (from CLAUDE.md) ---\n")
	b.WriteString(claudeMD)
	if !strings.HasSuffix(claudeMD, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// FindClaudeMD probes ./CLAUDE.md then ./.claude/CLAUDE.md and returns
// the first match. Contents larger than maxClaudeMDBytes are truncated
// and a warning is written to stderr — the wizard is the only caller, so
// the side-channel is appropriate.
func FindClaudeMD() (path, contents string, ok bool) {
	for _, p := range []string{"CLAUDE.md", ".claude/CLAUDE.md"} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if len(data) > maxClaudeMDBytes {
			fmt.Fprintf(os.Stderr, "[teem] %s is %d bytes; only the first %d will be folded into the leader brief\n", p, len(data), maxClaudeMDBytes)
			// Trim back to the last valid UTF-8 rune boundary so the
			// brief never contains a half-rune from a multi-byte char
			// straddling the cap.
			end := maxClaudeMDBytes
			for end > 0 && !utf8.RuneStart(data[end]) {
				end--
			}
			data = data[:end]
		}
		return p, string(data), true
	}
	return "", "", false
}
