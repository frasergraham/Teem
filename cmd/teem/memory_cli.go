package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/team"
)

// runMemory implements `teem memory <sub>`.
//
//	teem memory show   --role X [--team t]   print the role's full digest+entries
//	teem memory append --role X [--team t] "note text"
//	teem memory edit   --role X [--team t]   open the file in $EDITOR
//
// The CLI talks to the on-disk store directly — it does not require a
// running daemon. Role "leader" is accepted as the per-team leader
// memory; all other roles must match an archetype in the team YAML.
func runMemory(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: teem memory <show|append|edit> --role X [--team t] [text]")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("memory "+sub, flag.ExitOnError)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	role := fs.String("role", "", "role to operate on (e.g. worker, reviewer, leader)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *role == "" {
		return errors.New("--role is required")
	}
	if !archmem.IsValidRoleName(*role) {
		return fmt.Errorf("invalid role %q (must match ^[a-z][a-z0-9_-]*$)", *role)
	}

	resolved, err := resolveTeamPath(*teamPath)
	if err != nil {
		return err
	}
	t, err := team.Load(resolved)
	if err != nil {
		return err
	}
	if *role != archmem.LeaderRole && t.FindArchetypeByRole(*role) == nil {
		return fmt.Errorf("role %q is not in the team roster (and is not %q)", *role, archmem.LeaderRole)
	}

	memDir := defaultMemoryDir(t.Name)
	store := archmem.New(memDir, func() []string {
		archs := t.SnapshotArchetypes()
		out := make([]string, 0, len(archs))
		for _, a := range archs {
			out = append(out, a.Role)
		}
		return out
	})

	switch sub {
	case "show":
		return memoryShow(store, *role, os.Stdout)
	case "append":
		text := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if text == "" {
			return errors.New("append: missing note text (positional argument)")
		}
		return memoryAppend(store, *role, text)
	case "edit":
		return memoryEdit(store, memDir, *role)
	default:
		return fmt.Errorf("unknown memory subcommand %q (want show|append|edit)", sub)
	}
}

func memoryShow(store *archmem.Store, role string, w io.Writer) error {
	body, err := store.Load(role)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	if body == "" {
		fmt.Fprintf(w, "(no memory for role %q yet)\n", role)
		return nil
	}
	_, err = io.WriteString(w, body)
	return err
}

func memoryAppend(store *archmem.Store, role, text string) error {
	clean := strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
	entry := archmem.Entry{
		Timestamp: time.Now().UTC(),
		AgentID:   "operator",
		JobID:     "",
		Status:    "note",
		Summary:   clean,
	}
	if err := store.AppendEntry(role, entry); err != nil {
		return fmt.Errorf("append: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[teem] appended operator note to %s memory (%s.md)\n", role, role)
	return nil
}

// memoryEdit opens the role's memory file in $VISUAL, falling back to
// $EDITOR, then `vi` — matching the convention used by git, less, etc.
// Creates the file with default frontmatter first if it doesn't exist
// so the editor opens a meaningful skeleton rather than an empty buffer.
func memoryEdit(store *archmem.Store, dir, role string) error {
	path := filepath.Join(dir, role+".md")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := store.Rewrite(role, archmem.Frontmatter{Role: role}, "", nil); err != nil {
			return fmt.Errorf("init memory file: %w", err)
		}
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
