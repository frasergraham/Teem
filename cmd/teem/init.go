package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/frasergraham/teem/internal/team"
)

// runInit implements the `teem init` subcommand. If a team YAML already
// exists in the search chain, prints a summary and exits. Otherwise runs
// the interactive wizard and writes ./teem.yaml.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("out", teamSearchPaths[0], "where to write the team YAML when running the wizard")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if existing := findExistingTeamPath(); existing != "" {
		t, err := team.Load(existing)
		if err != nil {
			return fmt.Errorf("found %s but it failed to load: %w", existing, err)
		}
		fmt.Printf("Team file: %s\n\n", existing)
		fmt.Print(team.Summary(t))
		fmt.Println()
		fmt.Println("To run: teem chat --team", existing)
		fmt.Println("Edit that file directly to change the team — `teem init` is for first-time setup.")
		return nil
	}

	z := team.NewWizard(os.Stdin, os.Stdout)
	_, body, err := z.Run()
	if err != nil {
		if errors.Is(err, team.ErrCancelled) {
			fmt.Println("Cancelled. Nothing written.")
			return nil
		}
		return err
	}

	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite", *out)
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}

	// Reload to verify the file we just wrote is loadable. Cheap and
	// catches any drift between the wizard's in-memory Team and the
	// YAML's round-trip.
	if _, err := team.Load(*out); err != nil {
		return fmt.Errorf("wrote %s but it fails to load: %w", *out, err)
	}

	fmt.Printf("\nWrote %s. Run `teem chat` to start.\n", *out)
	return nil
}

// findExistingTeamPath returns the first path in teamSearchPaths that
// already exists on disk, or "" if none do. Mirrors resolveTeamPath but
// without the user-facing error.
func findExistingTeamPath() string {
	for _, p := range teamSearchPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
