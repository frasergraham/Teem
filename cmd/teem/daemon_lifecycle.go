package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// daemonFlags is the shared flag set for the daemon-mode commands.
//
// foreground keeps the daemon attached to the terminal (default is
// detached). detached is internal: the re-exec'd child sets it so the
// foreground branch runs without forking again.
type daemonFlags struct {
	useTailnet bool
	listenAddr string
	foreground bool
	detached   bool
}

func parseStartFlags(args []string) (*daemonFlags, error) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	df := &daemonFlags{}
	fs.BoolVar(&df.useTailnet, "tailnet", true, "join the tailnet via tsnet")
	fs.StringVar(&df.listenAddr, "listen", ":7777", "address the orchestrator listens on")
	fs.BoolVar(&df.foreground, "foreground", false, "stay attached to the terminal instead of detaching")
	fs.BoolVar(&df.detached, "detached", false, "internal: marks the re-exec'd child (do not pass)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return df, nil
}

// --- daemon entrypoints ---------------------------------------------------

// runStart launches the orchestrator daemon. Headless by default; pass
// --foreground to stay attached. Only one daemon per user at a time.
func runStart(args []string) error {
	df, err := parseStartFlags(args)
	if err != nil {
		return err
	}

	if pid, ok := readDaemonPID(); ok {
		return fmt.Errorf("daemon already running (pid %d). Run `teem stop` first.", pid)
	}

	if !df.foreground && !df.detached {
		return forkDetached(df)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveDaemon(ctx, df)
}

func runStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pid, alive := readDaemonPID()
	if !alive {
		fmt.Fprintln(os.Stderr, "no running daemon")
		clearDaemonState()
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := readDaemonPID(); !alive {
			fmt.Printf("stopped daemon (pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("sent SIGTERM to pid %d (cleanup may still be in progress)\n", pid)
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pid, alive := readDaemonPID()
	if !alive {
		if pid != 0 {
			fmt.Printf("stopped (stale pid file: %d)\n", pid)
		} else {
			fmt.Println("stopped")
		}
		return nil
	}
	s, ok, _ := readDaemonStateFile()
	fmt.Printf("running  pid=%d\n", pid)
	if ok {
		fmt.Printf("  endpoint: %s\n", s.Endpoint)
		fmt.Printf("  started:  %s\n", s.StartedAt.Local().Format(time.RFC3339))
		if len(s.Teams) == 0 {
			fmt.Println("  teams:    (none registered yet)")
		} else {
			fmt.Printf("  teams:    %d\n", len(s.Teams))
			for _, name := range s.Teams {
				fmt.Printf("    - %s\n", name)
			}
		}
	}
	return nil
}

// --- daemon process: top-level state file ---------------------------------

// daemonStateFile is the on-disk endpoint discovery file at
// ~/.teem/daemon.json. teem chat / teem status read it.
type daemonStateFile struct {
	PID      int    `json:"pid"`
	Endpoint string `json:"endpoint"`     // http://<host>:<port>
	Token    string `json:"worker_token"` // shared bearer for /audit and /control
	// Teams holds display names for `teem status` output. The daemon
	// keys teams internally by team_id; this field is for humans only.
	Teams     []string  `json:"teams,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

func daemonHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".teem"
	}
	return filepath.Join(home, ".teem")
}

func daemonPIDPath() string  { return filepath.Join(daemonHomeDir(), "daemon.pid") }
func daemonJSONPath() string { return filepath.Join(daemonHomeDir(), "daemon.json") }
func daemonLogPath() string  { return filepath.Join(daemonHomeDir(), "daemon.log") }

func readDaemonPID() (int, bool) {
	body, err := os.ReadFile(daemonPIDPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return pid, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

func readDaemonStateFile() (daemonStateFile, bool, error) {
	body, err := os.ReadFile(daemonJSONPath())
	if err != nil {
		if os.IsNotExist(err) {
			return daemonStateFile{}, false, nil
		}
		return daemonStateFile{}, false, err
	}
	var s daemonStateFile
	if err := json.Unmarshal(body, &s); err != nil {
		return daemonStateFile{}, false, err
	}
	return s, true, nil
}

func writeDaemonStateFile(s daemonStateFile) error {
	if err := os.MkdirAll(daemonHomeDir(), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(daemonJSONPath(), body); err != nil {
		return err
	}
	return atomicWrite(daemonPIDPath(), []byte(strconv.Itoa(s.PID)+"\n"))
}

func clearDaemonState() {
	_ = os.Remove(daemonPIDPath())
	_ = os.Remove(daemonJSONPath())
	// The persistent worker_token file is intentionally NOT removed
	// here so the token survives `teem stop`/`teem start`.
}

// workerTokenPath returns the persistent token file location.
func workerTokenPath() string { return filepath.Join(daemonHomeDir(), "worker_token") }

// loadOrCreateWorkerToken reads the persistent worker token file or
// generates+writes a fresh one. The token is shared between the leader
// (this process) and every worker it spawns; stable across daemon
// restarts so workers don't all get 401'd after a bounce. To rotate,
// stop the daemon, remove ~/.teem/worker_token, then start again.
func loadOrCreateWorkerToken() string {
	if body, err := os.ReadFile(workerTokenPath()); err == nil {
		t := strings.TrimSpace(string(body))
		if t != "" {
			return t
		}
	}
	t := randomToken()
	if err := os.MkdirAll(daemonHomeDir(), 0o700); err == nil {
		_ = os.WriteFile(workerTokenPath(), []byte(t+"\n"), 0o600)
	}
	return t
}

func atomicWrite(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// persistStateSnapshot refreshes daemon.json with the current set of
// registered teams. Called after every registration/unregistration so
// `teem status` sees up-to-date info.
func (d *daemon) persistStateSnapshot() {
	d.mu.Lock()
	names := make([]string, 0, len(d.teams))
	for _, rt := range d.teams {
		names = append(names, rt.team.Name)
	}
	d.mu.Unlock()
	_ = writeDaemonStateFile(daemonStateFile{
		PID:       os.Getpid(),
		Endpoint:  d.endpoint,
		Token:     d.token,
		Teams:     names,
		StartedAt: time.Now().UTC(),
	})
}

// --- detached fork ---------------------------------------------------------

// forkDetached re-execs the current binary with `start --detached`,
// redirecting stdio to ~/.teem/daemon.log and starting a new session
// so the child outlives the parent.
func forkDetached(df *daemonFlags) error {
	logPath := daemonLogPath()
	if err := os.MkdirAll(daemonHomeDir(), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	childArgs := []string{"start", "--detached", "--listen", df.listenAddr}
	if !df.useTailnet {
		childArgs = append(childArgs, "--tailnet=false")
	}
	cmd := exec.Command(self, childArgs...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn detached daemon: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	fmt.Fprintf(os.Stderr, "[teem] daemon spawned (pid %d, log: %s)\n", pid, logPath)
	return nil
}
