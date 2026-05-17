package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/channelbus"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/messaging"
	"github.com/frasergraham/teem/internal/pulse"
	"github.com/frasergraham/teem/internal/retention"
	"github.com/frasergraham/teem/internal/tailnet"
	"github.com/frasergraham/teem/internal/usage"
)

// serveDaemon runs the multi-tenant orchestrator until ctx is cancelled.
// Teams are registered lazily via POST /control/teams.
func serveDaemon(ctx context.Context, df *daemonFlags) error {
	hostname := os.Getenv("TEEM_DAEMON_HOSTNAME")
	if hostname == "" {
		hostname = "teem"
	}

	d := &daemon{teams: map[string]*registeredTeam{}, baseCtx: ctx}

	// Daemon-global messaging: optional ~/.teem/messaging.yaml drives the
	// outbound notifier wired into every team's audit hook chain. Missing
	// file = off (zero-config default). enabled=true + missing env var =
	// fail-fast so a misconfigured operator sees the error at startup
	// instead of discovering pings never arrive.
	if err := d.initMessaging(); err != nil {
		return err
	}

	// Usage-monitor wiring. Aggregator is daemon-global so the daily
	// budget applies across teams; KindUsageThrottle audit events are
	// fanned out to every team's audit sink so each leader sees its
	// local view of the throttle state.
	usageCfg, err := usage.LoadConfig(usage.DefaultConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] usage: bad config: %v (throttle disabled)\n", err)
		usageCfg = usage.Config{}
	}
	usageStore, err := usage.OpenStore(usage.DefaultStatePath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[teemd] usage: state open failed: %v (throttle disabled)\n", err)
	} else {
		d.usageAgg = usage.NewAggregator(usageCfg, usageStore, d.onUsageThrottle)
		if usageCfg.DailyTokenBudget > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] usage: cap=%d threshold=%.2f anchor=%q\n",
				usageCfg.DailyTokenBudget, usageCfg.EffectiveThreshold(), usageCfg.EffectiveAnchor())
		}
	}

	var listener net.Listener
	var mcpHost string
	if df.useTailnet {
		node, err := tailnet.New(tailnet.Config{
			Hostname: hostname,
			AuthKey:  resolveAuthKey("TS_AUTHKEY"),
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[teemd] joining tailnet as %q...\n", hostname)
		if err := node.Start(ctx); err != nil {
			return err
		}
		defer node.Close()
		listener, err = node.Listen("tcp", df.listenAddr)
		if err != nil {
			return fmt.Errorf("tailnet listen: %w", err)
		}
		d.tnetNode = node
		d.httpClient = node.HTTPClient()
		mcpHost = hostname
	} else {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1"+normalizePort(df.listenAddr))
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		mcpHost = "127.0.0.1"
	}

	if d.messagingWebhookToken != "" {
		port, defaulted := effectiveWebhookPort(d.messagingCfg, df.listenAddr)
		if port > 0 {
			addr := fmt.Sprintf(":%d", port)
			var (
				webhookListener net.Listener
				err             error
			)
			// When FunnelViaTsnet is on, tsnet's serve config will
			// terminate HTTPS at the tsnet node and proxy to
			// http://127.0.0.1:<port>. The webhook listener must be on
			// host loopback for that proxy to reach it; binding on the
			// tsnet node here would put it on the wrong side of the
			// proxy.
			bindLoopback := !df.useTailnet || d.tnetNode == nil || d.messagingCfg.FunnelViaTsnet
			if bindLoopback {
				webhookListener, err = net.Listen("tcp", "127.0.0.1"+addr)
			} else {
				webhookListener, err = d.tnetNode.Listen("tcp", addr)
			}
			if err != nil {
				if defaulted && errors.Is(err, syscall.EADDRINUSE) {
					return fmt.Errorf("messaging: webhook listener default port %d in use; set messaging.telegram.webhook_port explicitly in ~/.teem/messaging.yaml: %w", port, err)
				}
				return fmt.Errorf("messaging webhook listen on %s: %w", addr, err)
			}
			d.messagingWebhookListener = webhookListener
			d.messagingWebhookPort = port
			origin := "configured"
			if defaulted {
				origin = "default"
			}
			where := "tailnet"
			if bindLoopback {
				where = "127.0.0.1"
			}
			fmt.Fprintf(os.Stderr, "[teemd] messaging: webhook listener on %s%s (%s)\n", where, addr, origin)
		} else {
			fmt.Fprintf(os.Stderr, "[teemd] messaging: telegram enabled but couldn't derive webhook port from --listen=%q; set messaging.telegram.webhook_port explicitly\n", df.listenAddr)
		}
	}

	d.endpoint = fmt.Sprintf("http://%s%s", mcpHost, normalizePort(df.listenAddr))
	d.token = os.Getenv("TEEM_WORKER_TOKEN")
	if d.token == "" {
		// Persistent worker token: stored in its own file so it
		// survives across daemon stop/start cycles. Workers spawned
		// by an earlier daemon hold this token in their environment;
		// rotating it on every start orphans every in-flight worker
		// behind a 401 wall and renders the "workers survive a daemon
		// bounce" guarantee aspirational. Rotation is now opt-in via
		// `rm ~/.teem/worker_token`.
		d.token = loadOrCreateWorkerToken()
	}

	if err := writeDaemonStateFile(daemonStateFile{
		PID:       os.Getpid(),
		Endpoint:  d.endpoint,
		Token:     d.token,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("daemon state: %w", err)
	}
	defer clearDaemonState()

	fmt.Fprintf(os.Stderr, "[teemd] endpoint: %s\n", d.endpoint)

	// Restore every team that has a registration.json on disk. Done
	// before HTTP starts serving so the first inbound request sees a
	// fully-rehydrated daemon (pulses auto-resumed, workers
	// reattached). Best-effort: bad rows are logged and skipped.
	d.restoreTeams()

	fmt.Fprintf(os.Stderr, "[teemd] ready. Stop with `teem stop` or kill %d\n", os.Getpid())

	httpSrv := &http.Server{
		Handler:           d.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if d.messagingWebhookListener != nil {
		d.messagingWebhookServer = &http.Server{
			Handler:           newWebhookHandler(d),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	// Retention GC: spawns only when an operator has explicitly
	// configured one of the TTL knobs. Default is "never delete"; the
	// daemon preserves all stopped-agent and transcript history
	// indefinitely unless opted in.
	retCfg := retention.LoadConfig()
	if retCfg.Enabled() {
		fmt.Fprintf(os.Stderr, "[teemd] retention: stopped_agent_ttl=%s transcript_ttl=%s sweep=%s\n",
			retCfg.StoppedAgentTTL, retCfg.TranscriptTTL, retentionSweepInterval(retCfg))
		safeGo("retention.gc", func() { d.runRetentionGC(retCfg) })
	}

	// Cross-project peer awareness (XP1): once per interval, write a
	// "what your peers are doing" digest into each leader's archmem
	// memory file. Enabled by default; TEEM_PEERAWARE_INTERVAL=0
	// disables. With a single team registered the loop becomes a no-op.
	if peerInterval := peerAwareConfig(); peerInterval > 0 {
		fmt.Fprintf(os.Stderr, "[teemd] peeraware: interval=%s\n", peerInterval)
		safeGo("peeraware.loop", func() { d.runPeerAware(peerInterval) })
	}

	// Branch cleanup: sweep dead teem/* branches every 12h by default.
	// Set TEEM_PRUNE_INTERVAL=0 to disable. Default is on with logging
	// so operators don't accumulate hundreds of stale branches.
	if pruneInterval := pruneSweepConfig(); pruneInterval > 0 {
		fmt.Fprintf(os.Stderr, "[teemd] prune-branches: interval=%s\n", pruneInterval)
		safeGo("prune.sweep", func() { d.runPruneSweep(pruneInterval) })
	}

	serverErr := make(chan error, 2)
	go func() {
		err := httpSrv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		} else {
			serverErr <- nil
		}
	}()
	if d.messagingWebhookServer != nil {
		safeGo("messaging.webhook:listener", func() {
			err := d.messagingWebhookServer.Serve(d.messagingWebhookListener)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- fmt.Errorf("webhook listener: %w", err)
			}
		})
	}
	if d.messagingTelegram != nil && d.messagingCfg.PublicURL != "" && d.messagingWebhookToken != "" {
		safeGo("messaging.webhook:auto-register", func() {
			d.autoRegisterTelegramWebhook(d.baseCtx)
		})
	}
	if d.messagingTelegram != nil && d.messagingCfg.FunnelViaTsnet && d.tnetNode == nil {
		fmt.Fprintf(os.Stderr, "[teemd] messaging: funnel_via_tsnet=true but tsnet disabled — ignoring (start daemon with tailnet to use Funnel)\n")
	}
	if d.messagingTelegram != nil && d.messagingCfg.FunnelViaTsnet && d.tnetNode != nil && d.messagingWebhookPort > 0 {
		safeGo("messaging.webhook:funnel", func() {
			d.enableTelegramFunnel(d.baseCtx)
		})
	}
	defer func() {
		// 1. Stop accepting new HTTP requests. Shut the public-facing
		//    webhook listener down FIRST so in-flight Telegram retries
		//    don't keep hammering us during drain, then drain the main
		//    listener in parallel. Both share the same 3s budget but
		//    run concurrently so a slow main drain doesn't starve the
		//    webhook of its shutdown deadline.
		ctx2, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// Tear down the public-facing Funnel mapping FIRST so the
		// tsnet node stops proxying inbound HTTPS to a webhook we're
		// about to kill. Best-effort: failure here just means Telegram
		// retries during drain land on a dead listener, the same as
		// the no-funnel path.
		if d.tnetNode != nil && d.messagingCfg.FunnelViaTsnet && d.messagingWebhookPort > 0 {
			if err := d.tnetNode.DisableFunnel(ctx2); err != nil {
				fmt.Fprintf(os.Stderr, "[teemd] messaging: tsnet Funnel teardown failed: %v\n", err)
			}
		}
		var wg sync.WaitGroup
		if d.messagingWebhookServer != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = d.messagingWebhookServer.Shutdown(ctx2)
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = httpSrv.Shutdown(ctx2)
		}()
		wg.Wait()

		// 2. Graceful drain — wait up to TEEM_DRAIN_TIMEOUT for
		//    in-flight worker jobs to finish. After this expires,
		//    anything still running gets killed by Spawner.Stop's
		//    context cancellation. The startup reconcile of the next
		//    daemon will emit job_interrupted for it.
		if drain := drainTimeout(); drain > 0 {
			fmt.Fprintf(os.Stderr, "[teemd] draining in-flight jobs (up to %s)...\n", drain)
			drainCtx, dcancel := context.WithTimeout(context.Background(), drain)
			d.mu.Lock()
			teams := make([]*registeredTeam, 0, len(d.teams))
			for _, rt := range d.teams {
				teams = append(teams, rt)
			}
			d.mu.Unlock()
			for _, rt := range teams {
				if err := rt.spawner.Drain(drainCtx); err != nil {
					fmt.Fprintf(os.Stderr, "[teemd] %s: drain timed out with %d job(s) still in flight\n", rt.team.Name, rt.spawner.TotalInFlight())
				}
			}
			dcancel()
		}

		// 3. Final teardown.
		d.mu.Lock()
		for _, rt := range d.teams {
			// PM-loop first: it writes to the audit sink we close
			// below and pokes the spawner we Stop next. d.baseCtx
			// is already cancelled by this point (the outer select
			// fired on ctx.Done), so the goroutine has likely already
			// exited — stopPMLoop just waits on the done channel.
			stopPMLoop(rt)
			// Daemon shutdown: preserve the pulse running-flag so
			// `teem start` auto-resumes Pulse on the next boot. Operator
			// opt-out goes through `teem pulse stop` (the flag-clearing
			// Stop variant), not through bouncing the daemon.
			rt.pulse.StopForShutdown()
			rt.spawner.Stop()
			_ = rt.auditSink.Close()
			_ = rt.plan.Close()
			_ = rt.notes.Close()
			if rt.archMemCancel != nil {
				rt.archMemCancel()
			}
			if rt.inFlight != nil {
				_ = rt.inFlight.Close()
			}
		}
		d.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-serverErr:
		return err
	}
}

// handler is the daemon's top-level HTTP dispatcher.
//
//	/healthz                        liveness
//	/control/teams         (POST)   register a team
//	/control/teams         (GET)    list registered teams
//	/control/teams/<name>  (DELETE) unregister a team
//	/teams/<name>/mcp/*             that team's MCP server
//	/teams/<name>/audit             that team's audit endpoint
//
// /control and /teams routes require bearer auth via d.token.
func (d *daemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		case path == "/control/teams":
			d.requireAuth(w, r, d.handleControlTeamsCollection)
		case strings.HasPrefix(path, "/control/teams/") && strings.HasSuffix(path, "/ping"):
			// Dashboard "Ping leader" button. Unauth on purpose: shares
			// the dashboard's localhost-only security model (no
			// per-user auth yet). When the daemon binds to tailnet
			// rather than 127.0.0.1, anyone on the tailnet can hit
			// this — same boundary as the dashboard itself.
			d.handlePingTeam(w, r)
		case strings.HasPrefix(path, "/control/teams/") && isDashboardPulseAction(path):
			// Dashboard "Pulse" panel form posts (start/stop/config
			// sub-paths). Same unauth rationale as /ping — the
			// dashboard is the localhost / tailnet UI surface and
			// can't carry the bearer token. The GET on /pulse stays
			// auth'd; only the action sub-paths are exempt.
			d.handleControlTeamsItem(w, r)
		case strings.HasPrefix(path, "/control/teams/") && isDashboardTaskReadyAction(path):
			// Dashboard "→ ready" button. Same tailnet-boundary auth
			// model as the pulse actions: the SPA's fetch can't carry
			// the bearer token, so the unauth path is gated on the
			// `/tasks/<id>/ready` suffix shape.
			d.handleControlTeamsItem(w, r)
		case strings.HasPrefix(path, "/control/teams/") && strings.HasSuffix(path, "/chat"):
			// Dashboard chat panel. Same tailnet-boundary auth model as
			// /ping; spawns a one-shot leader `claude -p` and streams
			// the response as SSE.
			d.handleChatTeam(w, r)
		case path == messaging.WebhookPath:
			// Inbound Telegram bot updates. Auth is the daemon-issued
			// ?token=<random> URL parameter (weak); the tailnet
			// boundary is the load-bearing layer. Token rotates on
			// daemon restart so leaks expire on the next bounce.
			d.handleTelegramWebhook(w, r)
		case strings.HasPrefix(path, "/control/teams/"):
			d.requireAuth(w, r, d.handleControlTeamsItem)
		case strings.HasPrefix(path, "/api/teams/"):
			d.handleAPITeamRoute(w, r)
		case strings.HasPrefix(path, "/teams/"):
			d.handleTeamRoute(w, r)
		case path == "/" || path == "/ui" || path == "/ui/":
			// Tiny static team index. The SPA lives under
			// /teams/<id>/; this page exists so the operator's bookmark
			// at the daemon root still has something to click. Unauth
			// on purpose: tailnet is the security boundary.
			d.renderTeamIndex(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// isDashboardPulseAction reports whether the request path matches one
// of the dashboard's pulse-panel form-post sub-paths
// (/control/teams/<id>/pulse/{start,stop,config}). Those endpoints are
// served unauth so the dashboard's HTML form can hit them; the bare
// /pulse GET stays auth'd.
func isDashboardPulseAction(path string) bool {
	for _, suffix := range []string{"/pulse/start", "/pulse/stop", "/pulse/config"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// isDashboardTaskReadyAction reports whether path is the
// `/control/teams/<id>/tasks/<task_id>/ready` shape (and only that
// shape). Used to gate the dashboard's "→ ready" button onto the
// unauth-bypass list alongside pulse actions.
func isDashboardTaskReadyAction(path string) bool {
	if !strings.HasSuffix(path, "/ready") {
		return false
	}
	rest := strings.TrimPrefix(path, "/control/teams/")
	parts := strings.Split(rest, "/")
	// <team-key>/tasks/<task-id>/ready → 4 parts.
	return len(parts) == 4 && parts[1] == "tasks" && parts[3] == "ready"
}

// renderTeamIndex writes a multi-team overview page: one card per
// registered team showing leader status, task counts, in-flight
// workers, and today's spend. Pure inline HTML + inline <style> by
// design — Phase 4 deleted html/template (binary -28%), so this page
// stays template-free. Every dynamic value is run through
// html.EscapeString; the page is unauth on purpose (tailnet is the
// security boundary).
func (d *daemon) renderTeamIndex(w http.ResponseWriter, _ *http.Request) {
	views := d.snapshotTeams()
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	_, _ = io.WriteString(w, teamIndexHead)

	if len(views) == 0 {
		_, _ = io.WriteString(w, `<main><h1>teem</h1><p class="empty">No teams registered.</p></main>`)
		return
	}

	_, _ = io.WriteString(w, `<main><h1>teem</h1><div class="grid">`)
	for _, v := range views {
		writeTeamIndexCard(w, d.snapshotTeamCard(v))
	}
	_, _ = io.WriteString(w, `</div></main>`)
}

// teamIndexCard is the per-card data the index page renders. Derived
// from a full teamSnapshot — picking just the fields the card surface
// shows — so it tracks dashboardTeam without re-implementing counters.
type teamIndexCard struct {
	ID               string
	Name             string
	RegisteredAgo    string
	OpenTaskCount    int
	AwaitingApproval int
	RecentDoneCount  int
	InFlight         int64
	LeaderText       string
	LeaderAgo        string
	HasLeaderStatus  bool
	HasPricing       bool
	HeroSpendDisplay string
	PricingStale     bool
}

// snapshotTeamCard builds the card payload for one team by walking the
// full teamSnapshot. Reusing teamSnapshot keeps the index in lockstep
// with the per-team page (same counters, same spend formula).
func (d *daemon) snapshotTeamCard(v teamView) teamIndexCard {
	team := teamSnapshot(v)
	card := teamIndexCard{
		ID:               team.ID,
		Name:             team.Name,
		RegisteredAgo:    team.RegisteredAgo,
		OpenTaskCount:    team.OpenTaskCount,
		AwaitingApproval: len(team.AwaitingApproval),
		RecentDoneCount:  len(team.RecentDone),
		InFlight:         team.InFlight,
		HasPricing:       team.HasPricing,
		HeroSpendDisplay: team.HeroSpendDisplay,
		PricingStale:     team.PricingStale,
	}
	if team.LeaderStatus != nil {
		card.HasLeaderStatus = true
		card.LeaderText = team.LeaderStatus.Text
		card.LeaderAgo = team.LeaderStatus.UpdatedAgo
	}
	return card
}

const teamIndexHead = `<!doctype html><html lang="en"><head>` +
	`<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
	`<title>teem</title>` +
	`<style>` +
	`:root{--bg:#fafafa;--fg:#1f2937;--muted:#6b7280;--card:#ffffff;--border:#e5e7eb;` +
	`--accent:#2563eb;--ok:#16a34a;--warn:#d97706;--stale:#9ca3af}` +
	`@media (prefers-color-scheme:dark){:root{--bg:#0b0f17;--fg:#e5e7eb;--muted:#9ca3af;` +
	`--card:#111827;--border:#1f2937;--accent:#60a5fa;--ok:#34d399;--warn:#fbbf24;--stale:#6b7280}}` +
	`*{box-sizing:border-box}` +
	`body{margin:0;background:var(--bg);color:var(--fg);` +
	`font:14px/1.45 -apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif}` +
	`main{max-width:72rem;margin:0 auto;padding:2rem 1.25rem}` +
	`h1{font-size:1.5rem;margin:0 0 1.25rem;letter-spacing:-.01em}` +
	`.empty{color:var(--muted)}` +
	`.grid{display:grid;gap:1rem;grid-template-columns:repeat(auto-fill,minmax(20rem,1fr))}` +
	`.card{background:var(--card);border:1px solid var(--border);border-radius:.5rem;` +
	`padding:1rem 1.1rem;text-decoration:none;color:inherit;display:block;` +
	`transition:border-color .12s ease,transform .12s ease}` +
	`.card:hover{border-color:var(--accent);transform:translateY(-1px)}` +
	`.card-head{display:flex;justify-content:space-between;align-items:baseline;gap:.5rem;margin-bottom:.5rem}` +
	`.card-name{font-weight:600;font-size:1.05rem;letter-spacing:-.01em}` +
	`.card-meta{color:var(--muted);font-size:.78rem}` +
	`.counters{display:grid;grid-template-columns:repeat(4,1fr);gap:.5rem;margin:.6rem 0 .75rem}` +
	`.counter{display:flex;flex-direction:column;align-items:flex-start;gap:.1rem}` +
	`.counter .n{font-size:1.25rem;font-weight:600;font-variant-numeric:tabular-nums;line-height:1}` +
	`.counter .l{font-size:.7rem;color:var(--muted);text-transform:uppercase;letter-spacing:.04em}` +
	`.counter.accent .n{color:var(--accent)}` +
	`.counter.warn .n{color:var(--warn)}` +
	`.leader{margin-top:.55rem;padding-top:.55rem;border-top:1px dashed var(--border);` +
	`color:var(--muted);font-size:.85rem;display:flex;justify-content:space-between;gap:.5rem;align-items:baseline}` +
	`.leader-text{color:var(--fg);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;flex:1;min-width:0}` +
	`.leader-ago{color:var(--muted);font-size:.75rem;flex-shrink:0}` +
	`.leader.empty-leader{color:var(--stale);font-style:italic}` +
	`.spend{margin-top:.5rem;display:flex;justify-content:space-between;align-items:baseline;gap:.5rem;font-size:.85rem}` +
	`.spend .label{color:var(--muted);text-transform:uppercase;letter-spacing:.04em;font-size:.7rem}` +
	`.spend .val{font-variant-numeric:tabular-nums;font-weight:500}` +
	`.spend .stale{color:var(--stale);font-size:.7rem;margin-left:.25rem}` +
	`</style></head><body>`

func writeTeamIndexCard(w io.Writer, c teamIndexCard) {
	id := html.EscapeString(c.ID)
	name := html.EscapeString(c.Name)
	registered := html.EscapeString(c.RegisteredAgo)

	fmt.Fprintf(w, `<a class="card" href="/teams/%s/">`, id)
	fmt.Fprintf(w, `<div class="card-head"><span class="card-name">%s</span>`, name)
	if registered != "" {
		fmt.Fprintf(w, `<span class="card-meta">registered %s</span>`, registered)
	}
	_, _ = io.WriteString(w, `</div>`)

	_, _ = io.WriteString(w, `<div class="counters">`)
	openClass := "counter"
	if c.OpenTaskCount > 0 {
		openClass = "counter accent"
	}
	fmt.Fprintf(w, `<div class="%s"><span class="n">%d</span><span class="l">Open</span></div>`,
		openClass, c.OpenTaskCount)
	awaitingClass := "counter"
	if c.AwaitingApproval > 0 {
		awaitingClass = "counter warn"
	}
	fmt.Fprintf(w, `<div class="%s"><span class="n">%d</span><span class="l">Awaiting</span></div>`,
		awaitingClass, c.AwaitingApproval)
	inflightClass := "counter"
	if c.InFlight > 0 {
		inflightClass = "counter accent"
	}
	fmt.Fprintf(w, `<div class="%s"><span class="n">%d</span><span class="l">In-flight</span></div>`,
		inflightClass, c.InFlight)
	fmt.Fprintf(w, `<div class="counter"><span class="n">%d</span><span class="l">Done</span></div>`,
		c.RecentDoneCount)
	_, _ = io.WriteString(w, `</div>`)

	if c.HasLeaderStatus && strings.TrimSpace(c.LeaderText) != "" {
		text := html.EscapeString(truncateForTile(c.LeaderText, 120))
		ago := html.EscapeString(c.LeaderAgo)
		fmt.Fprintf(w, `<div class="leader"><span class="leader-text">%s</span>`+
			`<span class="leader-ago">%s</span></div>`, text, ago)
	} else {
		_, _ = io.WriteString(w, `<div class="leader empty-leader"><span class="leader-text">No leader status yet.</span></div>`)
	}

	if c.HasPricing && c.HeroSpendDisplay != "" {
		stale := ""
		if c.PricingStale {
			stale = `<span class="stale">(stale)</span>`
		}
		fmt.Fprintf(w, `<div class="spend"><span class="label">Today's spend</span>`+
			`<span class="val">%s%s</span></div>`,
			html.EscapeString(c.HeroSpendDisplay), stale)
	}

	_, _ = io.WriteString(w, `</a>`)
}

func (d *daemon) requireAuth(w http.ResponseWriter, r *http.Request, h func(http.ResponseWriter, *http.Request)) {
	if r.Header.Get("Authorization") != "Bearer "+d.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h(w, r)
}
func (d *daemon) handleControlTeamsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		d.handleRegister(w, r)
	case http.MethodGet:
		d.handleListTeams(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *daemon) handleControlTeamsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/control/teams/")
	if rest == "" {
		http.Error(w, "bad team id", http.StatusBadRequest)
		return
	}
	// Split into <key> (id or name alias) and optional <subresource>.
	key := rest
	sub := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		key, sub = rest[:i], rest[i+1:]
	}
	rt := d.resolveTeam(key)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	// Subsequent map mutations / state-file paths must use the
	// canonical id, not the URL key (which may be a slug alias).
	id := rt.team.ID

	switch {
	case sub == "":
		// Whole-team operations.
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Stop the PM-loop goroutine BEFORE removing from d.teams so
		// the goroutine can't observe a half-torn-down team. The audit
		// sink and spawner it holds onto are still alive at this point.
		stopPMLoop(rt)
		d.mu.Lock()
		delete(d.teams, id)
		d.mu.Unlock()
		rt.pulse.Stop()
		rt.spawner.Stop()
		_ = rt.auditSink.Close()
		_ = rt.plan.Close()
		_ = rt.notes.Close()
		if rt.archMemCancel != nil {
			rt.archMemCancel()
		}
		if rt.inFlight != nil {
			_ = rt.inFlight.Close()
		}
		removeTeamRegistration(id)
		d.persistStateSnapshot()
		w.WriteHeader(http.StatusNoContent)
	case sub == "pulse":
		d.handlePulseControl(w, r, rt, "")
	case strings.HasPrefix(sub, "pulse/"):
		d.handlePulseControl(w, r, rt, strings.TrimPrefix(sub, "pulse/"))
	case strings.HasPrefix(sub, "tasks/") && strings.HasSuffix(sub, "/ready"):
		// `tasks/<id>/ready` is split out from the decision-action
		// handler because it has different semantics: no body, idempotent
		// on re-post, gated on a different (non-awaiting_approval) set
		// of source stages.
		inner := strings.TrimSuffix(strings.TrimPrefix(sub, "tasks/"), "/ready")
		d.handleControlTaskReady(w, r, rt, inner)
	case strings.HasPrefix(sub, "tasks/"):
		d.handleControlTaskAction(w, r, rt, strings.TrimPrefix(sub, "tasks/"))
	default:
		http.NotFound(w, r)
	}
}

// pulseStatus is the GET response shape and the start-result shape.
// WakePrompt carries the active first-turn message; UseDefaultWakePrompt
// is true when the operator hasn't supplied an override (useful as a
// dashboard hint to render the textarea as a placeholder rather than a
// pre-filled value).
type pulseStatus struct {
	Running              bool      `json:"running"`
	Paused               bool      `json:"paused"`
	Interval             string    `json:"interval"`
	LastTick             time.Time `json:"last_tick,omitempty"`
	TickCount            int64     `json:"tick_count"`
	WakePrompt           string    `json:"wake_prompt"`
	UseDefaultWakePrompt bool      `json:"use_default_wake_prompt"`
	DefaultWakePrompt    string    `json:"default_wake_prompt"`
}

// pulseCommand is the POST body for action-style requests. WakePrompt
// is *string so callers can distinguish "leave alone" (nil) from
// "clear override / fall back to default" (empty string).
type pulseCommand struct {
	Action     string  `json:"action"`   // start|stop|pause|resume|tick|config (action also derived from URL sub-path)
	Interval   string  `json:"interval"` // for start/config; Go duration string
	Reason     string  `json:"reason"`   // for pause
	WakePrompt *string `json:"wake_prompt,omitempty"`
}

// handlePulseControl handles GET/POST under /control/teams/<id>/pulse,
// including the sub-paths /pulse/start, /pulse/stop, /pulse/config used
// by the dashboard's pulse panel. subAction (when non-empty) overrides
// any cmd.Action in the body so form-style POSTs don't need to repeat
// the verb.
func (d *daemon) handlePulseControl(w http.ResponseWriter, r *http.Request, rt *registeredTeam, subAction string) {
	switch r.Method {
	case http.MethodGet:
		if subAction != "" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, currentPulseStatus(rt))
	case http.MethodPost:
		cmd, err := decodePulseCommand(r)
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		action := cmd.Action
		if subAction != "" {
			action = subAction
		}
		switch action {
		case "start":
			if cmd.Interval != "" {
				dur, err := time.ParseDuration(cmd.Interval)
				if err != nil {
					http.Error(w, "bad interval: "+err.Error(), http.StatusBadRequest)
					return
				}
				rt.pulse.SetInterval(dur)
			}
			if cmd.WakePrompt != nil {
				if err := rt.pulse.SetWakePrompt(*cmd.WakePrompt); err != nil {
					http.Error(w, "set wake prompt: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			rt.pulse.Start(d.baseCtx)
		case "stop":
			rt.pulse.Stop()
		case "pause":
			if err := rt.pulse.Pause(cmd.Reason); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "resume":
			if err := rt.pulse.Resume(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "tick":
			// Synchronous one-off tick — useful for `teem pulse tick`
			// and for testing. Runs on a background context so the
			// HTTP request returning doesn't cancel the tick.
			safeGo("pulse.tick:"+rt.team.Name, func() { _ = rt.pulse.Tick(d.baseCtx, "manual") })
		case "config":
			var dur time.Duration
			if cmd.Interval != "" {
				parsed, err := time.ParseDuration(cmd.Interval)
				if err != nil {
					http.Error(w, "bad interval: "+err.Error(), http.StatusBadRequest)
					return
				}
				dur = parsed
			}
			if err := rt.pulse.UpdateConfig(dur, cmd.WakePrompt); err != nil {
				http.Error(w, "update config: "+err.Error(), http.StatusInternalServerError)
				return
			}
		default:
			http.Error(w, "unknown action: "+action, http.StatusBadRequest)
			return
		}
		// Pulse controls are now driven by the SPA via fetch+JSON; the
		// form-post HTML branch is dead. Always return JSON.
		writeJSON(w, http.StatusOK, currentPulseStatus(rt))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// decodePulseCommand reads a pulseCommand from either an
// application/x-www-form-urlencoded body (dashboard form posts) or a
// JSON body (CLI / programmatic callers). Empty bodies decode to a
// zero-value command so callers can still drive the URL sub-action.
func decodePulseCommand(r *http.Request) (pulseCommand, error) {
	var cmd pulseCommand
	ctype := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	ctype = strings.TrimSpace(ctype)
	switch ctype {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		if err := r.ParseForm(); err != nil {
			return cmd, err
		}
		cmd.Action = r.FormValue("action")
		cmd.Reason = r.FormValue("reason")
		// Dashboard splits interval into number + unit; either form
		// works here. A bare `interval` value wins if both are sent.
		if v := r.FormValue("interval"); v != "" {
			cmd.Interval = v
		} else if num := r.FormValue("interval_value"); num != "" {
			unit := r.FormValue("interval_unit")
			if unit == "" {
				unit = "m"
			}
			cmd.Interval = num + unit
		}
		// Form-post wake-prompt: the textarea is always present in
		// the form, even when blank — that's how the operator clears
		// an override. Detect "submitted as part of the form" via the
		// PostForm map so a missing field doesn't accidentally clear.
		if r.PostForm.Has("wake_prompt") {
			v := r.PostForm.Get("wake_prompt")
			cmd.WakePrompt = &v
		}
	default:
		if r.Body == nil {
			return cmd, nil
		}
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil && err != io.EOF {
			return cmd, err
		}
	}
	return cmd, nil
}

// handlePingTeam serves POST /control/teams/<id>/ping — the dashboard's
// manual "Ping leader" button. Fires one pulse tick (trigger=manual)
// when the team isn't paused and no tick is in flight, otherwise
// returns a status that the dashboard turns into a flash message.
//
// Auth: localhost-only dashboard, no per-user auth yet. Operator
// identity isn't carried; the audit event uses agent_id="operator" as
// a placeholder until we have real auth.
func (d *daemon) handlePingTeam(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Path is /control/teams/<id>/ping.
	rest := strings.TrimPrefix(r.URL.Path, "/control/teams/")
	key := strings.TrimSuffix(rest, "/ping")
	if key == "" || strings.ContainsRune(key, '/') {
		http.Error(w, "bad team id", http.StatusBadRequest)
		return
	}
	rt := d.resolveTeam(key)
	if rt == nil {
		http.NotFound(w, r)
		return
	}

	if rt.pulse == nil {
		http.Error(w, "pulse not configured", http.StatusInternalServerError)
		return
	}
	// channels-live = operator chat is active. Route the ping as a
	// channel nudge into the running chat session instead of starting
	// a fresh claude subprocess — same wake signal, no session race.
	if rt.channelsLive.Load() {
		publishPulseChannelNudge(rt.channelBus)
		if rt.auditSink != nil {
			_ = rt.auditSink.Write(audit.Event{
				Timestamp: time.Now().UTC(),
				AgentID:   "operator",
				Kind:      audit.Kind("pulse_tick"),
				Message:   "manual ping routed as channel nudge",
				Meta:      map[string]any{"trigger": "manual", "route": "channel"},
			})
		}
		d.pingRespond(w, http.StatusOK,
			"operator chat session is active — ping delivered as a channel nudge")
		return
	}
	if rt.pulse.Paused() {
		d.pingRespond(w, http.StatusConflict,
			"pulse paused; `teem pulse resume` first")
		return
	}
	if rt.pulse.Busy() {
		d.pingRespond(w, http.StatusAccepted,
			"tick already in progress")
		return
	}

	if rt.auditSink != nil {
		_ = rt.auditSink.Write(audit.Event{
			Timestamp: time.Now().UTC(),
			AgentID:   "operator",
			Kind:      audit.KindPulseTick,
			Message:   "manual ping from dashboard",
			Meta:      map[string]any{"trigger": "manual"},
		})
	}
	safeGo("pulse.ping:"+rt.team.ID, func() { _ = rt.pulse.Tick(d.baseCtx, "manual") })
	d.pingRespond(w, http.StatusOK, "ping queued")
}

// pingRespond writes the plain-text ping outcome. The SPA reads this
// via fetch; there is no SSR HTML branch since the dashboard form-post
// path was removed.
func (d *daemon) pingRespond(w http.ResponseWriter, code int, body string) {
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func currentPulseStatus(rt *registeredTeam) pulseStatus {
	wp := rt.pulse.WakePrompt()
	custom := rt.pulse.IsCustomWakePrompt()
	return pulseStatus{
		Running:              rt.pulse.Running(),
		Paused:               rt.pulse.Paused(),
		Interval:             rt.pulse.Interval().String(),
		LastTick:             rt.pulse.LastTick(),
		TickCount:            rt.pulse.TickCount(),
		WakePrompt:           wp,
		UseDefaultWakePrompt: !custom,
		DefaultWakePrompt:    pulse.DefaultWakePrompt(),
	}
}

func (d *daemon) handleListTeams(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]map[string]any, 0, len(d.teams))
	for id, rt := range d.teams {
		out = append(out, map[string]any{
			"id":         id,
			"name":       rt.team.Name,
			"mcp_url":    d.endpoint + "/teams/" + id + "/mcp",
			"audit_url":  d.endpoint + "/teams/" + id + "/audit",
			"registered": rt.registered.Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// newAuditHandlerWithRegistry wraps an audit handler so each accepted
// POST body is parsed once and its events get fanned out to the
// registry (LastSeen update). The hook chain fires from the hooked
// sink the inner handler writes through, so this middleware no longer
// re-fires it.
func newAuditHandlerWithRegistry(inner http.Handler, reg *mcpsrv.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err == nil {
				var events []audit.Event
				if json.Unmarshal(body, &events) == nil {
					now := time.Now().UTC()
					for _, e := range events {
						ts := e.Timestamp
						if ts.IsZero() {
							ts = now
						}
						reg.SetLastSeen(e.AgentID, ts)
					}
				}
				r.Body = newBytesReadCloser(body)
			}
		}
		inner.ServeHTTP(w, r)
	})
}

// newBytesReadCloser returns an io.ReadCloser that hands out the
// supplied bytes. Used to restore r.Body after we read it for the
// audit tee.
func newBytesReadCloser(body []byte) io.ReadCloser {
	return &bytesReadCloser{r: strings.NewReader(string(body))}
}

type bytesReadCloser struct{ r *strings.Reader }

func (b *bytesReadCloser) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *bytesReadCloser) Close() error               { return nil }

// --- /teams/<name>/* handlers ---------------------------------------------

// resolveTeam looks a team up by its canonical id (the `t-<hex>`
// routing key) or, as a fallback, by its display name. The id match
// always wins, so when two teams happen to share a Name (rare, since
// init/register checks for it) the alias is best-effort and won't
// shadow an id lookup. Returns nil if no team matches.
//
// The name alias exists so URLs that long-lived clients captured
// before the T33 / TI1 migration — when the daemon keyed `d.teams`
// by t.Name — still resolve after a restart. Concretely: a `teem
// chat` Claude Code subprocess holds a stale `/teams/<old-name>/mcp`
// URL; the alias keeps that handshake alive instead of forcing a
// reconnect.
func (d *daemon) resolveTeam(key string) *registeredTeam {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rt, ok := d.teams[key]; ok {
		return rt
	}
	for _, rt := range d.teams {
		if rt.team != nil && rt.team.Name == key {
			return rt
		}
	}
	return nil
}

// handleTeamRoute dispatches /teams/<id>/(mcp|audit) to the matching
// per-team handler after stripping the team prefix from the request
// path. The MCP handler is path-agnostic; the audit handler reads
// "/audit" so we rewrite the URL.Path accordingly.
func (d *daemon) handleTeamRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/teams/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		// Phase 3: bare /teams/<id> serves the SPA. Redirect to the
		// trailing-slash form so the SPA's relative ./assets/... refs
		// resolve under /teams/<id>/ rather than /teams/.
		if rest == "" {
			http.NotFound(w, r)
			return
		}
		if d.resolveTeam(rest) == nil {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/teams/"+rest+"/", http.StatusSeeOther)
		return
	}
	id, suffix := rest[:slash], rest[slash:]
	rt := d.resolveTeam(id)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case suffix == "/":
		// SPA shell (trailing-slash form of bare /teams/<id>).
		serveSPA(w, r, "")
	case strings.HasPrefix(suffix, "/assets/"):
		// SPA static assets emitted by Vite (base: './'), resolved by
		// the browser against /teams/<id>/.
		serveSPA(w, r, suffix)
	case strings.HasPrefix(suffix, "/mcp"):
		// Forward to the team's MCP handler. It expects to see /mcp at
		// the root.
		r2 := r.Clone(r.Context())
		r2.URL.Path = suffix
		rt.mcp.Handler().ServeHTTP(w, r2)
	case suffix == "/audit" || strings.HasPrefix(suffix, "/audit?") || suffix == "/audit/":
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/audit"
		rt.auditH.ServeHTTP(w, r2)
	case strings.HasPrefix(suffix, "/transcripts/"):
		// GET /teams/<id>/transcripts/<agent>/<job> serves the SPA so a
		// browser following the participation-log link lands on a
		// rendered transcript page (the SPA fetches the raw NDJSON from
		// the /api/ path under the hood). POST stays on the bearer-auth
		// mirror path — workers/leader writing transcripts back to disk.
		rest := strings.TrimPrefix(suffix, "/transcripts/")
		if r.Method == http.MethodGet && isTranscriptPageRest(rest) {
			// Inject <base> so the SPA's relative ./assets/... refs
			// resolve under /teams/<id>/ rather than the transcript
			// URL's directory, which would 401 against the bearer-auth
			// transcripts mirror.
			serveSPAWithBase(w, r, "", "/teams/"+id+"/")
			return
		}
		d.handleTranscripts(w, r, rt, rest)
	case suffix == "/channel-events" || strings.HasPrefix(suffix, "/channel-events?"):
		d.handleChannelEvents(w, r, rt)
	case suffix == "/v2" || suffix == "/v2/" || strings.HasPrefix(suffix, "/v2/"):
		// Transitional alias for any bookmarks captured during Phase 1/2;
		// both /v2[/...] and bare /teams/<id> now serve the same bundle.
		rest := strings.TrimPrefix(suffix, "/v2")
		serveSPA(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

// validIDRegexp matches the agent_id / job_id forms accepted on the
// transcripts route. Restricted to letters, digits, and `.`/`_`/`-`
// so URL handlers can't be tricked into writing outside the team's
// transcripts directory. isSafeID layers on top of the regex to
// reject the dot-only literals `.` and `..` which the character class
// would otherwise accept and which filepath.Join resolves to a path
// escape.
var validIDRegexp = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func isSafeID(s string) bool {
	return validIDRegexp.MatchString(s) && s != "." && s != ".."
}

// isTranscriptPageRest reports whether the path tail after
// "/teams/<id>/transcripts/" is the two-segment "<agent>/<job>" form
// the SPA's <TranscriptPage> renders. Anything else — sub-paths
// (/watch), missing segments, malformed ids — falls through to the
// legacy bearer-auth mirror handler so we don't mask 400s with an
// SPA shell.
func isTranscriptPageRest(rest string) bool {
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return false
	}
	agentID, jobID := rest[:slash], rest[slash+1:]
	if jobID == "" || strings.Contains(jobID, "/") {
		return false
	}
	return isSafeID(agentID) && isSafeID(jobID)
}

// handleTranscripts implements GET/POST /teams/<name>/transcripts/<agent>/<job>.
// Bearer-auth gated (same shared token as /audit). POST writes the body
// to the team's transcripts mirror; GET serves it back. ?head=N on GET
// returns the first N NDJSON events (lines) rather than the whole body.
func (d *daemon) handleTranscripts(w http.ResponseWriter, r *http.Request, rt *registeredTeam, rest string) {
	if r.Header.Get("Authorization") != "Bearer "+d.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		http.Error(w, "want /transcripts/<agent_id>/<job_id>", http.StatusBadRequest)
		return
	}
	agentID, jobID := rest[:slash], rest[slash+1:]
	if !isSafeID(agentID) || !isSafeID(jobID) {
		http.Error(w, "bad agent_id or job_id (must match [A-Za-z0-9._-]+ and not be . or ..)", http.StatusBadRequest)
		return
	}
	if rt.transcriptsDir == "" {
		http.Error(w, "transcripts not configured", http.StatusInternalServerError)
		return
	}
	path := filepath.Join(rt.transcriptsDir, agentID, jobID+".jsonl")
	switch r.Method {
	case http.MethodPost:
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			http.Error(w, "mkdir: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// MaxBytesReader (not LimitReader) — surfaces the cap as a
		// returned error so we can 413 instead of silently truncating
		// a too-large upload.
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024*1024)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "transcript too large (>64 MiB)", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := atomicWrite(path, body); err != nil {
			http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "read: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		if h := r.URL.Query().Get("head"); h != "" {
			n, err := strconv.Atoi(h)
			if err != nil || n < 0 {
				http.Error(w, "bad head", http.StatusBadRequest)
				return
			}
			body = headLines(body, n)
		}
		_, _ = w.Write(body)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// observeChannelSubscribe subscribes a new SSE handler to the team's
// channelbus and drives the channels-live ↔ fallback state machine.
// It is the single place that mutates rt.channelsLive: the flag, the
// audit transition event, and the pulse gate are all flipped together
// under detectionMu, so concurrent connect/disconnect races converge
// on a single transition emission. The returned cancel unsubscribes
// and runs the symmetric fallback transition when the last subscriber
// leaves.
//
// Why the mutex wraps cancel() (not just the bool flip): without it,
// a concurrent subscribe between cancel()'s "I am the last" snapshot
// and the flag mutation would see channelsLive=true and skip its own
// 0→1 transition, then this cancel would clear the flag and emit
// "fallback" while the new subscriber is in fact live. Holding
// detectionMu across both the channelbus mutation AND the flag
// mutation linearizes the decisions.
func (d *daemon) observeChannelSubscribe(rt *registeredTeam) (<-chan channelbus.Event, func()) {
	if rt.channelBus == nil {
		closed := make(chan channelbus.Event)
		close(closed)
		return closed, func() {}
	}
	_, ch, count, cancelSub := rt.channelBus.SubscribeAndCount()
	rt.detectionMu.Lock()
	if count == 1 && !rt.channelsLive.Load() {
		rt.channelsLive.Store(true)
		if rt.pulse != nil {
			rt.pulse.SetChannelsLive(true)
		}
		if rt.auditSink != nil {
			_ = rt.auditSink.Write(audit.Event{
				Timestamp: time.Now().UTC(),
				AgentID:   "leader",
				Kind:      audit.KindChannelsState,
				Meta:      map[string]any{"state": "live", "team": rt.team.ID},
			})
		}
	}
	rt.detectionMu.Unlock()
	cancel := func() {
		rt.detectionMu.Lock()
		defer rt.detectionMu.Unlock()
		post := cancelSub()
		if post == 0 && rt.channelsLive.Load() {
			rt.channelsLive.Store(false)
			if rt.pulse != nil {
				rt.pulse.SetChannelsLive(false)
			}
			if rt.auditSink != nil {
				_ = rt.auditSink.Write(audit.Event{
					Timestamp: time.Now().UTC(),
					AgentID:   "leader",
					Kind:      audit.KindChannelsState,
					Meta:      map[string]any{"state": "fallback", "team": rt.team.ID},
				})
			}
		}
	}
	return ch, cancel
}

// handleChannelEvents serves the team's channel-event SSE stream to a
// teem-channel stdio shim. The shim runs as a subprocess of Claude
// Code, holds open one GET against this endpoint, and re-emits every
// received Event as a notifications/claude/channel message on its
// stdio MCP transport — which is what claude actually listens on for
// channels. Bearer-auth (same worker_token as /audit).
//
// Event wire format: SSE frames with event name "channel" and a JSON
// data line { "content": "...", "meta": { "agent_id": "...", ... } }.
// A periodic ":keepalive\n\n" comment keeps the connection through
// upstream idle timeouts.
func (d *daemon) handleChannelEvents(w http.ResponseWriter, r *http.Request, rt *registeredTeam) {
	if r.Header.Get("Authorization") != "Bearer "+d.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if rt.channelBus == nil {
		http.Error(w, "channel bus not configured", http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := d.observeChannelSubscribe(rt)
	defer cancel()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := io.WriteString(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: channel\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// headLines returns the first n NDJSON lines from body (preserving
// trailing newlines). n <= 0 returns body unchanged.
func headLines(body []byte, n int) []byte {
	if n <= 0 {
		return body
	}
	count := 0
	for i, c := range body {
		if c == '\n' {
			count++
			if count == n {
				return body[:i+1]
			}
		}
	}
	return body
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
