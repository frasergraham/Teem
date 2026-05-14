package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/frasergraham/teem/internal/prompts"
	"github.com/frasergraham/teem/internal/team"
)

// runPrompt implements the `teem prompt` subcommand. It dispatches to
// show/append/edit on the role's prompt assembly. The "role" arg
// accepts "leader" or any archetype role name.
//
//	teem prompt show --role leader
//	teem prompt show --role worker --raw
//	teem prompt append --role worker "Always run go vet before commit"
//	teem prompt edit --role reviewer
func runPrompt(args []string) error {
	if len(args) == 0 {
		return errors.New("teem prompt <show|append|edit> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		return runPromptShow(rest)
	case "append":
		return runPromptAppend(rest)
	case "edit":
		return runPromptEdit(rest)
	case "-h", "--help":
		fmt.Println(promptUsage())
		return nil
	default:
		return fmt.Errorf("unknown prompt subcommand %q\n\n%s", sub, promptUsage())
	}
}

func promptUsage() string {
	return strings.TrimSpace(`
teem prompt — view, append to, and edit the leader / archetype prompt overrides

Usage:
  teem prompt show   [--role leader|<role>] [--team yaml] [--raw]
  teem prompt append --role <leader|role> [--team yaml] "<text>"
  teem prompt edit   --role <leader|role> [--team yaml]

Overrides live at ~/.teem/state/<team-slug>/prompt-overrides/<role>.md.
`)
}

// loadTeamForPrompt resolves the team YAML and returns (team, builder).
func loadTeamForPrompt(teamPath string) (*team.Team, *prompts.Builder, error) {
	resolved, err := resolveTeamPath(teamPath)
	if err != nil {
		return nil, nil, err
	}
	t, err := team.Load(resolved)
	if err != nil {
		return nil, nil, err
	}
	b := prompts.New(t, defaultPromptOverrideDir(t.Name))
	return t, b, nil
}

func runPromptShow(args []string) error {
	fs := flag.NewFlagSet("prompt show", flag.ExitOnError)
	role := fs.String("role", prompts.LeaderRole, `"leader" or an archetype role`)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	raw := fs.Bool("raw", false, "print only the override layer (no YAML-derived base)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, b, err := loadTeamForPrompt(*teamPath)
	if err != nil {
		return err
	}
	if err := validateRoleAgainstTeam(t, *role); err != nil {
		return err
	}
	if *raw {
		body, _, err := b.Override(*role)
		if err != nil {
			return err
		}
		if body == "" {
			fmt.Fprintf(os.Stderr, "(no override file at %s)\n", b.OverridePath(*role))
			return nil
		}
		_, err = io.WriteString(os.Stdout, body)
		if err == nil && !strings.HasSuffix(body, "\n") {
			fmt.Println()
		}
		return err
	}
	var assembled string
	if *role == prompts.LeaderRole {
		assembled = b.Leader()
	} else {
		got, ok := b.Archetype(*role)
		if !ok {
			return fmt.Errorf("role %q is not declared in team %q (see team YAML archetypes)", *role, t.Name)
		}
		assembled = got
	}
	_, err = io.WriteString(os.Stdout, assembled)
	if err == nil && !strings.HasSuffix(assembled, "\n") {
		fmt.Println()
	}
	return err
}

func runPromptAppend(args []string) error {
	fs := flag.NewFlagSet("prompt append", flag.ExitOnError)
	role := fs.String("role", "", `"leader" or an archetype role (required)`)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *role == "" {
		return errors.New("--role is required")
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return errors.New("append text is required as positional arg(s)")
	}
	t, b, err := loadTeamForPrompt(*teamPath)
	if err != nil {
		return err
	}
	if err := validateRoleAgainstTeam(t, *role); err != nil {
		return err
	}
	if err := b.AppendOverride(*role, text); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[teem] appended to %s\n", b.OverridePath(*role))
	fmt.Fprintln(os.Stderr, "[teem] note: takes effect on next 'teem chat --new-session' (a running leader session keeps its existing prompt until exited)")
	return nil
}

func runPromptEdit(args []string) error {
	fs := flag.NewFlagSet("prompt edit", flag.ExitOnError)
	role := fs.String("role", "", `"leader" or an archetype role (required)`)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *role == "" {
		return errors.New("--role is required")
	}
	t, b, err := loadTeamForPrompt(*teamPath)
	if err != nil {
		return err
	}
	if err := validateRoleAgainstTeam(t, *role); err != nil {
		return err
	}
	path := b.OverridePath(*role)
	if err := os.MkdirAll(b.OverrideDir(), 0o700); err != nil {
		return fmt.Errorf("mkdir override dir: %w", err)
	}
	// Touch the file so the editor opens something — even an empty
	// new override is a fine starting state.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			return fmt.Errorf("create override file: %w", err)
		}
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	resolved, err := exec.LookPath(editor)
	if err != nil {
		return fmt.Errorf("editor %q not found on PATH: %w", editor, err)
	}
	return syscall.Exec(resolved, []string{editor, path}, os.Environ())
}

// validateRoleAgainstTeam rejects role names outside the safe set and
// archetype roles that aren't in the team YAML. "leader" always passes.
func validateRoleAgainstTeam(t *team.Team, role string) error {
	if err := prompts.ValidateRole(role); err != nil {
		return fmt.Errorf("invalid role %q (allowed: leader or [a-z0-9_-]+)", role)
	}
	if role == prompts.LeaderRole {
		return nil
	}
	if t.FindArchetypeByRole(role) == nil {
		return fmt.Errorf("no archetype with role %q in team %q", role, t.Name)
	}
	return nil
}
