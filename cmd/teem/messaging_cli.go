package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/frasergraham/teem/internal/messaging"
)

// runMessagingCmd dispatches `teem messaging …` subcommands. v1 has one:
// `telegram register-webhook` — calls Telegram's setWebhook endpoint
// with the daemon's `/messaging/telegram/webhook?token=…` URL.
func runMessagingCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: teem messaging <channel> <subcommand>\n  e.g. teem messaging telegram register-webhook")
	}
	switch args[0] {
	case "telegram":
		return runMessagingTelegramCmd(args[1:])
	default:
		return fmt.Errorf("unknown messaging channel %q (supported: telegram)", args[0])
	}
}

func runMessagingTelegramCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: teem messaging telegram <subcommand>\n  subcommands: register-webhook")
	}
	switch args[0] {
	case "register-webhook":
		return runMessagingTelegramRegisterWebhook(args[1:])
	default:
		return fmt.Errorf("unknown telegram subcommand %q", args[0])
	}
}

// runMessagingTelegramRegisterWebhook reads the daemon-issued webhook
// token + the local messaging.yaml config, asks the operator for a
// publicly-reachable base URL (Tailscale Funnel, ngrok, …), and posts
// setWebhook to Telegram's API.
//
// We read the daemon's webhook token out-of-band from
// ~/.teem/state/messaging-webhook.json so the CLI doesn't need to be
// authenticated to the daemon. The bot token still comes from the env
// var named in messaging.yaml — same model as the daemon's outbound
// path, so credentials only live in one place.
func runMessagingTelegramRegisterWebhook(args []string) error {
	fs := flag.NewFlagSet("register-webhook", flag.ExitOnError)
	publicURL := fs.String("public-url", "", "publicly-reachable base URL the bot can POST to (e.g. https://my-tailnet.ts.net)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *publicURL == "" {
		return fmt.Errorf("--public-url is required (the URL Telegram should hit; e.g. a Tailscale Funnel URL fronting the daemon)")
	}
	base := strings.TrimRight(*publicURL, "/")

	cfg, err := messaging.Load(daemonHomeDir())
	if err != nil {
		return fmt.Errorf("load messaging config: %w", err)
	}
	if !cfg.Enabled || !cfg.Telegram.Enabled {
		return fmt.Errorf("telegram messaging is not enabled in %s — set messaging.enabled=true and telegram.enabled=true first", messaging.MessagingYAMLPath(daemonHomeDir()))
	}
	tokenEnv := cfg.Telegram.BotTokenEnv
	if tokenEnv == "" {
		tokenEnv = messaging.DefaultBotTokenEnv
	}
	botToken := os.Getenv(tokenEnv)
	if botToken == "" {
		return fmt.Errorf("env var %s is empty (set %s=<bot-token>)", tokenEnv, tokenEnv)
	}
	webhookToken, err := readWebhookToken(defaultMessagingWebhookTokenPath())
	if err != nil {
		return fmt.Errorf("read daemon webhook token (is the daemon running?): %w", err)
	}
	hookURL := base + messaging.WebhookPath + "?token=" + url.QueryEscape(webhookToken)

	tn := messaging.NewTelegramNotifier(botToken, cfg.Telegram.ChatID, nil)
	if err := tn.SetWebhook(context.Background(), hookURL); err != nil {
		return fmt.Errorf("setWebhook: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[teem] telegram webhook registered: %s\n", redactToken(hookURL))
	return nil
}

// redactToken strips the ?token= value from a URL for log output so a
// stray copy-paste from the operator's terminal doesn't leak the
// daemon's inbound auth.
func redactToken(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("token") != "" {
		q.Set("token", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.String()
}
