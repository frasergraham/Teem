package main

import (
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:plugin
var pluginFS embed.FS

// runInstallPlugin implements `teem install-plugin`. Writes the embedded
// plugin tree to ~/.claude/plugins/teem/. By default skips when the
// install already exists; --force overwrites.
func runInstallPlugin(args []string) error {
	fs := flag.NewFlagSet("install-plugin", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing install")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := pluginInstallPath()
	if err != nil {
		return err
	}
	installed, err := ensurePlugin(path, *force)
	if err != nil {
		return err
	}
	if installed {
		fmt.Printf("Installed teem plugin to %s\n", path)
	} else {
		fmt.Printf("Plugin already installed at %s (use --force to overwrite)\n", path)
	}
	return nil
}

// pluginInstallPath returns ~/.claude/plugins/teem.
func pluginInstallPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "plugins", "teem"), nil
}

// ensurePlugin makes sure the embedded plugin is installed at dest.
// Returns true when files were written. If force is false and the
// destination already exists, ensurePlugin does nothing — user
// modifications are preserved across teem upgrades. Use `teem
// install-plugin --force` to refresh.
func ensurePlugin(dest string, force bool) (bool, error) {
	if !force {
		if _, err := os.Stat(filepath.Join(dest, "plugin.json")); err == nil {
			return false, nil
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return false, err
	}
	err := pluginWalkAndWrite(dest)
	return err == nil, err
}

func pluginWalkAndWrite(dest string) error {
	return fs.WalkDir(pluginFS, "plugin", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "plugin")
		rel = strings.TrimPrefix(rel, "/")
		target := dest
		if rel != "" {
			target = filepath.Join(dest, rel)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := pluginFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

// quietEnsurePlugin is called from `teem chat` before exec'ing claude.
// First-run install only — silent no-op otherwise. We don't want to
// surprise an operator who customised their plugin.
func quietEnsurePlugin() {
	path, err := pluginInstallPath()
	if err != nil {
		return
	}
	installed, _ := ensurePlugin(path, false)
	if installed {
		fmt.Fprintf(os.Stderr, "[teem] installed plugin to %s\n", path)
	}
}

// installPluginForInit is the explicit-onboarding variant: announces the
// plugin path whether or not files were just written. Used by `teem
// init` so the user always sees where the plugin lives after onboarding.
func installPluginForInit() error {
	path, err := pluginInstallPath()
	if err != nil {
		return err
	}
	installed, err := ensurePlugin(path, false)
	if err != nil {
		return err
	}
	if installed {
		fmt.Printf("Installed Claude Code plugin to %s\n", path)
	} else {
		fmt.Printf("Plugin already at %s — use `teem install-plugin --force` to refresh.\n", path)
	}
	return nil
}

// pluginVersion reads the version from the embedded plugin.json. Used
// for diagnostics; not enforced anywhere yet.
func pluginVersion() string {
	body, err := pluginFS.ReadFile("plugin/plugin.json")
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.Version
}

// Errors that callers might want to introspect.
var (
	ErrPluginPathUnresolved = errors.New("plugin: could not resolve install path")
)
