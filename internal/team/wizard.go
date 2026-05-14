package team

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Wizard drives an interactive build of a Team from prompts written to W
// and answers read from R. Splitting prompts off the *os.Stdin/Stdout
// concrete types makes the flow testable.
type Wizard struct {
	R *bufio.Reader
	W io.Writer
	// ClaudeMDFinder probes for a project-level CLAUDE.md. Tests inject
	// a stub; production wiring defaults to FindClaudeMD which reads
	// ./CLAUDE.md then ./.claude/CLAUDE.md.
	ClaudeMDFinder func() (path, contents string, ok bool)
}

// NewWizard wires reader/writer and a default CLAUDE.md probe.
func NewWizard(r io.Reader, w io.Writer) *Wizard {
	return &Wizard{
		R:              bufio.NewReader(r),
		W:              w,
		ClaudeMDFinder: FindClaudeMD,
	}
}

// Run walks the operator through creating a team. Returns the assembled
// Team (validated) and the serialized YAML bytes ready to write to disk.
// Returns ErrCancelled if the operator declines the final write step.
func (z *Wizard) Run() (*Team, []byte, error) {
	z.println("teem init — let's build a team.")
	z.println("Answers in [brackets] are defaults. Hit enter to accept.")
	z.println("")

	name, err := z.askRequired("Team name", "")
	if err != nil {
		return nil, nil, err
	}

	// CLAUDE.md fold — silently skipped when neither file exists.
	var claudeMD string
	if z.ClaudeMDFinder != nil {
		if path, contents, ok := z.ClaudeMDFinder(); ok {
			z.printf("Detected %s (%s). Fold into the leader brief? ", path, humanSize(len(contents)))
			fold, err := z.askYesNo("", true)
			if err != nil {
				return nil, nil, err
			}
			if fold {
				claudeMD = contents
			}
		}
	}

	t := &Team{ID: NewID(), Name: name}

	// Default archetypes.
	z.println("")
	z.println("Default archetypes: worker, reviewer, integrator (all local).")
	useDefaults, err := z.askYesNo("Use these defaults?", true)
	if err != nil {
		return nil, nil, err
	}
	if useDefaults {
		t.Archetypes = append(t.Archetypes, cloneArchetypes(DefaultArchetypes)...)
	} else {
		z.println("")
		z.println("Declare your own archetypes — role templates the leader spawns from.")
		z.println("Each archetype has a max_concurrent cap; the leader decides how many to spawn.")
		z.println("Common roles: worker, reviewer, integrator, backend, frontend.")
		z.println("")
		for {
			add, err := z.askYesNo("Add an archetype?", true)
			if err != nil {
				return nil, nil, err
			}
			if !add {
				break
			}
			arch, err := z.askArchetype()
			if err != nil {
				return nil, nil, err
			}
			t.Archetypes = append(t.Archetypes, arch)
			z.printf("  added %s (up to %d, %s)\n\n", arch.Role, arch.MaxConcurrent, arch.Placement)
		}
	}

	// Leader brief.
	z.println("")
	defaultPrompt := BuildDefaultLeaderPrompt(claudeMD)
	customize, err := z.askYesNo("Customize the leader brief?", false)
	if err != nil {
		return nil, nil, err
	}
	if customize {
		z.println("Enter the leader brief. Hit enter on an empty line to accept the default below.")
		z.println(indent(defaultPrompt, "  | "))
		prompt, err := z.askMultiline()
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(prompt) == "" {
			prompt = defaultPrompt
		}
		t.Leader.SystemPrompt = prompt
	} else {
		t.Leader.SystemPrompt = defaultPrompt
	}

	if err := t.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate: %w", err)
	}

	body, err := t.MarshalYAML()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal: %w", err)
	}

	z.println("")
	z.println("Team summary:")
	z.println(indent(string(body), "  "))

	keep, err := z.askYesNo("Write this team?", true)
	if err != nil {
		return nil, nil, err
	}
	if !keep {
		return nil, nil, ErrCancelled
	}
	return t, body, nil
}

// ErrCancelled is returned when the operator declines the final write.
var ErrCancelled = fmt.Errorf("wizard: cancelled")

func cloneArchetypes(in []ArchetypeSpec) []ArchetypeSpec {
	out := make([]ArchetypeSpec, len(in))
	copy(out, in)
	return out
}

func humanSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}

func (z *Wizard) askArchetype() (ArchetypeSpec, error) {
	role, err := z.askRequired("  Role", "worker")
	if err != nil {
		return ArchetypeSpec{}, err
	}
	desc, err := z.ask("  Description (optional)", "")
	if err != nil {
		return ArchetypeSpec{}, err
	}

	placement, err := z.askChoice("  Where do instances run?", []string{
		"local  — same host as the leader, isolated git worktree per instance",
		"ssh    — remote host you reach via SSH",
		"fargate — ephemeral container on AWS ECS Fargate",
	}, 0)
	if err != nil {
		return ArchetypeSpec{}, err
	}

	a := ArchetypeSpec{Role: role, Description: desc}
	switch placement {
	case 0:
		a.Placement = "local"
	case 1:
		target, err := z.askRequired("    SSH target (user@host)", "")
		if err != nil {
			return ArchetypeSpec{}, err
		}
		a.Placement = "ssh:" + target
		wd, err := z.askRequired("    Working dir on remote host", "/home/"+leftOfAt(target)+"/teem-"+role)
		if err != nil {
			return ArchetypeSpec{}, err
		}
		a.WorkingDir = wd
	case 2:
		a.Placement = "fargate"
	}

	maxStr, err := z.askRequired("  Max concurrent instances", "3")
	if err != nil {
		return ArchetypeSpec{}, err
	}
	n, err := strconv.Atoi(maxStr)
	if err != nil || n <= 0 {
		return ArchetypeSpec{}, fmt.Errorf("max concurrent must be a positive integer (got %q)", maxStr)
	}
	a.MaxConcurrent = n

	return a, nil
}

func leftOfAt(s string) string {
	if i := strings.Index(s, "@"); i >= 0 {
		return s[:i]
	}
	return s
}

// --- low-level prompt helpers ---

func (z *Wizard) ask(label, def string) (string, error) {
	if def == "" {
		z.printf("%s: ", label)
	} else {
		z.printf("%s [%s]: ", label, def)
	}
	line, err := z.R.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (z *Wizard) askRequired(label, def string) (string, error) {
	for {
		v, err := z.ask(label, def)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
		z.println("  (required — please answer)")
	}
}

func (z *Wizard) askYesNo(label string, def bool) (bool, error) {
	yn := "Y/n"
	if !def {
		yn = "y/N"
	}
	for {
		if label == "" {
			z.printf("[%s]: ", yn)
		} else {
			z.printf("%s [%s]: ", label, yn)
		}
		line, err := z.R.ReadString('\n')
		if err != nil && line == "" {
			return false, err
		}
		s := strings.ToLower(strings.TrimSpace(line))
		switch s {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		z.println("  (please answer y or n)")
	}
}

func (z *Wizard) askChoice(label string, options []string, def int) (int, error) {
	z.printf("%s\n", label)
	for i, o := range options {
		z.printf("    %d) %s\n", i+1, o)
	}
	for {
		z.printf("  Choose [1-%d, default %d]: ", len(options), def+1)
		line, err := z.R.ReadString('\n')
		if err != nil && line == "" {
			return 0, err
		}
		s := strings.TrimSpace(line)
		if s == "" {
			return def, nil
		}
		n, err := strconv.Atoi(s)
		if err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		z.printf("  (please enter a number 1-%d)\n", len(options))
	}
}

// askMultiline reads lines until the operator hits a blank line. A
// trailing newline is preserved on the joined result so YAML's literal
// block scalar (|) renders cleanly.
func (z *Wizard) askMultiline() (string, error) {
	var lines []string
	for {
		z.printf("> ")
		line, err := z.R.ReadString('\n')
		if err != nil && line == "" {
			return strings.Join(lines, "\n"), err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func (z *Wizard) println(s string) { fmt.Fprintln(z.W, s) }

func (z *Wizard) printf(format string, args ...any) { fmt.Fprintf(z.W, format, args...) }

func indent(s, prefix string) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, p := range parts {
		parts[i] = prefix + p
	}
	return strings.Join(parts, "\n")
}

// Summary returns a one-screen description of an existing team, used by
// `teem init` when a YAML already exists.
func Summary(t *Team) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Team: %s\n", t.Name)
	if t.ID != "" {
		fmt.Fprintf(&b, "Team id: %s\n", t.ID)
	}
	first := strings.SplitN(strings.TrimSpace(t.Leader.SystemPrompt), "\n", 2)[0]
	if len(first) > 80 {
		first = first[:77] + "..."
	}
	fmt.Fprintf(&b, "Leader brief: %s\n", first)
	if len(t.Archetypes) == 0 {
		fmt.Fprintln(&b, "Archetypes: (none — leader only)")
		return b.String()
	}
	fmt.Fprintln(&b, "Archetypes:")
	for _, a := range t.Archetypes {
		lc := a.LifecycleOrDefault()
		fmt.Fprintf(&b, "  - %s (up to %d, %s, %s) — %s\n", a.Role, a.MaxConcurrent, a.Placement, lc, a.Description)
	}
	return b.String()
}
