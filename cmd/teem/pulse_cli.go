package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/frasergraham/teem/internal/team"
)

// runPulse implements `teem pulse <sub>`.
//
//	teem pulse start  [--team x] [--interval 5m]   turn on autonomy
//	teem pulse stop   [--team x]                   turn off
//	teem pulse pause  [--team x] [--reason "..."]  skip ticks while paused
//	teem pulse resume [--team x]                   undo pause
//	teem pulse tick   [--team x]                   force one tick now
//	teem pulse status [--team x]                   print current state
func runPulse(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: teem pulse <start|stop|pause|resume|tick|status> [flags]")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("pulse "+sub, flag.ExitOnError)
	teamPath := fs.String("team", "", "team YAML (default: ./teem.yaml or ./config/team.example.yaml)")
	interval := fs.String("interval", "", "tick interval (Go duration); start only")
	reason := fs.String("reason", "", "pause reason (annotation only)")
	if err := fs.Parse(rest); err != nil {
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

	ds, ok, err := readDaemonStateFile()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("daemon not running — `teem start` first")
	}
	base := ds.Endpoint + "/control/teams/" + t.Name + "/pulse"

	switch sub {
	case "status":
		return printPulseStatus(base, ds.Token)
	case "start":
		body := pulseCommand{Action: "start", Interval: *interval}
		return postPulseCommand(base, ds.Token, body)
	case "stop":
		return postPulseCommand(base, ds.Token, pulseCommand{Action: "stop"})
	case "pause":
		return postPulseCommand(base, ds.Token, pulseCommand{Action: "pause", Reason: *reason})
	case "resume":
		return postPulseCommand(base, ds.Token, pulseCommand{Action: "resume"})
	case "tick":
		return postPulseCommand(base, ds.Token, pulseCommand{Action: "tick"})
	default:
		return fmt.Errorf("unknown pulse subcommand %q", sub)
	}
}

func postPulseCommand(url, token string, cmd pulseCommand) error {
	body, _ := json.Marshal(cmd)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var status pulseStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return err
	}
	printStatusHuman(status, cmd.Action)
	return nil
}

func printPulseStatus(url, token string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var status pulseStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return err
	}
	printStatusHuman(status, "")
	return nil
}

func printStatusHuman(s pulseStatus, action string) {
	if action != "" {
		fmt.Fprintf(os.Stderr, "[pulse] %s ok\n", action)
	}
	state := "stopped"
	if s.Running {
		state = "running"
	}
	if s.Paused {
		state = "paused"
	}
	fmt.Printf("pulse: %s\n", state)
	fmt.Printf("  interval:    %s\n", s.Interval)
	fmt.Printf("  ticks total: %d\n", s.TickCount)
	if s.LastTick.IsZero() {
		fmt.Println("  last tick:   (never)")
	} else {
		fmt.Printf("  last tick:   %s (%s ago)\n", s.LastTick.Local().Format(time.RFC3339), time.Since(s.LastTick).Truncate(time.Second))
	}
}
