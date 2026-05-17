package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/agent"
	"github.com/frasergraham/teem/internal/archmem"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/channelbus"
	"github.com/frasergraham/teem/internal/inflight"
	"github.com/frasergraham/teem/internal/leaderstatus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/messaging"
	"github.com/frasergraham/teem/internal/notes"
	"github.com/frasergraham/teem/internal/plan"
	"github.com/frasergraham/teem/internal/prompts"
	"github.com/frasergraham/teem/internal/pulse"
	"github.com/frasergraham/teem/internal/roster"
	"github.com/frasergraham/teem/internal/state"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/wsbus"
)

// buildTeamServices stands up the per-team MCP server, spawner, and
// audit sink. Repo root and worktree base come from the chat client's
// CWD (so worktrees land where the operator expects).
func (d *daemon) buildTeamServices(t *team.Team, repoRoot, worktreeBase string) (*registeredTeam, error) {
	if t.ID == "" {
		// Defensive: every code path that constructs a Team should
		// have minted an id by now. Mint one if not so we never key a
		// per-team filesystem path on the empty string.
		t.ID = team.NewID()
		fmt.Fprintf(os.Stderr, "[teemd] warning: team %q had no id; minted %s\n", t.Name, t.ID)
	}
	if worktreeBase == "" {
		worktreeBase = defaultWorktreeBase(t.ID)
	}
	leaderURL := d.endpoint + "/teams/" + t.ID
	stateStore := state.NewStore(defaultStateDir(t.ID))
	gitCfg := readGitConfig()

	auditPath := defaultAuditPath(t.ID)
	auditFile, err := audit.OpenFile(auditPath)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	// All audit Writes flow through this decorator chain:
	//   FileSink ◄ hookedSink ◄ injectingSink
	// The outer injectingSink stamps Meta["task_id"] on events whose
	// JobID is registered in jobTaskIdx so the hook chain
	// (messaging, channels, archmem, pulse-nudge, usage, …) and the
	// on-disk JSONL see a uniform event shape with task attribution
	// already in place. The hook itself is wired in below, after the
	// per-team components it depends on have been constructed.
	hookedAudit := newHookedSink(auditFile)
	jobTaskIdx := audit.NewJobTaskIndex()
	auditSink := newInjectingSink(hookedAudit, jobTaskIdx)

	planPath := defaultPlanPath(t.ID)
	planStore, err := plan.Open(planPath)
	if err != nil {
		_ = auditSink.Close()
		return nil, fmt.Errorf("plan: %w", err)
	}

	// Rehydrate the job→task index from plan evidence so audit events
	// for jobs that survived a daemon restart still pick up task_id.
	// Terminal-kind events emitted *after* restart will clear the
	// entry, so the index naturally trims itself back to in-flight
	// jobs once the next worker tick lands.
	for _, task := range planStore.List(plan.Filter{}) {
		for _, jid := range task.Evidence {
			// agent_id is unknown at rehydration time — the
			// originating spawner state did not survive the
			// bounce. The entry still clears on per-job
			// terminal kinds; only ClearByAgent skips it.
			jobTaskIdx.Set(jid, task.ID, "")
		}
	}

	notesInbox, err := notes.Open(defaultNotesPath(t.ID))
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		return nil, fmt.Errorf("notes: %w", err)
	}

	leaderStatusStore, err := leaderstatus.Open(defaultLeaderStatusPath(t.ID))
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("leader_status: %w", err)
	}

	// In-flight log for durability. Opened before reconcile so the
	// next steps can both (a) emit job_interrupted for orphans and
	// (b) hand it to the spawner for future jobs.
	inFlightLog, err := inflight.Open(defaultInFlightPath(t.ID))
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("inflight: %w", err)
	}
	// Reconcile: any "start" without a matching "end" in the log was
	// interrupted by the previous shutdown. Emit a final audit event
	// so the leader can see what's incomplete, then truncate so we
	// don't re-report on the next restart.
	if orphans, err := inFlightLog.Outstanding(); err == nil && len(orphans) > 0 {
		for _, o := range orphans {
			_ = auditSink.Write(audit.Event{
				Timestamp: time.Now().UTC(),
				AgentID:   o.AgentID,
				JobID:     o.JobID,
				Kind:      audit.KindJobInterrupted,
				Message:   "daemon shutdown interrupted this job",
				Meta: map[string]any{
					"prompt_preview": o.PromptPreview,
					"started_at":     o.StartedAt.Format(time.RFC3339),
				},
			})
		}
		fmt.Fprintf(os.Stderr, "[teemd] %s: marked %d job(s) interrupted by prior shutdown\n", t.Name, len(orphans))
		_ = inFlightLog.Reset()
	}

	bs := bus.NewMemBus()
	reg := mcpsrv.NewRegistry()
	chBus := channelbus.New(0)
	wsB := wsbus.New(2000)

	// Archetype memory store: per-team directory of per-role markdown
	// files the leader injects as baseline context for every freshly
	// spawned worker. Created up-front so the spawner can read from
	// it and the audit hook can append to it.
	archMemDir := defaultMemoryDir(t.ID)
	archMemStore := archmem.New(archMemDir, leaderAwareRoles(t))
	archMemStore.SweepTmp()

	transcriptsDir := filepath.Join(defaultStateDir(t.ID), "transcripts")

	// Prompt builder: layered assembly of the leader's and each
	// archetype's system prompt with an operator-override layer on
	// disk. Shared by the CLI, the MCP read_prompt/append_prompt
	// tools, and the spawner's per-worker bake-in.
	promptBuilder := prompts.New(t, defaultPromptOverrideDir(t.ID))

	// Roster: per-team worker-name allocator. On first open after the
	// T9 rollout (no existing roster.json), migrate legacy
	// `<role>-N` ids from the previous archetype-seq.json counter
	// and any historical transcripts subdirs so they participate in
	// reincarnation. The legacy file is left in place — we no longer
	// read it, but keeping it makes a downgrade non-destructive.
	rosterPath := defaultRosterPath(t.ID)
	rost, err := roster.Open(rosterPath)
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("roster: %w", err)
	}
	roleList := func() []string {
		archs := t.SnapshotArchetypes()
		roles := make([]string, 0, len(archs))
		for _, a := range archs {
			roles = append(roles, a.Role)
		}
		return roles
	}()
	if n := rost.MigrateLegacy(defaultArchetypeSeqPath(t.ID), transcriptsDir, roleList, nil); n > 0 {
		fmt.Fprintf(os.Stderr, "[teemd] %s: migrated %d legacy worker id(s) into the roster\n", t.Name, n)
	}

	spawner := agent.NewSpawner(d.baseCtx, t, bs, reg, agent.Config{
		HTTPClient:          d.httpClient,
		WorkerToken:         d.token,
		CloudProvisioner:    cloudProvisionerFactory(d.token, leaderURL, gitCfg, stateStore),
		RepoRoot:            repoRoot,
		WorktreeBase:        worktreeBase,
		LeaderURL:           leaderURL,
		StateStore:          stateStore,
		AuditSink:           auditSink,
		Roster:              rost,
		InFlight:            inFlightLog,
		SocketDir:           defaultSocketDir(t.ID),
		LoadArchetypeMemory: archMemStore.Load,
		// Spawner.LoadArchetypePrompt is (role) -> string; Builder.Archetype
		// now signals "role not declared" via the bool. The spawner only
		// reaches here for roles it just resolved from the team YAML, so
		// an empty string on miss is a safe degenerate case.
		LoadArchetypePrompt: func(role string) string {
			s, _ := promptBuilder.Archetype(role)
			return s
		},
		UsageQuota: d.spawnerQuota(),
	})

	srv, err := mcpsrv.New(mcpsrv.Config{
		Bus:            bs,
		Team:           t,
		Registry:       reg,
		Spawner:        spawner,
		Audit:          auditSink,
		Plan:           planStore,
		Notes:          notesInbox,
		TranscriptsDir: transcriptsDir,
		ArchMem:        archMemStore,
		LeaderStatus:   leaderStatusStore,
		Prompts:        promptBuilder,
		JobTaskIndex:   jobTaskIdx,
		ChannelSink: func(content string, meta map[string]string) {
			chBus.Publish(channelbus.Event{Content: content, Meta: meta})
		},
	})
	if err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, err
	}

	// Pulse: autonomous-leader heartbeat. Built per team, NOT started
	// (phase 4's `teem pulse start` activates it). Needs the team's
	// MCP URL via a small JSON file pulse hands to claude.
	pulseMCPPath := filepath.Join(defaultStateDir(t.ID), "pulse-mcp.json")
	shimPath, _ := exec.LookPath("teem-channel")
	if err := pulse.WriteMCPConfig(pulseMCPPath, leaderURL+"/mcp", t.ID, d.endpoint, shimPath); err != nil {
		_ = auditSink.Close()
		_ = planStore.Close()
		_ = notesInbox.Close()
		return nil, fmt.Errorf("pulse mcp config: %w", err)
	}
	pulseInst := pulse.New(pulse.Config{
		TeamName: t.Name,
		TeamID:   t.ID,
		LoadSession: func() (string, bool, error) {
			s, ok, err := loadLeaderSession(t.ID)
			if err != nil || !ok {
				return "", false, err
			}
			return s.SessionID, true, nil
		},
		PauseFile:      filepath.Join(defaultStateDir(t.ID), "pulse.paused"),
		RunningFile:    defaultPulseRunningFlag(t.ID),
		WakePromptFile: filepath.Join(defaultStateDir(t.ID), "pulse-wake.txt"),
		ConfigPath:     filepath.Join(defaultStateDir(t.ID), "pulse_config.json"),
		MCPConfig:      pulseMCPPath,
		RepoRoot:       repoRoot,
		Plan:           planStore,
		Audit:          auditSink,
		Registry:       reg,
		// Usage rollups land on the wrapped audit sink (KindUsageEvent),
		// which the usage hook records into the aggregator. Wiring
		// OnUsage here as well would double-count every pulse tick.
		OnChannelNudge: func(context.Context) { publishPulseChannelNudge(chBus) },
	})

	// Wake hook: publish on the in-process leader.wake bus topic
	// whenever a worker emits a job-terminal event. Today no chat
	// client consumes it because runChat exec()s directly into claude;
	// kept as an additive T6 signal future chat clients can subscribe
	// to.
	wakeHook := func(events []audit.Event) {
		for _, e := range events {
			if !isWakeKind(e.Kind) {
				continue
			}
			payload, _ := json.Marshal(map[string]string{
				"kind":     string(e.Kind),
				"agent_id": e.AgentID,
				"job_id":   e.JobID,
			})
			_ = bs.Publish(d.baseCtx, bus.Message{
				ID:        bus.NewID(),
				Topic:     "leader.wake",
				From:      e.AgentID,
				Kind:      bus.KindStatus,
				Payload:   payload,
				CreatedAt: time.Now().UTC(),
			})
		}
	}

	// Stop hook: when a worker emits worker_stopped, reconcile the
	// spawner's bookkeeping (registry → stopped, teardown skipping
	// /shutdown, drop subscriptions). Runs in a goroutine so the
	// audit POST returns promptly; HandleWorkerStopped is idempotent
	// against duplicates.
	stopHook := func(events []audit.Event) {
		for _, e := range events {
			if e.Kind != audit.KindWorkerStopped {
				continue
			}
			agentID := e.AgentID
			go spawner.HandleWorkerStopped(context.Background(), agentID)
		}
	}

	// Archmem hook: on every job-terminal event, append a one-line
	// summary to the archetype's per-role memory file. The role is
	// resolved from the registry; if the agent is gone we skip
	// silently (audit fallback would need to scan history and isn't
	// worth it for an append).
	archMemHook := func(events []audit.Event) {
		for _, e := range events {
			if e.Kind != audit.KindJobComplete && e.Kind != audit.KindJobError {
				continue
			}
			role := lookupRole(reg, e.AgentID)
			if role == "" {
				continue
			}
			status := "done"
			summary, _ := e.Meta["output"].(string)
			if e.Kind == audit.KindJobError {
				status = "error"
				if summary == "" {
					summary = e.Message
				}
			}
			entry := archmem.Entry{
				Timestamp: e.Timestamp,
				AgentID:   e.AgentID,
				JobID:     e.JobID,
				Status:    status,
				Summary:   shortSummary(summary),
			}
			if err := archMemStore.AppendEntry(role, entry); err != nil && !errors.Is(err, archmem.ErrUnknownRole) {
				fmt.Fprintf(os.Stderr, "[archmem] append %q: %v\n", role, err)
			}
		}
	}

	// Channel hook: push a one-line summary of selected audit events
	// into the leader's claude session via the team's MCP server
	// (Claude Code "channels"). Fire-and-forget; safe when no leader is
	// currently subscribed. The filter intentionally excludes
	// high-volume kinds like heartbeats and pulse_tick echoes.
	channelHook := makeChannelHook(srv.PushChannel)

	// Messaging hook: out-of-band push to the operator's phone (Telegram
	// in v1) for the narrow operator-must-see set — awaiting_approval,
	// blockers, severity=question decisions, leader errors. Daemon-global
	// notifier, per-team formatter (needs plan for task titles). nil when
	// messaging.yaml is absent / disabled / missing token; combineHooks
	// drops the nil hook silently.
	var messagingHook auditHook
	if d.messagingNotifier != nil && d.messagingDedup != nil {
		fmtr := messaging.MessageFormatter{
			TeamID:           t.ID,
			DashboardBaseURL: d.messagingCfg.DashboardBaseURL,
			TaskTitle:        messaging.FromPlan(planStore),
			LeaderStatus: func() string {
				if e, ok := leaderStatusStore.Get("leader"); ok {
					return e.Text
				}
				return ""
			},
		}
		messagingHook = makeMessagingHook(d.messagingNotifier, fmtr, d.messagingDedup, d.messagingReplyTokens)
	}

	// Pulse audit-nudge hook: schedules a debounced pulse Tick on
	// interesting worker events when channels are NOT live (the L2
	// fallback path in docs/wake-strategy.md). Pulse internally gates
	// on its channels-live flag — when chat is connected, this hook is
	// a no-op and the channel block does the waking. Safe whether or
	// not Pulse has been Started: NudgeFromAudit early-returns when
	// the loop isn't running.
	pulseNudgeHook := func(events []audit.Event) {
		pulseInst.NudgeFromAudit(events)
	}

	// SPA WebSocket hook: publish every audit event onto the per-team
	// wsbus so /api/teams/<id>/events clients can react in real time.
	// Fire-and-forget; the bus drops events for slow subscribers
	// individually (see internal/wsbus).
	wsbusHook := func(events []audit.Event) {
		for i := range events {
			ev := events[i]
			wsB.Publish(wsbus.Envelope{
				Kind:  "audit",
				Seq:   wsB.NextSeq(),
				TS:    time.Now().UTC(),
				Event: &ev,
			})
		}
	}

	// With every component wired, install the hook chain on the
	// decorator. Subsequent Writes — from MCP tools, pulse, chat,
	// dashboard form posts, the HTTP audit handler — fan out through
	// this chain. The HTTP middleware no longer fires hooks itself;
	// the wrapped sink is the only source.
	hookedAudit.SetHook(combineHooks(wakeHook, stopHook, archMemHook, channelHook, messagingHook, pulseNudgeHook, wsbusHook, d.makeUsageHook()))

	// Auto-resume Pulse if it was running before the daemon restarted.
	// Started AFTER SetHook so the first tick's KindUsageEvent reaches
	// the usage hook (otherwise the bare FileSink Write happens before
	// the hook chain is installed). Operator opt-out is `teem pulse stop`
	// (which clears the flag) or `teem pulse pause` (which leaves it
	// alone but skips ticks).
	if pulseInst.WasRunning() {
		pulseInst.Start(d.baseCtx)
		fmt.Fprintf(os.Stderr, "[teemd] auto-resumed Pulse for %q\n", t.Name)
	}

	// Summarizer goroutine: rolling digest + retention pruning per
	// role. Best-effort — failures log to stderr and the next tick
	// retries. Uses the operator's Claude Code auth via `claude -p`
	// subprocess; if the binary isn't on PATH we still run the loop
	// so retention pruning happens, just without an LLM digest.
	archMemCtx, archMemCancel := context.WithCancel(d.baseCtx)
	var completer archmem.Completer
	if path, err := exec.LookPath("claude"); err == nil {
		completer = archmem.NewClaudeSubprocessCompleter(path, repoRoot)
	} else {
		fmt.Fprintf(os.Stderr, "[archmem] claude CLI not on PATH; digest will be skipped: %v\n", err)
	}
	summarizer := &archmem.Summarizer{
		Store:    archMemStore,
		Complete: completer,
		Roles:    leaderAwareRoles(t),
	}
	safeGo("archmem.summarizer:"+t.ID, func() { _ = summarizer.Run(archMemCtx) })

	// Scheduled project-manager tick. Only fires for tracker-configured
	// teams (the PM archetype was synthesised by MaybePMArchetype
	// earlier in the same code path); a zero/negative PollInterval
	// disables the loop while leaving the on-demand leader spawn alive.
	if t.Tracker != nil && t.Tracker.Type != "" {
		interval := t.Tracker.PollInterval
		if interval == 0 {
			interval = pmLoopDefaultInterval
		}
		if interval > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] %s: pm-loop interval=%s\n", t.Name, interval)
			pmCfg := PMLoopConfig{
				TeamName: t.Name,
				Interval: interval,
				Spawner:  spawner,
				Audit:    auditSink,
			}
			safeGo("pm.loop:"+t.ID, func() { pmCfg.Loop(d.baseCtx) })
		}
	}

	// Orphan-job sweep: catch job_received events that never got a
	// terminal partner (worker SIGKILL with no worker_stopped emit,
	// audit-post lost mid-flight, etc.) so dangling worker rows in
	// the SPA and JobTaskIndex entries get cleaned up. Idempotent —
	// the synthetic interrupt itself terminates the job in the log,
	// so subsequent ticks see nothing new.
	teamID := t.ID
	safeGo("orphan-sweep:"+teamID, func() { runOrphanJobSweep(d.baseCtx, teamID, auditSink) })

	return &registeredTeam{
		team:      t,
		mcp:       srv,
		spawner:   spawner,
		auditSink: auditSink,
		// Audit handler fans every POST out to: write to disk via the
		// hooked sink (which runs the hook chain — wake, stop,
		// archmem, channels, messaging, pulse-nudge, wsbus, usage) and
		// bump the agent's LastSeen on the registry. The hook chain
		// itself is wired on auditSink above so any caller — HTTP, MCP,
		// pulse, chat — fans out identically.
		auditH:         newAuditHandlerWithRegistry(audit.Handler(auditSink, d.token), reg),
		plan:           planStore,
		notes:          notesInbox,
		pulse:          pulseInst,
		inFlight:       inFlightLog,
		registry:       reg,
		archMem:        archMemStore,
		archMemCancel:  archMemCancel,
		leaderStatus:   leaderStatusStore,
		leaderURL:      leaderURL,
		registered:     time.Now(),
		transcriptsDir: transcriptsDir,
		repoRoot:       repoRoot,
		channelBus:     chBus,
		wsbus:          wsB,
	}, nil
}

// defaultLeaderStatusPath returns the per-team leader-status board
// file path, alongside plan.jsonl and notes.jsonl.
func defaultLeaderStatusPath(teamID string) string {
	return filepath.Join(defaultStateDir(teamID), "leader_status.json")
}

// defaultMessagingDedupPath returns the daemon-global dedup state file
// (not per-team — messaging is daemon-wide for v1).
func defaultMessagingDedupPath() string {
	return filepath.Join(daemonHomeDir(), "state", "messaging.json")
}

// defaultMessagingReplyTokensPath is the on-disk store for outbound→
// inbound reply tokens. Survives daemon restarts so a Telegram /reply
// arriving after a bounce still finds its task context (subject to the
// 24h TTL).
func defaultMessagingReplyTokensPath() string {
	return filepath.Join(daemonHomeDir(), "state", "messaging-reply-tokens.json")
}

// defaultMessagingWebhookTokenPath holds the daemon-issued random token
// embedded in the inbound webhook URL. Re-written on every daemon start.
func defaultMessagingWebhookTokenPath() string {
	return filepath.Join(daemonHomeDir(), "state", "messaging-webhook.json")
}

// initMessaging loads ~/.teem/messaging.yaml, resolves credentials from
// the environment, and stashes the resulting Notifier + Dedup on the
// daemon for per-team wiring. Returns an error only when messaging is
// configured-but-broken (enabled with no token / chat_id) — a missing
// config file is the silent off state.
func (d *daemon) initMessaging() error {
	cfg, err := messaging.Load(daemonHomeDir())
	if err != nil {
		return err
	}
	n, tn, err := messaging.Resolve(cfg, os.Getenv)
	if err != nil {
		return err
	}
	if n == nil {
		return nil
	}
	dedup, err := messaging.NewDedup(defaultMessagingDedupPath(), cfg.Telegram.DedupWindow())
	if err != nil {
		// Best-effort: a broken dedup file logs but doesn't block startup.
		// The fresh in-memory map still gates duplicates this run.
		fmt.Fprintf(os.Stderr, "[messaging] dedup state warning: %v\n", err)
	}
	tokens, err := messaging.NewReplyTokenStore(defaultMessagingReplyTokensPath(), messaging.DefaultReplyTokenTTL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[messaging] reply-token state warning: %v\n", err)
	}
	webhookToken, err := mintWebhookToken(defaultMessagingWebhookTokenPath())
	if err != nil {
		return fmt.Errorf("messaging: mint webhook token: %w", err)
	}
	d.messagingNotifier = n
	d.messagingTelegram = tn
	d.messagingCfg = cfg.Telegram
	d.messagingDedup = dedup
	d.messagingReplyTokens = tokens
	d.messagingWebhookToken = webhookToken
	d.messagingChatSessions = newTelegramChatSessions()
	fmt.Fprintf(os.Stderr, "[teemd] messaging: telegram enabled (chat_id=%d)\n", cfg.Telegram.ChatID)
	return nil
}

// autoRegisterTelegramWebhook compares the Telegram bot's currently
// registered webhook URL against the daemon's expected hookURL (built
// from messaging.telegram.public_url + the freshly-minted webhook
// token), and POSTs setWebhook only when they differ. Logged failures
// are non-fatal — a slow or transient Telegram API call must never
// stop the daemon coming up.
func (d *daemon) autoRegisterTelegramWebhook(ctx context.Context) {
	if d.messagingTelegram == nil || d.messagingCfg.PublicURL == "" || d.messagingWebhookToken == "" {
		return
	}
	base := strings.TrimRight(d.messagingCfg.PublicURL, "/")
	hookURL := base + messaging.WebhookPath + "?token=" + url.QueryEscape(d.messagingWebhookToken)
	changed, err := d.messagingTelegram.EnsureWebhook(ctx, hookURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] messaging: telegram auto-register failed: %v\n", err)
		return
	}
	if changed {
		fmt.Fprintf(os.Stderr, "[teemd] messaging: telegram webhook updated -> %s\n", redactToken(hookURL))
	} else {
		fmt.Fprintf(os.Stderr, "[teemd] messaging: telegram webhook already registered with bot\n")
	}
}

// enableTelegramFunnel configures Tailscale Funnel on the tsnet node so
// https://<node-fqdn>/messaging/telegram/webhook reaches the dedicated
// webhook listener on host loopback. Non-fatal: any failure here is
// logged and the daemon continues — the operator can fall back to the
// host-side `tailscale funnel` command, and unrelated daemon
// functionality (dashboard, MCP, control) is unaffected.
//
// Retried with a 2s backoff because EnableFunnel resolves the node's
// FQDN via tsnet's LocalClient.Status, and Self.DNSName comes back
// empty until tsnet completes first-auth — without retries the goroutine
// can fire just before that and error out with no recovery.
func (d *daemon) enableTelegramFunnel(ctx context.Context) {
	if d.tnetNode == nil || d.messagingWebhookPort <= 0 {
		return
	}
	const (
		attempts = 3
		backoff  = 2 * time.Second
	)
	var (
		fqdn string
		err  error
	)
	for i := 0; i < attempts; i++ {
		fqdn, err = d.tnetNode.EnableFunnel(ctx, messaging.WebhookPath, d.messagingWebhookPort)
		if err == nil {
			break
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] messaging: tsnet Funnel setup failed after %d attempt(s): %v\n", attempts, err)
		return
	}
	if fqdn == "" {
		// Funnel succeeded but we couldn't read the FQDN back — log
		// without it. Shouldn't happen since EnableFunnel just used it,
		// but the log is for the operator, not the code path.
		fmt.Fprintf(os.Stderr, "[teemd] messaging: tsnet Funnel enabled for %s -> :%d\n", messaging.WebhookPath, d.messagingWebhookPort)
		return
	}
	fmt.Fprintf(os.Stderr, "[teemd] messaging: tsnet Funnel enabled for %s -> :%d (https://%s%s)\n", messaging.WebhookPath, d.messagingWebhookPort, fqdn, messaging.WebhookPath)
}

// mintWebhookToken generates 32 hex chars of entropy, writes it to path
// (replacing any prior value), and returns the new token. Daemon start
// rotates the token so a leaked URL stops working after a bounce.
func mintWebhookToken(path string) (string, error) {
	var buf [16]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf[:])
	if path == "" {
		return tok, nil
	}
	body, err := json.Marshal(map[string]string{"token": tok})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return tok, nil
}

// readWebhookToken reads the daemon-issued inbound-webhook token from
// disk. The `teem messaging telegram register-webhook` CLI uses this to
// learn the current token out-of-band from a running daemon.
func readWebhookToken(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var stored struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &stored); err != nil {
		return "", err
	}
	if stored.Token == "" {
		return "", fmt.Errorf("messaging: webhook token missing in %s", path)
	}
	return stored.Token, nil
}

// lookupRole returns the role for agentID from the registry, or ""
// when the agent isn't currently tracked. Falls back to parsing the
// instance suffix off the id ("<role>-<N>") because the registry can
// race a worker_stopped reconcile.
func lookupRole(reg *mcpsrv.Registry, agentID string) string {
	if e, ok := reg.Get(agentID); ok && e.Role != "" {
		return e.Role
	}
	if i := strings.LastIndexByte(agentID, '-'); i > 0 {
		return agentID[:i]
	}
	return ""
}
