package messaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeYAML(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "messaging.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfig_LoadAbsentReturnsZero(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("absent file should give Enabled=false, got %+v", cfg)
	}
}

func TestConfig_LoadParses(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
messaging:
  enabled: true
  telegram:
    enabled: true
    bot_token_env: MY_BOT
    chat_id: 1234567
    dashboard_base_url: "https://teem.example"
    dedup_window: "5m"
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || !cfg.Telegram.Enabled {
		t.Fatalf("enabled not parsed: %+v", cfg)
	}
	if cfg.Telegram.BotTokenEnv != "MY_BOT" || cfg.Telegram.ChatID != 1234567 {
		t.Fatalf("telegram fields not parsed: %+v", cfg.Telegram)
	}
	if got := cfg.Telegram.DedupWindow(); got.String() != "5m0s" {
		t.Fatalf("dedup window = %s, want 5m0s", got)
	}
}

func TestConfig_LoadParsesWebhookPort(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
messaging:
  enabled: true
  telegram:
    enabled: true
    bot_token_env: MY_BOT
    chat_id: 1234567
    webhook_port: 7788
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.WebhookPort != 7788 {
		t.Fatalf("webhook_port not parsed: got %d want 7788", cfg.Telegram.WebhookPort)
	}
}

func TestConfig_LoadParsesPublicURL(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
messaging:
  enabled: true
  telegram:
    enabled: true
    bot_token_env: MY_BOT
    chat_id: 1234567
    public_url: "https://x.ts.net"
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.PublicURL != "https://x.ts.net" {
		t.Fatalf("public_url not parsed: got %q want %q", cfg.Telegram.PublicURL, "https://x.ts.net")
	}
}

func TestConfig_LoadPublicURLDefaultsToEmpty(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
messaging:
  enabled: true
  telegram:
    enabled: true
    bot_token_env: MY_BOT
    chat_id: 1234567
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.PublicURL != "" {
		t.Fatalf("absent public_url should default to empty, got %q", cfg.Telegram.PublicURL)
	}
}

func TestConfig_LoadParsesFunnelViaTsnet(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
messaging:
  enabled: true
  telegram:
    enabled: true
    bot_token_env: MY_BOT
    chat_id: 1234567
    funnel_via_tsnet: true
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Telegram.FunnelViaTsnet {
		t.Fatalf("funnel_via_tsnet not parsed: %+v", cfg.Telegram)
	}
}

func TestConfig_LoadFunnelViaTsnetDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
messaging:
  enabled: true
  telegram:
    enabled: true
    bot_token_env: MY_BOT
    chat_id: 1234567
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.FunnelViaTsnet {
		t.Fatalf("absent funnel_via_tsnet should default to false, got true")
	}
}

func TestConfig_LoadWebhookPortDefaultsToZero(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `
messaging:
  enabled: true
  telegram:
    enabled: true
    bot_token_env: MY_BOT
    chat_id: 1234567
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.WebhookPort != 0 {
		t.Fatalf("absent webhook_port should default to 0, got %d", cfg.Telegram.WebhookPort)
	}
}

func TestConfig_RefusesStartWithoutEnv(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Telegram: TelegramConfig{
			Enabled:     true,
			BotTokenEnv: "DEFINITELY_UNSET_VAR_XYZ",
			ChatID:      42,
		},
	}
	_, _, err := Resolve(cfg, func(string) string { return "" })
	if err == nil {
		t.Fatal("expected error when env var is empty")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_UNSET_VAR_XYZ") {
		t.Errorf("error should name the env var; got %v", err)
	}
}

func TestConfig_RefusesStartWithoutChatID(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Telegram: TelegramConfig{
			Enabled:     true,
			BotTokenEnv: "X",
			ChatID:      0,
		},
	}
	_, _, err := Resolve(cfg, func(s string) string {
		if s == "X" {
			return "tok"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected error for missing chat_id")
	}
	if !strings.Contains(err.Error(), "chat_id") {
		t.Errorf("error should mention chat_id; got %v", err)
	}
}

func TestConfig_ResolveDisabledReturnsNil(t *testing.T) {
	n, _, err := Resolve(Config{Enabled: false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Fatalf("expected nil notifier when disabled, got %T", n)
	}
}

func TestConfig_ResolveDefaultsBotTokenEnv(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Telegram: TelegramConfig{
			Enabled: true,
			ChatID:  9,
		},
	}
	n, _, err := Resolve(cfg, func(s string) string {
		if s == DefaultBotTokenEnv {
			return "tok"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == nil {
		t.Fatal("expected a notifier")
	}
}
