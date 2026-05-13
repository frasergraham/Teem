package team

import (
	"fmt"
	"os"
	"strings"
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

- integrator — Your release hand. Spawn an integrator once review is clean,
  to merge, resolve conflicts against main, run final checks, and push.
  One integrator per merge train; never run two in parallel against the
  same branch.

Operating rules:
- Plan first, dispatch second. State the plan in chat before spawning.
- Each delegated brief must be self-contained — workers don't see this
  conversation.
- Always review before integrating. Never integrate your own worker's
  output without a reviewer in between.
- Keep your own messages concise; long-form thinking belongs in the
  briefs you hand to workers.
`

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
		Description:   "Merges reviewed branches into main, resolves conflicts, runs final checks, pushes. Run at most one at a time per merge train.",
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
			data = data[:maxClaudeMDBytes]
		}
		return p, string(data), true
	}
	return "", "", false
}
