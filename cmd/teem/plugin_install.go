package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:plugin
var pluginFS embed.FS

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

// cleanupLegacyCommands removes slash-command files that earlier teem
// versions shipped but have since been retired. Idempotent and silent:
// missing files are not an error. Keep in sync with the embedded set in
// `plugin/commands/`.
func cleanupLegacyCommands() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	commandsDir := filepath.Join(home, ".claude", "commands")
	for _, name := range []string{"teem-team.md", "teem-spawn.md", "teem-audit.md"} {
		_ = os.Remove(filepath.Join(commandsDir, name))
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
	cleanupLegacyCommands()
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
		fmt.Println("Available slash commands in Claude Code: /teem-status")
	} else {
		fmt.Println("Teem commands + skill already installed under ~/.claude/.")
	}
	cleanupLegacyPluginDir()
	cleanupLegacyCommands()
	return nil
}
