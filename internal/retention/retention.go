// Package retention owns the daemon's age-based history GC policy.
//
// **Default: never delete.** The daemon preserves all history (stopped
// registry entries, transcripts on disk) indefinitely so audit, debugging,
// and cross-task investigations stay tractable. Retention is opt-in: an
// operator who explicitly configures a TTL accepts that older data will be
// removed on the configured cadence.
//
// Two knobs, both env-tuned at daemon startup:
//
//   - TEEM_STOPPED_AGENT_TTL — duration; entries in StateStopped older
//     than this are removed from the in-memory registry. The audit log
//     and transcripts are independent; clearing a registry entry does
//     NOT delete its on-disk history.
//   - TEEM_TRANSCRIPT_TTL — duration; per-job transcript files
//     (`<state>/transcripts/<agent>/<job>.jsonl`) older than this are
//     deleted from disk.
//
// Values understood: a Go duration ("168h"), "0", "off", "never", or
// empty → disabled. Invalid values fall back to "never" with a stderr
// warning.
//
// Cadence is a single shared ticker (default 1h) so the daemon doesn't
// spin up two timers for two TTL knobs. The first pass runs ~30s after
// startup so a dev iteration can observe whether the configured TTL is
// sane without waiting an hour.
package retention

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config captures the operator-tuned policy. Zero-valued Config means
// retention is fully disabled (the default).
type Config struct {
	// StoppedAgentTTL: 0 disables. When > 0, stopped registry entries
	// older than this are removed.
	StoppedAgentTTL time.Duration

	// TranscriptTTL: 0 disables. When > 0, transcript files older
	// than this are removed from disk.
	TranscriptTTL time.Duration

	// SweepInterval is how often the GC ticker fires. Defaults to 1h
	// via DefaultSweepInterval when zero. Smaller values are useful
	// in tests; production should leave it alone.
	SweepInterval time.Duration
}

// DefaultSweepInterval is the ticker cadence when SweepInterval is unset.
const DefaultSweepInterval = time.Hour

// Enabled reports whether any retention rule is active. If false, the
// daemon shouldn't spin up the GC goroutine at all.
func (c Config) Enabled() bool {
	return c.StoppedAgentTTL > 0 || c.TranscriptTTL > 0
}

// LoadConfig reads retention TTLs from the environment, with the
// "default: never" semantics: anything ambiguous, missing, or "off" is
// treated as disabled. Invalid Go duration strings log to stderr and
// resolve to disabled rather than failing daemon startup.
func LoadConfig() Config {
	return Config{
		StoppedAgentTTL: parseTTL("TEEM_STOPPED_AGENT_TTL"),
		TranscriptTTL:   parseTTL("TEEM_TRANSCRIPT_TTL"),
		SweepInterval:   parseInterval("TEEM_RETENTION_SWEEP_INTERVAL"),
	}
}

func parseTTL(envKey string) time.Duration {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(envKey)))
	switch v {
	case "", "0", "off", "never", "disabled":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "[retention] bad %s %q — retention disabled for this knob\n", envKey, v)
		return 0
	}
	return d
}

func parseInterval(envKey string) time.Duration {
	v := strings.TrimSpace(os.Getenv(envKey))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "[retention] bad %s %q — using default sweep interval\n", envKey, v)
		return 0
	}
	return d
}

// SweepTranscripts walks transcriptsDir and removes any per-job jsonl
// file whose modification time is before (now - ttl). Sub-directories
// of agents that end up empty after the sweep are kept — they're cheap
// and operators may find them useful to know an agent existed at all.
//
// ttl <= 0 is a no-op. Returns the count of files removed and the first
// error encountered (errors don't stop the walk; later files are still
// considered).
func SweepTranscripts(transcriptsDir string, now time.Time, ttl time.Duration) (int, error) {
	if ttl <= 0 || transcriptsDir == "" {
		return 0, nil
	}
	cutoff := now.Add(-ttl)
	removed := 0
	var firstErr error
	err := filepath.WalkDir(transcriptsDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if firstErr == nil {
				firstErr = walkErr
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if rmErr := os.Remove(path); rmErr != nil {
				if firstErr == nil {
					firstErr = rmErr
				}
				return nil
			}
			removed++
		}
		return nil
	})
	if err != nil && firstErr == nil {
		firstErr = err
	}
	return removed, firstErr
}
