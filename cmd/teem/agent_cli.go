package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/prompts"
	"github.com/frasergraham/teem/internal/team"
)

// runAgent implements `teem agent <list|show|update>`. It unifies the
// previous `teem prompt` and `teem memory` commands behind a single
// archetype-centric dispatcher: every command takes an archetype name
// ("leader" or any role declared in the team YAML) and operates on its
// prompt override and/or memory markdown.
func runAgent(args []string) error {
	if len(args) == 0 {
		fmt.Println(agentUsage())
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runAgentList(rest)
	case "show":
		return runAgentShow(rest)
	case "update":
		return runAgentUpdate(rest)
	case "-h", "--help":
		fmt.Println(agentUsage())
		return nil
	default:
		return fmt.Errorf("unknown agent subcommand %q\n\n%s", sub, agentUsage())
	}
}

func agentUsage() string {
	return strings.TrimSpace(`
teem agent — inspect and edit per-archetype prompt overrides and memory

Usage:
  teem agent list                                              list archetypes incl. leader
  teem agent show   <archetype> [--prompt|--memory] [--team t] show prompt and/or memory
  teem agent update <archetype> [--prompt|--memory] [--team t] open the artefact in $EDITOR

<archetype> is "leader" or any role declared in the team YAML.
'show' with no flag prints both prompt and memory.
'update' with no flag defaults to --prompt; passing both is an error.
`)
}

// loadAgentDeps resolves the team YAML and returns the team plus the
// prompt Builder and memory Store rooted under the team's state dir.
func loadAgentDeps(teamPath string) (*team.Team, *prompts.Builder, *archmem.Store, error) {
	resolved, err := resolveTeamPath(teamPath)
	if err != nil {
		return nil, nil, nil, err
	}
	t, err := team.Load(resolved)
	if err != nil {
		return nil, nil, nil, err
	}
	b := prompts.New(t, defaultPromptOverrideDir(t.ID))
	store := archmem.New(defaultMemoryDir(t.ID), func() []string {
		archs := t.SnapshotArchetypes()
		out := make([]string, 0, len(archs))
		for _, a := range archs {
			out = append(out, a.Role)
		}
		return out
	})
	return t, b, store, nil
}

// validateArchetype rejects unsafe slugs and unknown archetype names.
// "leader" is always accepted; any other name must match a declared
// archetype in the team YAML.
func validateArchetype(t *team.Team, name string) error {
	if !archmem.IsValidRoleName(name) {
		return fmt.Errorf("invalid archetype %q (must match ^[a-z][a-z0-9_-]*$)", name)
	}
	if name == prompts.LeaderRole {
		return nil
	}
	if t.FindArchetypeByRole(name) == nil {
		valid := validArchetypeNames(t)
		return fmt.Errorf("unknown archetype %q — valid choices: %s", name, strings.Join(valid, ", "))
	}
	return nil
}

func validArchetypeNames(t *team.Team) []string {
	out := []string{prompts.LeaderRole}
	for _, a := range t.SnapshotArchetypes() {
		out = append(out, a.Role)
	}
	return out
}

func runAgentList(args []string) error {
	fs := flag.NewFlagSet("agent list", flag.ContinueOnError)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, b, store, err := loadAgentDeps(*teamPath)
	if err != nil {
		return err
	}

	type row struct{ name, source string }
	rows := []row{{prompts.LeaderRole, "leader"}}
	for _, a := range t.SnapshotArchetypes() {
		rows = append(rows, row{a.Role, "archetype"})
	}

	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSOURCE\tHAS_PROMPT_OVERRIDE\tHAS_MEMORY")
	for _, r := range rows {
		_, hasOverride, _ := b.Override(r.name)
		digest, entries, _ := store.LoadParsed(r.name)
		hasMemory := strings.TrimSpace(digest) != "" || len(entries) > 0
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.name, r.source, yesNo(hasOverride), yesNo(hasMemory))
	}
	return tw.Flush()
}

func runAgentShow(args []string) error {
	fs := flag.NewFlagSet("agent show", flag.ContinueOnError)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	showPrompt := fs.Bool("prompt", false, "show prompt only")
	showMemory := fs.Bool("memory", false, "show memory only")
	archetype, err := parsePositional(fs, args, "show")
	if err != nil {
		return err
	}
	t, b, store, err := loadAgentDeps(*teamPath)
	if err != nil {
		return err
	}
	if err := validateArchetype(t, archetype); err != nil {
		return err
	}
	// No flag → show both.
	wantPrompt := *showPrompt || (!*showPrompt && !*showMemory)
	wantMemory := *showMemory || (!*showPrompt && !*showMemory)

	if wantPrompt {
		if wantMemory {
			fmt.Println("=== prompt ===")
		}
		if err := printPrompt(b, archetype); err != nil {
			return err
		}
	}
	if wantMemory {
		if wantPrompt {
			fmt.Println()
			fmt.Println("=== memory ===")
		}
		if err := printMemory(store, archetype); err != nil {
			return err
		}
	}
	return nil
}

func runAgentUpdate(args []string) error {
	fs := flag.NewFlagSet("agent update", flag.ContinueOnError)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	editPrompt := fs.Bool("prompt", false, "edit the prompt override")
	editMemory := fs.Bool("memory", false, "edit the memory markdown")
	force := fs.Bool("force", false, "allow memory edits that drop the '# Recent entries' header")
	archetype, err := parsePositional(fs, args, "update")
	if err != nil {
		return err
	}
	if *editPrompt && *editMemory {
		return errors.New("--prompt and --memory are mutually exclusive — pass one")
	}
	if !*editPrompt && !*editMemory {
		*editPrompt = true
	}
	t, b, store, err := loadAgentDeps(*teamPath)
	if err != nil {
		return err
	}
	if err := validateArchetype(t, archetype); err != nil {
		return err
	}
	if *editPrompt {
		return editPromptOverride(b, archetype)
	}
	return editMemoryFile(store, t.ID, archetype, *force)
}

func printPrompt(b *prompts.Builder, role string) error {
	var assembled string
	if role == prompts.LeaderRole {
		assembled = b.Leader()
	} else {
		got, ok := b.Archetype(role)
		if !ok {
			return fmt.Errorf("archetype %q not declared in team", role)
		}
		assembled = got
	}
	if _, err := io.WriteString(os.Stdout, assembled); err != nil {
		return err
	}
	if !strings.HasSuffix(assembled, "\n") {
		fmt.Println()
	}
	return nil
}

func printMemory(store *archmem.Store, role string) error {
	body, err := store.Load(role)
	if err != nil {
		return fmt.Errorf("load memory: %w", err)
	}
	if strings.TrimSpace(body) == "" {
		fmt.Printf("(no memory for %q yet)\n", role)
		return nil
	}
	_, err = io.WriteString(os.Stdout, body)
	if err == nil && !strings.HasSuffix(body, "\n") {
		fmt.Println()
	}
	return err
}

// editPromptOverride opens the role's override file in $EDITOR via a
// temp file. On save+exit it diffs the buffer; unchanged → no-op,
// changed → atomic write through to the override path.
func editPromptOverride(b *prompts.Builder, role string) error {
	path := b.OverridePath(role)
	current, _, err := b.Override(role)
	if err != nil {
		return err
	}
	edited, changed, err := launchEditor(role, "prompt", current)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("no changes")
		return nil
	}
	if err := os.MkdirAll(b.OverrideDir(), 0o700); err != nil {
		return fmt.Errorf("mkdir override dir: %w", err)
	}
	if err := atomicWrite(path, []byte(edited)); err != nil {
		return fmt.Errorf("write override: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[teem] wrote prompt override for %s (%s)\n", role, path)
	fmt.Fprintln(os.Stderr, "[teem] note: takes effect on next 'teem chat --new-session' for the leader, or next spawn for workers")
	return nil
}

// editMemoryFile opens the role's memory markdown directly. We bypass
// the archmem parser on write so the operator can rearrange the file
// freely; the parser is tolerant enough that the next AppendEntry call
// will still work as long as the recent-entries header is intact. To
// catch accidental loss of that header (which would silently break
// future appends — parse returns no entries, then AppendEntry tacks new
// lines onto an unparseable file), we refuse a write that drops it
// unless --force is set.
func editMemoryFile(store *archmem.Store, teamID, role string, force bool) error {
	memDir := defaultMemoryDir(teamID)
	path := filepath.Join(memDir, role+".md")
	current, err := store.Load(role)
	if err != nil {
		return fmt.Errorf("load memory: %w", err)
	}
	if current == "" {
		// Seed an empty skeleton so the editor opens something useful.
		if err := store.Rewrite(role, archmem.Frontmatter{Role: role}, "", nil); err != nil {
			return fmt.Errorf("init memory: %w", err)
		}
		current, _ = store.Load(role)
	}
	edited, changed, err := launchEditor(role, "memory", current)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("no changes")
		return nil
	}
	if !force && strings.Contains(current, recentEntriesHeader) && !strings.Contains(edited, recentEntriesHeader) {
		return fmt.Errorf("memory file is missing required %q header — refusing to write; please restore the header or use --force", recentEntriesHeader)
	}
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		return fmt.Errorf("mkdir memory: %w", err)
	}
	if err := atomicWrite(path, []byte(edited)); err != nil {
		return fmt.Errorf("write memory: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[teem] wrote memory for %s (%s)\n", role, path)
	return nil
}

// recentEntriesHeader mirrors the archmem internal constant; we
// duplicate it here so a missing header (which would silently break the
// next AppendEntry) can be detected without exposing archmem internals.
const recentEntriesHeader = "# Recent entries"

// launchEditor seeds a temp file with `initial`, opens it in
// $VISUAL/$EDITOR/vi, and returns the edited bytes plus a changed flag.
// Multi-word editor commands like `code --wait` are shell-split so the
// flags reach the editor rather than being treated as part of the
// binary name. The temp file is removed on return.
func launchEditor(archetype, artefact, initial string) (string, bool, error) {
	pattern := fmt.Sprintf("teem-agent-%s-%s-*.md", archetype, artefact)
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", false, fmt.Errorf("temp file: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)
	if _, err := f.WriteString(initial); err != nil {
		_ = f.Close()
		return "", false, fmt.Errorf("seed temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", false, fmt.Errorf("close temp: %w", err)
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	parts, err := splitEditor(editor)
	if err != nil {
		return "", false, fmt.Errorf("parse %s=%q: %w", "EDITOR", editor, err)
	}
	if len(parts) == 0 {
		return "", false, fmt.Errorf("empty editor command")
	}
	parts = append(parts, tmpPath)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("editor %q: %w", editor, err)
	}
	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", false, fmt.Errorf("read edited: %w", err)
	}
	if bytes.Equal(edited, []byte(initial)) {
		return string(edited), false, nil
	}
	return string(edited), true, nil
}

// splitEditor shell-splits an $EDITOR/$VISUAL value into argv. Handles
// the common shapes (`vi`, `code --wait`, `emacs -nw`, `"path with
// space/editor" --flag`) without pulling in a shlex dependency. It's
// not a full POSIX shell parser — no globbing, no env expansion, no
// backslash inside double quotes for non-special chars — but it covers
// every EDITOR value a human would plausibly set.
func splitEditor(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		out = append(out, cur.String())
		cur.Reset()
	}
	hasField := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(s):
			cur.WriteByte(s[i+1])
			i++
			hasField = true
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			hasField = true
		case c == '"' && !inSingle:
			inDouble = !inDouble
			hasField = true
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			if hasField {
				flush()
				hasField = false
			}
		default:
			cur.WriteByte(c)
			hasField = true
		}
	}
	if inSingle || inDouble {
		return nil, errors.New("unbalanced quotes")
	}
	if hasField {
		flush()
	}
	return out, nil
}

// parsePositional parses `fs` against args while tolerating one
// positional argument (the archetype name) appearing anywhere in the
// sequence. Go's flag package stops at the first non-flag token, so we
// loop: parse → grab the next positional → resume parsing remaining
// args. The result is that `teem agent show worker --prompt`,
// `teem agent show --prompt worker`, and `teem agent show --team t.yaml
// worker --prompt` all parse identically.
//
// subcmd is the leaf subcommand name ("show", "update") used solely to
// shape the usage error message.
func parsePositional(fs *flag.FlagSet, args []string, subcmd string) (string, error) {
	usage := fmt.Sprintf("usage: teem agent %s <archetype> [--prompt|--memory]", subcmd)
	var positionals []string
	remaining := args
	for {
		if err := fs.Parse(remaining); err != nil {
			return "", err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		remaining = rest[1:]
	}
	if len(positionals) == 0 {
		return "", fmt.Errorf("%s: expected archetype name", usage)
	}
	if len(positionals) > 1 {
		return "", fmt.Errorf("%s: unexpected extra args: %s", usage, strings.Join(positionals[1:], " "))
	}
	return positionals[0], nil
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
