package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:plugin
var pluginFS embed.FS

// runInstallPlugin implements `teem install-plugin`. Writes the
// embedded slash commands and skill into the user's Claude Code config
// directory at ~/.claude/. No marketplace step required — both
// directories are auto-loaded by Claude Code on session start.
//
// Layout written on disk:
//
//	~/.claude/commands/teem-team.md         /teem-team
//	~/.claude/commands/teem-spawn.md        /teem-spawn
//	~/.claude/commands/teem-audit.md        /teem-audit
//	~/.claude/skills/teem-orchestration/SKILL.md
//
// Names are prefixed with `teem-` so they don't collide with built-in
// slash commands (e.g. /agents is reserved).
func runInstallPlugin(args []string) error {
	fs := flag.NewFlagSet("install-plugin", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite existing files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	written, err := installClaudeAssets(*force)
	if err != nil {
		return err
	}
	if written == 0 {
		fmt.Println("Commands and skill already installed in ~/.claude/. Use --force to refresh.")
	} else {
		fmt.Printf("Installed %d file(s) under ~/.claude/.\n", written)
		fmt.Println("Open Claude Code (via `teem chat`) and try: /teem-team")
	}
	// Tidy up the old plugin directory if a previous teem version put
	// files there — it's never been loaded.
	cleanupLegacyPluginDir()
	return nil
}

// installClaudeAssets writes the embedded commands + skill to
// ~/.claude/. Returns the number of files written. When force is false,
// existing files are skipped (operator edits survive).
func installClaudeAssets(force bool) (int, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	commandsDir := filepath.Join(home, ".claude", "commands")
	skillsDir := filepath.Join(home, ".claude", "skills")

	written := 0
	addFile := func(target string, body []byte) error {
		if !force {
			if _, err := os.Stat(target); err == nil {
				return nil
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
		written++
		return nil
	}

	walkErr := fs.WalkDir(pluginFS, "plugin", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "plugin"), "/")
		body, err := pluginFS.ReadFile(p)
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(rel, "commands/"):
			name := strings.TrimPrefix(rel, "commands/")
			return addFile(filepath.Join(commandsDir, name), body)
		case strings.HasPrefix(rel, "skills/"):
			suffix := strings.TrimPrefix(rel, "skills/")
			return addFile(filepath.Join(skillsDir, suffix), body)
		default:
			return nil
		}
	})
	return written, walkErr
}

// cleanupLegacyPluginDir removes ~/.claude/plugins/teem/ if it exists.
// Previous teem versions wrote files there expecting Claude Code to
// auto-load them, but plugins need an explicit `/plugin install` step
// that we never invoked. Leaving the directory around just confuses the
// user. Best-effort.
func cleanupLegacyPluginDir() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".claude", "plugins", "teem")
	if _, err := os.Stat(path); err == nil {
		_ = os.RemoveAll(path)
	}
}

// quietEnsurePlugin is called from `teem chat` before exec'ing claude.
// First-run install only — silent no-op afterwards. We don't want to
// surprise an operator who customised their commands.
func quietEnsurePlugin() {
	written, _ := installClaudeAssets(false)
	if written > 0 {
		fmt.Fprintf(os.Stderr, "[teem] installed %d teem command/skill file(s) under ~/.claude/\n", written)
	}
	cleanupLegacyPluginDir()
}

// installPluginForInit is the explicit-onboarding variant for `teem
// init`. Always prints a status line so the user sees where the files
// went.
func installPluginForInit() error {
	written, err := installClaudeAssets(false)
	if err != nil {
		return err
	}
	if written > 0 {
		fmt.Printf("Installed %d teem command/skill file(s) under ~/.claude/.\n", written)
		fmt.Println("Available slash commands in Claude Code: /teem-team, /teem-spawn, /teem-audit")
	} else {
		fmt.Println("Teem commands + skill already installed under ~/.claude/.")
		fmt.Println("Use `teem install-plugin --force` to refresh.")
	}
	cleanupLegacyPluginDir()
	return nil
}
