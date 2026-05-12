package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/frasergraham/teem/internal/agent"
	"github.com/frasergraham/teem/internal/bus"
	"github.com/frasergraham/teem/internal/leader"
	"github.com/frasergraham/teem/internal/llm"
	mcpsrv "github.com/frasergraham/teem/internal/mcp"
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
  teem chat    --team config/team.example.yaml [--tailnet=true] [--leader-host user@host] [--listen :7777]
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
	teamPath := fs.String("team", "config/team.example.yaml", "team YAML")
	useTailnet := fs.Bool("tailnet", true, "join the tailnet via tsnet")
	leaderHost := fs.String("leader-host", "", "if set (user@host), run the Leader on that host via SSH; otherwise local")
	listenAddr := fs.String("listen", ":7777", "address the orchestrator MCP server listens on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	t, err := team.Load(*teamPath)
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

	reg := mcpsrv.NewRegistry()
	spawner := agent.NewSpawner(t, bs, reg)
	defer spawner.Stop()

	srv, err := mcpsrv.New(mcpsrv.Config{
		Bus:      bs,
		Team:     t,
		Registry: reg,
		Spawner:  spawner,
	})
	if err != nil {
		return err
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Serve(listener)
	}()
	defer func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx2)
	}()

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
