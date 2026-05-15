package messaging

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the daemon-global messaging config loaded from
// ~/.teem/messaging.yaml. Zero value (Enabled=false) means messaging
// is off — the file may be absent and that's fine.
type Config struct {
	Enabled  bool           `yaml:"enabled"`
	Telegram TelegramConfig `yaml:"telegram"`
}

// TelegramConfig is the per-channel configuration for Telegram. The bot
// token itself never goes in the YAML — only the env var name to read it
// from, so the file is safe to check in or sync.
type TelegramConfig struct {
	Enabled          bool   `yaml:"enabled"`
	BotTokenEnv      string `yaml:"bot_token_env"`
	ChatID           int64  `yaml:"chat_id"`
	DashboardBaseURL string `yaml:"dashboard_base_url"`
	DedupWindowStr   string `yaml:"dedup_window"`
}

// fileShape is the on-disk YAML root: { messaging: { ... } }.
type fileShape struct {
	Messaging Config `yaml:"messaging"`
}

// DefaultDedupWindow is the dedup window used when telegram.dedup_window
// is empty or unparseable.
const DefaultDedupWindow = 10 * time.Minute

// DefaultBotTokenEnv is the env-var name assumed when telegram.bot_token_env
// is not set.
const DefaultBotTokenEnv = "TEEM_TELEGRAM_TOKEN"

// MessagingYAMLPath is the canonical config path inside the teem home.
func MessagingYAMLPath(home string) string {
	return filepath.Join(home, "messaging.yaml")
}

// Load reads <home>/messaging.yaml. Returns a zero Config (Enabled=false)
// if the file is absent — messaging is opt-in.
func Load(home string) (Config, error) {
	path := MessagingYAMLPath(home)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("messaging: read %s: %w", path, err)
	}
	var f fileShape
	if err := yaml.Unmarshal(body, &f); err != nil {
		return Config{}, fmt.Errorf("messaging: parse %s: %w", path, err)
	}
	return f.Messaging, nil
}

// DedupWindow returns the configured window or DefaultDedupWindow if
// the value is empty or unparseable.
func (c TelegramConfig) DedupWindow() time.Duration {
	if c.DedupWindowStr == "" {
		return DefaultDedupWindow
	}
	d, err := time.ParseDuration(c.DedupWindowStr)
	if err != nil || d <= 0 {
		return DefaultDedupWindow
	}
	return d
}

// Resolve materialises a Notifier from cfg + the process env. Returns
// (nil, nil, nil) when messaging is disabled. Returns an error when a
// sub-channel is enabled but its credentials are missing — the daemon
// refuses to start rather than ship pings to nowhere. The second return
// is the concrete *TelegramNotifier (when telegram is on) so callers
// that need chat-id-aware SendText (the inbound webhook) can reach it
// without unwrapping the Notifier interface.
func Resolve(cfg Config, env func(string) string) (Notifier, *TelegramNotifier, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}
	if env == nil {
		env = os.Getenv
	}

	var (
		ns MultiNotifier
		tn *TelegramNotifier
	)
	if cfg.Telegram.Enabled {
		tokenEnv := cfg.Telegram.BotTokenEnv
		if tokenEnv == "" {
			tokenEnv = DefaultBotTokenEnv
		}
		token := env(tokenEnv)
		if token == "" {
			return nil, nil, fmt.Errorf("messaging: telegram enabled but env var %s is empty (set %s=<bot-token> or disable telegram in %s)",
				tokenEnv, tokenEnv, "~/.teem/messaging.yaml")
		}
		if cfg.Telegram.ChatID == 0 {
			return nil, nil, fmt.Errorf("messaging: telegram enabled but chat_id is unset in ~/.teem/messaging.yaml")
		}
		tn = NewTelegramNotifier(token, cfg.Telegram.ChatID, nil)
		ns = append(ns, tn)
	}

	if len(ns) == 0 {
		return nil, nil, nil
	}
	if len(ns) == 1 {
		return ns[0], tn, nil
	}
	return ns, tn, nil
}
