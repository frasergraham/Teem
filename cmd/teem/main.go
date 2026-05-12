package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/frasergraham/teem/internal/agent"
	"github.com/frasergraham/teem/internal/audit"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/leader"
	"github.com/frasergraham/teem/internal/llm"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
	"github.com/frasergraham/teem/internal/provisioner"
	"github.com/frasergraham/teem/internal/state"
	"github.com/frasergraham/teem/internal/tailnet"
	"github.com/frasergraham/teem/internal/team"
	"github.com/frasergraham/teem/internal/transport"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	var err error
	switch sub {
	case "chat":
		err = runChat(args)
	case "init":
		err = runInit(args)
	case "audit":
		err = runAudit(args)
	case "llm":
		err = runLLM(args)
	case "version":
		fmt.Println(versionString())
	case "-h", "--help", "help":
		fmt.Println(usage())
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s\n", sub, usage())
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() string {
	return strings.TrimSpace(`
teem — orchestrate Claude Code agents as a team

Usage:
  teem init                                            show current team or run the setup wizard
  teem chat    [--team teem.yaml] [--tailnet=true] [--leader-host user@host] [--listen :7777]
  teem audit   [--agent ID] [--since RFC3339] [--limit 50] [--follow]
  teem llm ping --prompt "say hi"
  teem version

Run 'teem <subcommand> -h' for flags.
`)
}

func versionString() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

// runChat starts the orchestrator and the Leader, then runs a REPL.
func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	useTailnet := fs.Bool("tailnet", true, "join the tailnet via tsnet")
	leaderHost := fs.String("leader-host", "", "if set (user@host), run the Leader on that host via SSH; otherwise local")
	listenAddr := fs.String("listen", ":7777", "address the orchestrator MCP server listens on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolved, err := resolveTeamPath(*teamPath)
	if err != nil {
		return err
	}
	t, err := team.Load(resolved)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		listener  net.Listener
		mcpHost   string
		tnetNode  *tailnet.Node
	)
	if *useTailnet {
		tnetNode, err = tailnet.New(tailnet.Config{
			Hostname: t.Tailnet.Hostname,
			AuthKey:  resolveAuthKey(t.Tailnet.AuthKeyEnv),
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[teem] joining tailnet as %q...\n", t.Tailnet.Hostname)
		if err := tnetNode.Start(ctx); err != nil {
			return err
		}
		listener, err = tnetNode.Listen("tcp", *listenAddr)
		if err != nil {
			return fmt.Errorf("tailnet listen: %w", err)
		}
		mcpHost = t.Tailnet.Hostname
	} else {
		listener, err = net.Listen("tcp", "127.0.0.1"+normalizePort(*listenAddr))
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		mcpHost = "127.0.0.1"
	}

	bs := bus.NewMemBus()
	defer bs.Close()

	// Tailnet HTTP client + per-session worker token for cloud-backed
	// agents. The token is generated if not pre-set in env; cloud
	// provisioners inject it into the worker container so HTTP calls from
	// the leader can be authenticated. Without a tailnet, cloud agents are
	// not reachable, so we leave HTTPClient nil and the spawner returns a
	// clear error if a fargate agent is requested.
	var httpClient *http.Client
	if tnetNode != nil {
		httpClient = tnetNode.HTTPClient()
	}
	workerToken := os.Getenv("TEEM_WORKER_TOKEN")
	if workerToken == "" {
		workerToken = randomToken()
	}

	// Resolve the leader's git repo root (best-effort) and derive a
	// per-team base directory for local agent worktrees. Neither is fatal
	// if missing — the local provisioner only needs them when an agent
	// without an explicit working_dir is spawned.
	repoRoot, _ := provisioner.ResolveRepoRoot("")
	worktreeBase := defaultWorktreeBase(t.Name)

	// Leader URL workers reach for audit posts. Empty when no tailnet
	// (a worker on a remote container can't reach 127.0.0.1).
	var leaderURL string
	if tnetNode != nil {
		leaderURL = fmt.Sprintf("http://%s%s", t.Tailnet.Hostname, normalizePort(*listenAddr))
	}

	gitCfg := readGitConfig()
	if gitCfg.RepoURL != "" {
		fmt.Fprintf(os.Stderr, "[teem] cloud workers will clone %s on branch %s<agent-id>\n", gitCfg.RepoURL, branchPrefixOrDefault(gitCfg.BranchPrefix))
	}

	stateStore := state.NewStore(defaultStateDir(t.Name))

	reg := mcpsrv.NewRegistry()
	spawner := agent.NewSpawner(t, bs, reg, agent.Config{
		HTTPClient:       httpClient,
		WorkerToken:      workerToken,
		CloudProvisioner: cloudProvisionerFactory(workerToken, leaderURL, gitCfg, stateStore),
		RepoRoot:         repoRoot,
		WorktreeBase:     worktreeBase,
		LeaderURL:        leaderURL,
		StateStore:       stateStore,
	})
	defer spawner.Stop()

	// Reconnect to any persistent agents the operator has running. This
	// makes them visible in `list_agents` without an explicit spawn.
	if n := spawner.Reconcile(ctx); n > 0 {
		fmt.Fprintf(os.Stderr, "[teem] reconciled %d persistent agent(s)\n", n)
	}

	// Audit sink — workers POST structured events here over the tailnet.
	// Survives leader restarts; greppable JSONL on disk.
	auditPath := defaultAuditPath(t.Name)
	auditSink, err := audit.OpenFile(auditPath)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer auditSink.Close()
	fmt.Fprintf(os.Stderr, "[teem] audit log: %s\n", auditPath)

	srv, err := mcpsrv.New(mcpsrv.Config{
		Bus:      bs,
		Team:     t,
		Registry: reg,
		Spawner:  spawner,
		Audit:    auditSink,
	})
	if err != nil {
		return err
	}

	// Composite HTTP routing: /mcp/* → MCP server (no auth — local
	// consumer is the leader claude); /audit → audit endpoints (bearer
	// auth via worker token, since workers post across the tailnet).
	rootHandler := newRootHandler(srv.Handler(), audit.Handler(auditSink, workerToken))

	httpSrv := &http.Server{
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		} else {
			serverErr <- nil
		}
	}()
	defer func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx2)
	}()
	_ = srv // hold the reference so its registry/spawner deps don't get GC'd

	endpoint := fmt.Sprintf("http://%s%s/mcp", mcpHost, normalizePort(*listenAddr))
	fmt.Fprintf(os.Stderr, "[teem] orchestrator MCP endpoint: %s\n", endpoint)

	var tr transport.Transport = transport.LocalTransport{}
	leaderLoc := "local"
	if *leaderHost != "" {
		tr = transport.SSHTransport{Target: *leaderHost}
		leaderLoc = *leaderHost
	}

	ld, err := leader.Start(ctx, leader.Config{
		Transport:    tr,
		MCPEndpoint:  endpoint,
		SystemPrompt: t.LeaderSystemPrompt(),
	})
	if err != nil {
		return fmt.Errorf("leader start: %w", err)
	}
	defer ld.Close()
	fmt.Fprintf(os.Stderr, "[teem] leader running on: %s\n", leaderLoc)
	fmt.Fprintln(os.Stderr, "[teem] ready — type your message and press Enter. Ctrl-D to quit.")

	return runREPL(ctx, ld, serverErr)
}

func runREPL(ctx context.Context, ld leader.Leader, serverErr <-chan error) error {
	in := bufio.NewScanner(os.Stdin)
	prompt := func() { fmt.Print("> ") }
	prompt()
	for in.Scan() {
		text := strings.TrimSpace(in.Text())
		if text == "" {
			prompt()
			continue
		}
		if err := ld.Send(ctx, text); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		if err := drainTurn(ctx, ld, serverErr); err != nil {
			return err
		}
		prompt()
	}
	return in.Err()
}

func drainTurn(ctx context.Context, ld leader.Leader, serverErr <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverErr:
			if err != nil {
				return fmt.Errorf("mcp server: %w", err)
			}
			return errors.New("mcp server stopped unexpectedly")
		case ev, ok := <-ld.Events():
			if !ok {
				return errors.New("leader exited")
			}
			switch ev.Kind {
			case leader.EventAssistantText:
				fmt.Println(ev.Text)
			case leader.EventToolUse:
				fmt.Fprintf(os.Stderr, "[tool] %s\n", ev.ToolName)
			case leader.EventResult:
				if ev.Text != "" {
					fmt.Println(ev.Text)
				}
				return nil
			case leader.EventError:
				fmt.Fprintf(os.Stderr, "[error] %s\n", ev.Text)
				return nil
			}
		}
	}
}

// teamSearchPaths lists the file names `teem chat` searches for a team
// when --team is unset, in priority order. The wizard writes to the first
// entry.
var teamSearchPaths = []string{"teem.yaml", "config/team.example.yaml"}

// resolveTeamPath returns the team YAML to load. An explicit path is used
// as-is; otherwise the search chain is walked. Returns a helpful error
// pointing at `teem init` if nothing is found.
func resolveTeamPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	for _, p := range teamSearchPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no team YAML found (looked for %s). Run `teem init` to create one.", strings.Join(teamSearchPaths, ", "))
}

// newRootHandler composes the leader's HTTP routes. /mcp/* goes to the
// MCP server (no auth — its consumer is the leader's own Claude
// subprocess); /audit goes to the audit endpoints (bearer auth, since
// workers reach it across the tailnet).
func newRootHandler(mcp http.Handler, audit http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/mcp"):
			mcp.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/audit"):
			audit.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// defaultStateDir returns the directory holding persistent-agent state
// files for the team. Lives alongside the audit log and worktrees so
// everything Teem persists is under ~/.teem.
func defaultStateDir(teamName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teem", "state")
	}
	return filepath.Join(home, ".teem", "state", slug(teamName))
}

// defaultAuditPath returns the on-disk audit log path for a team.
// Lives alongside the other ~/.teem state so it's predictable across
// sessions. Team name is slugged so YAML can't escape the path.
func defaultAuditPath(teamName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teem", "audit", "audit.jsonl")
	}
	return filepath.Join(home, ".teem", "audit", slug(teamName), "audit.jsonl")
}

// slug normalises an arbitrary name to a path-safe slug. Shared by
// defaultAuditPath and defaultWorktreeBase.
func slug(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == ' ' || r == '_' || r == '-':
			return '-'
		}
		return -1
	}, s)
	out = strings.Trim(out, "-")
	if out == "" {
		return "default"
	}
	return out
}

// defaultWorktreeBase returns the directory under which local agent
// worktrees are placed for this team. Lives under ~/.teem alongside the
// tsnet state so it's predictable across sessions. The team name is
// slugged (lowercase, ascii-and-dash) so an arbitrary team.Name in the
// YAML can't escape into the path.
func defaultWorktreeBase(teamName string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".teem", "worktrees", slug(teamName))
}

// randomToken returns a 24-byte hex token used as the leader↔worker bearer
// when TEEM_WORKER_TOKEN isn't already set.
func randomToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failures only happen on very broken systems; falling
		// back to a fixed string would mask the failure, so panic.
		panic("teem: read random: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// cloudProvisionerFactory returns a factory the spawner uses to build
// provisioners for cloud backends. Today only Fargate; constructed lazily
// because it reads from env (AWS_REGION, TEEM_ECS_* etc) at build time.
func cloudProvisionerFactory(workerToken, leaderURL string, git provisioner.GitConfig, stateStore *state.Store) agent.CloudProvisionerFactory {
	return func(backend provisioner.Backend) (provisioner.Provisioner, error) {
		switch backend {
		case provisioner.BackendFargate:
			return provisioner.NewFargateProvisioner(workerToken, leaderURL, git, stateStore)
		default:
			return nil, fmt.Errorf("no cloud provisioner for backend %q", backend)
		}
	}
}

// branchPrefixOrDefault returns the operator-configured branch prefix
// or "teem/" — used only for the startup banner so the message matches
// what the worker daemon will actually do.
func branchPrefixOrDefault(s string) string {
	if s == "" {
		return "teem/"
	}
	return s
}

// readGitConfig pulls TEEM_GIT_* env into a GitConfig the provisioner
// can hand off to remote workers. Returns the same struct whether or not
// any vars are set; an empty RepoURL is the worker-side signal to skip
// cloning.
func readGitConfig() provisioner.GitConfig {
	return provisioner.GitConfig{
		RepoURL:      os.Getenv("TEEM_GIT_REPO_URL"),
		Token:        os.Getenv("TEEM_GIT_TOKEN"),
		Username:     os.Getenv("TEEM_GIT_USERNAME"),
		AuthorName:   os.Getenv("TEEM_GIT_AUTHOR_NAME"),
		AuthorEmail:  os.Getenv("TEEM_GIT_AUTHOR_EMAIL"),
		BranchPrefix: os.Getenv("TEEM_GIT_BRANCH_PREFIX"),
		AutoPush:     os.Getenv("TEEM_GIT_AUTO_PUSH"),
	}
}

func resolveAuthKey(envName string) string {
	if envName == "" {
		envName = "TS_AUTHKEY"
	}
	return os.Getenv(envName)
}

func normalizePort(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return ":" + port
	}
	return addr
}

// runLLM is the `teem llm ...` subcommand group.
func runLLM(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: teem llm ping --prompt \"...\"")
	}
	switch args[0] {
	case "ping":
		fs := flag.NewFlagSet("llm ping", flag.ExitOnError)
		prompt := fs.String("prompt", "say hi", "prompt to send")
		model := fs.String("model", "", "model id (default: claude-sonnet-4-6)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		c, err := llm.NewAnthropic(*model)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		resp, err := c.Complete(ctx, llm.CompletionRequest{
			Messages: []llm.Message{{Role: llm.RoleUser, Content: *prompt}},
		})
		if err != nil {
			return err
		}
		fmt.Printf("[%s] %s\n", resp.Model, resp.Content)
		return nil
	default:
		return fmt.Errorf("unknown llm subcommand: %s", args[0])
	}
}
