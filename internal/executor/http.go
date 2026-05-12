package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPExecutor talks to a remote teem-worker daemon over HTTP. It is used
// for cloud-backed agents (today: Fargate) where the leader does not own
// the claude subprocess directly.
//
// The contract with teem-worker:
//
//	POST {base}/jobs              → 202 on accept
//	GET  {base}/jobs/{id}?wait=Ns  → {status, output, error}, long-poll
//	GET  {base}/healthz            → liveness
//
// Every request carries Authorization: Bearer <token>.
type HTTPExecutor struct {
	Client  *http.Client
	BaseURL string
	Token   string

	// PollWait controls the long-poll wait sent to GET /jobs/{id}?wait=...
	// Defaults to 30s if zero.
	PollWait time.Duration
	// HealthCheckEvery controls the cadence of /healthz pings the watchdog
	// performs concurrently with long-polls. Defaults to 15s if zero.
	HealthCheckEvery time.Duration
	// MaxHealthFailures is the consecutive /healthz failures tolerated
	// before Execute returns "worker unreachable". Defaults to 3.
	MaxHealthFailures int
}

// NewHTTP constructs an HTTPExecutor with sensible defaults.
func NewHTTP(client *http.Client, baseURL, token string) *HTTPExecutor {
	return &HTTPExecutor{
		Client:            client,
		BaseURL:           strings.TrimRight(baseURL, "/"),
		Token:             token,
		PollWait:          30 * time.Second,
		HealthCheckEvery:  15 * time.Second,
		MaxHealthFailures: 3,
	}
}

// ErrWorkerUnreachable signals that /healthz failed enough times in a row
// that the executor gave up on the job. Outstanding jobs surfaced this
// way map to resultMessage{Error: "worker unreachable: ..."}.
var ErrWorkerUnreachable = errors.New("worker unreachable")

type httpJobResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Output string `json:"output"`
	Error  string `json:"error"`
}

// Execute posts the job to the remote worker, then long-polls until the
// worker reports done/error or the watchdog declares it unreachable.
func (e *HTTPExecutor) Execute(ctx context.Context, job Job) (string, error) {
	if e.Client == nil {
		return "", fmt.Errorf("http executor: no http client configured")
	}
	if err := e.postJob(ctx, job); err != nil {
		return "", err
	}

	pollWait := e.PollWait
	if pollWait == 0 {
		pollWait = 30 * time.Second
	}
	healthEvery := e.HealthCheckEvery
	if healthEvery == 0 {
		healthEvery = 15 * time.Second
	}
	maxFail := e.MaxHealthFailures
	if maxFail == 0 {
		maxFail = 3
	}

	// Watchdog: concurrent goroutine pinging /healthz. If it trips, we
	// cancel pollCtx so the in-flight long-poll returns and Execute
	// surfaces ErrWorkerUnreachable.
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	healthErrCh := make(chan error, 1)
	go e.runWatchdog(pollCtx, healthEvery, maxFail, healthErrCh, cancelPoll)

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-healthErrCh:
			return "", err
		default:
		}
		resp, err := e.pollJob(pollCtx, job.ID, pollWait)
		if err != nil {
			// pollCtx was cancelled by the watchdog → surface its error.
			select {
			case herr := <-healthErrCh:
				return "", herr
			default:
			}
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return "", ctx.Err()
			}
			// Transient network blip during long-poll. Brief backoff, retry.
			select {
			case <-pollCtx.Done():
				return "", pollCtx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		switch resp.Status {
		case "done":
			return resp.Output, nil
		case "error":
			if resp.Error != "" {
				return resp.Output, errors.New(resp.Error)
			}
			return resp.Output, fmt.Errorf("worker reported error with no message")
		case "pending", "running":
			// Loop: the long-poll already waited up to pollWait seconds.
		default:
			return resp.Output, fmt.Errorf("unexpected job status %q", resp.Status)
		}
	}
}

func (e *HTTPExecutor) postJob(ctx context.Context, job Job) error {
	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/jobs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.Client.Do(req)
	if err != nil {
		return fmt.Errorf("post job: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post job: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (e *HTTPExecutor) pollJob(ctx context.Context, jobID string, wait time.Duration) (*httpJobResponse, error) {
	url := fmt.Sprintf("%s/jobs/%s?wait=%s", e.BaseURL, jobID, wait)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get job: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var jr httpJobResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		return nil, fmt.Errorf("decode job: %w", err)
	}
	return &jr, nil
}

// CheckHealth makes a single GET /healthz call. Used by the watchdog and
// also exposed for spawner-side liveness checks during agent provisioning.
func (e *HTTPExecutor) CheckHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

func (e *HTTPExecutor) runWatchdog(ctx context.Context, every time.Duration, maxFail int, errCh chan<- error, cancel context.CancelFunc) {
	t := time.NewTicker(every)
	defer t.Stop()
	consecutive := 0
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		hctx, hcancel := context.WithTimeout(ctx, 5*time.Second)
		err := e.CheckHealth(hctx)
		hcancel()
		if err != nil {
			consecutive++
			lastErr = err
			if consecutive >= maxFail {
				select {
				case errCh <- fmt.Errorf("%w: %v", ErrWorkerUnreachable, lastErr):
				default:
				}
				cancel()
				return
			}
			continue
		}
		consecutive = 0
	}
}
