package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

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

	// teem init owns onboarding — that includes the Claude Code plugin
	// (slash commands + orchestration skill). Idempotent: noop if
	// already installed.
	if err := installPluginForInit(); err != nil {
		// Plugin install is best-effort; print but don't abort. The user
		// can still chat without it (Claude Code can use the MCP tools
		// directly; they just won't have the skill or slash commands).
		fmt.Fprintf(os.Stderr, "[teem] plugin install failed: %v\n", err)
	}

	if existing := findExistingTeamPath(); existing != "" {
		t, err := team.Load(existing)
		if err != nil {
			return fmt.Errorf("found %s but it failed to load: %w", existing, err)
		}
		fmt.Printf("Team file: %s\n\n", existing)
		fmt.Print(team.Summary(t))
		fmt.Println()
		maybeOfferDaemonStart(t)
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
	written, err := team.Load(*out)
	if err != nil {
		return fmt.Errorf("wrote %s but it fails to load: %w", *out, err)
	}

	fmt.Printf("\nWrote %s.\n", *out)
	maybeOfferDaemonStart(written)
	fmt.Println("Run `teem chat` to talk to the leader.")
	return nil
}

// maybeOfferDaemonStart asks the operator whether to start the
// orchestrator daemon now. When stdin EOFs immediately (scripted init,
// `</dev/null`) the prompt is treated as "no" so onboarding doesn't
// surprise-start a daemon in CI.
func maybeOfferDaemonStart(_ *team.Team) {
	if _, alive := readDaemonPID(); alive {
		fmt.Println("Daemon is already running.")
		return
	}
	fmt.Printf("Start the orchestrator daemon now? [Y/n]: ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		// EOF with no input — assume non-interactive. Don't auto-start.
		fmt.Println("(no input; skipped — run `teem start` when you're ready)")
		return
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans != "" && ans != "y" && ans != "yes" {
		fmt.Println("Skipped. Start it later with `teem start`.")
		return
	}
	df := &daemonFlags{useTailnet: true, listenAddr: ":7777"}
	if err := forkDetached(df); err != nil {
		fmt.Fprintf(os.Stderr, "daemon start failed: %v\n", err)
	}
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
