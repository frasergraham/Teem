package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/frasergraham/teem/internal/agent"
	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/channelbus"
	"github.com/frasergraham/teem/internal/inflight"
	"github.com/frasergraham/teem/internal/leaderstatus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/messaging"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/pulse"
	"github.com/frasergraham/teem/internal/retention"
	"github.com/frasergraham/teem/internal/tailnet"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/usage"
	"github.com/frasergraham/teem/internal/wsbus"
)

// leaderAwareRoles returns a RolesFunc whose result is the team's
// current archetype roles plus the reserved leader role. Used as a
// single source of truth so the archmem.Store validator and the
// Summarizer's Roles callback can't drift apart if the archetype set
// changes.
func leaderAwareRoles(t *team.Team) archmem.RolesFunc {
	return func() []string {
		archs := t.SnapshotArchetypes()
		roles := make([]string, 0, len(archs)+1)
		for _, a := range archs {
			roles = append(roles, a.Role)
		}
		roles = append(roles, archmem.LeaderRole)
		return roles
	}
}

// --- daemon process: serve the multi-tenant orchestrator ------------------

// daemon is the long-running state owned by serveDaemon. It holds a
// registry of teams; each registered team has its own MCP server,
// spawner, audit sink, and a worker→leader URL pointed at the daemon's
// per-team mount path.
type daemon struct {
	tnetNode   *tailnet.Node
	httpClient *http.Client
	endpoint   string          // public URL: http://host:port
	token      string          // shared bearer for /audit and /control
	baseCtx    context.Context // daemon lifetime — passed to per-team spawners

	// messagingNotifier is the daemon-global outbound push channel
	// (Telegram in v1) loaded from ~/.teem/messaging.yaml. nil when
	// messaging is disabled — combineHooks drops the nil hook silently.
	messagingNotifier messaging.Notifier
	messagingCfg      messaging.TelegramConfig
	messagingDedup    *messaging.Dedup
	// messagingTelegram is the concrete Telegram notifier (when
	// telegram is enabled) — kept around separately from the wrapped
	// messagingNotifier so the inbound webhook handler can post to
	// arbitrary chat_ids via SendText.
	messagingTelegram *messaging.TelegramNotifier
	// messagingReplierOverride is the test seam — when non-nil it
	// replaces the production Telegram path so unit tests can record
	// outbound posts without a real Telegram server.
	messagingReplierOverride telegramReplier
	// messagingMessageIDLookup is the test seam for the native-reply
	// gesture path: when non-nil it replaces the production
	// d.messagingTelegram.LookupByMessageID call. Tests inject a fake
	// mapping without standing up a real notifier.
	messagingMessageIDLookup func(int64) (string, bool)
	// messagingReplyTokens correlates outbound Telegram pings with inbound
	// /reply <token> messages. Issued at outbound emit, consumed by the
	// inbound webhook handler. Nil when messaging is disabled.
	messagingReplyTokens *messaging.ReplyTokenStore
	// messagingWebhookToken is the daemon-issued random token that must
	// appear as ?token=<value> on the inbound webhook URL. Rotates on
	// daemon start so a leaked token expires when the operator bounces
	// the daemon. Persisted to ~/.teem/state/messaging-webhook.json so
	// `teem messaging telegram register-webhook` can read the current
	// value out of band.
	messagingWebhookToken string
	// messagingChatSessions tracks in-flight Telegram reply subprocesses
	// keyed by replyToken. Used to enforce the 10-minute idle kill and
	// the /done early-exit.
	messagingChatSessions *telegramChatSessions
	// messagingWebhookListener is the dedicated second listener (when
	// telegram.webhook_port > 0) that exposes ONLY the webhook
	// endpoint, so Tailscale Funnel can be pointed at it without
	// also publishing the dashboard / MCP / control routes from the
	// main listener. Nil when webhook_port is 0.
	messagingWebhookListener net.Listener
	messagingWebhookServer   *http.Server
	// messagingWebhookPort is the local port the webhook listener bound
	// to. Captured so the tsnet Funnel goroutine can target it without
	// reaching back into messagingWebhookListener.Addr().
	messagingWebhookPort int

	// usageAgg is the daemon-global daily token-budget tracker.
	// Spawners consult it before provisioning new workers; the audit
	// hook chain feeds it KindUsageEvent rows so it stays current.
	// Nil-OK in tests that don't wire usage.
	usageAgg *usage.Aggregator

	// chatRunner is the subprocess seam the chat handler uses to spawn
	// the leader subprocess. Production wires it to the real
	// `claude -p` invocation; tests inject a fake. Nil ⇒ defaultChatRunner.
	chatRunner chatRunner

	// chatTimeout overrides the chat handler's per-request deadline.
	// Zero means 5 minutes (matches pulse.TickTimeout). Tests set this
	// to a small value so the timeout path is reachable in milliseconds.
	chatTimeout time.Duration

	mu    sync.Mutex
	teams map[string]*registeredTeam
}

type registeredTeam struct {
	team          *team.Team
	mcp           *mcpsrv.Server
	spawner       *agent.Spawner
	auditSink     audit.Sink
	auditH        http.Handler
	plan          *plan.Plan
	notes         *notes.Inbox
	pulse         *pulse.Pulse
	inFlight      *inflight.Log
	registry      *mcpsrv.Registry
	archMem       *archmem.Store
	archMemCancel context.CancelFunc
	leaderStatus  *leaderstatus.Store
	leaderURL     string
	registered    time.Time
	// transcriptsDir is the leader-side mirror root for worker
	// transcript files: <stateDir>/transcripts/<agent>/<job>.jsonl.
	transcriptsDir string
	// repoRoot is the git working tree the team's workers branch off
	// of. Empty for Fargate-only / repo-less teams; the dashboard's
	// "Active branches" section renders an empty placeholder in that
	// case. Comes straight from the registration payload.
	repoRoot string
	// channelBus fans channel events (PushChannel calls) out to every
	// teem-channel SSE subscriber. One bus per team; survives daemon
	// lifetime, no persistence.
	channelBus *channelbus.Bus

	// wsbus fans audit + snapshot-invalidate envelopes out to SPA
	// WebSocket clients connected to /api/teams/<id>/events. Holds a
	// ring buffer of recent envelopes for since_seq backfill on
	// reconnect. In-memory only; restarting the daemon resets the seq
	// counter (clients fall back to snapshot_invalidate).
	wsbus *wsbus.Bus

	// detectionMu guards channelsLive and serializes the per-team
	// channels-live ↔ fallback state machine. Held around BOTH the
	// channelbus Subscribe/cancel call and the flag mutation so the
	// "first subscriber" / "last subscriber" decision is TOCTOU-safe
	// even when two SSE handlers connect concurrently. See
	// docs/wake-strategy.md §5.
	detectionMu  sync.Mutex
	channelsLive bool
}

// teamView pairs a stable *registeredTeam pointer with a snapshot of
// the team.Team fields that can mutate at runtime under d.mu (currently
// Name and Leader.SystemPrompt, refreshed by handleRegister on
// re-register). Every other registeredTeam field is fixed once
// buildTeamServices returns, so the rt pointer itself can be read
// outside the lock — only the inner display fields need copying.
//
// Build with snapshotTeams / snapshotTeam inside a single d.mu critical
// section, then operate on the snapshot. This lets sort-by-name, log
// lines, and page titles run outside the lock without racing handleRegister.
type teamView struct {
	rt                 *registeredTeam
	Name               string
	LeaderSystemPrompt string
}

// snapshotTeams returns a teamView for every registered team, captured
// in a single d.mu critical section.
func (d *daemon) snapshotTeams() []teamView {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]teamView, 0, len(d.teams))
	for _, rt := range d.teams {
		out = append(out, teamView{
			rt:                 rt,
			Name:               rt.team.Name,
			LeaderSystemPrompt: rt.team.Leader.SystemPrompt,
		})
	}
	return out
}

// snapshotTeam captures the mutable display fields for one rt.
func (d *daemon) snapshotTeam(rt *registeredTeam) teamView {
	d.mu.Lock()
	defer d.mu.Unlock()
	return teamView{
		rt:                 rt,
		Name:               rt.team.Name,
		LeaderSystemPrompt: rt.team.Leader.SystemPrompt,
	}
}

// --- retention sweep -------------------------------------------------------

// runRetentionGC ticks on cfg.SweepInterval and, for each registered
// team, removes stopped registry entries older than cfg.StoppedAgentTTL
// and transcript files older than cfg.TranscriptTTL. Default
// configuration ("never delete") prevents this goroutine from being
// started in the first place — see serveDaemon's retCfg.Enabled() guard.
//
// The first sweep runs 30s after startup so a developer can observe
// whether the configured TTL is sane without waiting an hour. Subsequent
// sweeps fire on the configured interval (default 1h).
func (d *daemon) runRetentionGC(cfg retention.Config) {
	interval := retentionSweepInterval(cfg)
	select {
	case <-d.baseCtx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	d.retentionSweep(cfg)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-d.baseCtx.Done():
			return
		case <-t.C:
			d.retentionSweep(cfg)
		}
	}
}

// retentionSweep runs one pass across every registered team, logging
// counts only when something was actually removed so the log stays
// quiet on idle systems.
func (d *daemon) retentionSweep(cfg retention.Config) {
	d.mu.Lock()
	teams := make([]*registeredTeam, 0, len(d.teams))
	for _, rt := range d.teams {
		teams = append(teams, rt)
	}
	d.mu.Unlock()
	now := time.Now()
	for _, rt := range teams {
		if cfg.StoppedAgentTTL > 0 && rt.registry != nil {
			if removed := rt.registry.GCStopped(now, cfg.StoppedAgentTTL); removed > 0 {
				fmt.Fprintf(os.Stderr, "[retention] %s: pruned %d stopped agent(s) older than %s\n",
					rt.team.Name, removed, cfg.StoppedAgentTTL)
			}
		}
		if cfg.TranscriptTTL > 0 && rt.transcriptsDir != "" {
			removed, err := retention.SweepTranscripts(rt.transcriptsDir, now, cfg.TranscriptTTL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[retention] %s: transcript sweep error: %v\n", rt.team.Name, err)
			}
			if removed > 0 {
				fmt.Fprintf(os.Stderr, "[retention] %s: removed %d transcript file(s) older than %s\n",
					rt.team.Name, removed, cfg.TranscriptTTL)
			}
		}
	}
}

// retentionSweepInterval returns the effective sweep cadence, falling
// back to retention.DefaultSweepInterval when unset.
func retentionSweepInterval(cfg retention.Config) time.Duration {
	if cfg.SweepInterval > 0 {
		return cfg.SweepInterval
	}
	return retention.DefaultSweepInterval
}
