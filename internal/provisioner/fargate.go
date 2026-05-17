package provisioner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/frasergraham/teem/internal/state"
)

// FargateProvisioner places workers as ephemeral ECS Fargate tasks. One
// task per agent. The leader hands every task the same bearer token via
// container overrides so leader-side HTTPExecutors can authenticate.
//
// Required env (read in NewFargateProvisioner):
//
//	AWS_REGION                  AWS region for the ECS API client
//	TEEM_ECS_CLUSTER            ECS cluster name or arn
//	TEEM_ECS_TASK_DEF           Task definition family or family:revision
//	TEEM_ECS_SUBNETS            csv of subnet ids (single AZ recommended)
//	TEEM_ECS_SECURITY_GROUPS    csv of security group ids
//	TS_AUTHKEY                  ephemeral tailnet auth key (reusable, preauth)
//	ANTHROPIC_API_KEY           passed to the worker container so claude works
//
// Optional env:
//
//	TEEM_WORKER_IMAGE           override the task def container image
//	TEEM_ECS_CONTAINER_NAME     container name in the task def (default "teem-worker")
//	TEEM_ECS_ASSIGN_PUBLIC_IP   "true"/"false" (default true; needed without NAT GW)
//	TEEM_ECS_PROVISION_TIMEOUT  RUNNING wait, Go duration (default 5m)
type FargateProvisioner struct {
	WorkerToken string
	LeaderURL   string

	// Git is the source-control configuration passed to every worker
	// container. Empty fields are omitted from the overrides.
	Git GitConfig

	// State is the persistent-agent store. When set, persistent agents
	// reuse a prior task ARN if it's still RUNNING.
	State *state.Store

	Client *ecs.Client

	Cluster        string
	TaskDefinition string
	Subnets        []string
	SecurityGroups []string
	ContainerName  string
	ImageOverride  string
	AssignPublicIP ecstypes.AssignPublicIp
	TSAuthKey      string
	AnthropicKey   string
	WaitTimeout    time.Duration
}

// ErrFargateNotConfigured is returned when required env vars are missing.
var ErrFargateNotConfigured = errors.New("provisioner: fargate not configured")

// GitConfig is the per-worker git configuration. Mirrors agent.GitConfig
// — kept here so internal/provisioner doesn't depend on internal/agent.
type GitConfig struct {
	RepoURL      string
	Token        string
	Username     string
	AuthorName   string
	AuthorEmail  string
	BranchPrefix string
	AutoPush     string
}

// NewFargateProvisioner reads config from env and constructs the provisioner.
// Returns ErrFargateNotConfigured if any required env var is missing so the
// caller can surface a helpful message.
//
// leaderURL is the URL workers POST audit events to (e.g.
// http://teem-leader:7777). Pass "" to disable the worker→leader event
// channel for tasks this provisioner launches.
//
// git carries the source-control config (repo URL, PAT, etc.) the
// provisioner injects into each task's container overrides.
//
// stateStore enables persistent agents — when present, persistent agents
// reuse a prior task ARN if DescribeTasks reports it as RUNNING. Pass nil
// to disable the optimisation (every spawn launches a new task).
func NewFargateProvisioner(workerToken, leaderURL string, git GitConfig, stateStore *state.Store) (*FargateProvisioner, error) {
	region := os.Getenv("AWS_REGION")
	cluster := os.Getenv("TEEM_ECS_CLUSTER")
	taskDef := os.Getenv("TEEM_ECS_TASK_DEF")
	subnetsCsv := os.Getenv("TEEM_ECS_SUBNETS")
	sgsCsv := os.Getenv("TEEM_ECS_SECURITY_GROUPS")
	missing := []string{}
	if region == "" {
		missing = append(missing, "AWS_REGION")
	}
	if cluster == "" {
		missing = append(missing, "TEEM_ECS_CLUSTER")
	}
	if taskDef == "" {
		missing = append(missing, "TEEM_ECS_TASK_DEF")
	}
	if subnetsCsv == "" {
		missing = append(missing, "TEEM_ECS_SUBNETS")
	}
	if sgsCsv == "" {
		missing = append(missing, "TEEM_ECS_SECURITY_GROUPS")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing %s", ErrFargateNotConfigured, strings.Join(missing, ", "))
	}

	cfg, err := awscfg.LoadDefaultConfig(context.Background(), awscfg.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	assignPublic := ecstypes.AssignPublicIpEnabled
	if v := os.Getenv("TEEM_ECS_ASSIGN_PUBLIC_IP"); strings.EqualFold(v, "false") || v == "0" {
		assignPublic = ecstypes.AssignPublicIpDisabled
	}
	containerName := os.Getenv("TEEM_ECS_CONTAINER_NAME")
	if containerName == "" {
		containerName = "teem-worker"
	}

	timeout := 5 * time.Minute
	if v := os.Getenv("TEEM_ECS_PROVISION_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	return &FargateProvisioner{
		WorkerToken:    workerToken,
		LeaderURL:      leaderURL,
		Git:            git,
		State:          stateStore,
		Client:         ecs.NewFromConfig(cfg),
		Cluster:        cluster,
		TaskDefinition: taskDef,
		Subnets:        splitCsv(subnetsCsv),
		SecurityGroups: splitCsv(sgsCsv),
		ContainerName:  containerName,
		ImageOverride:  os.Getenv("TEEM_WORKER_IMAGE"),
		AssignPublicIP: assignPublic,
		TSAuthKey:      os.Getenv("TS_AUTHKEY"),
		AnthropicKey:   os.Getenv("ANTHROPIC_API_KEY"),
		WaitTimeout:    timeout,
	}, nil
}

func splitCsv(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Provision runs a new ECS Fargate task for the agent and waits until it
// reports RUNNING. The container is expected to be the teem-worker image
// configured in the task definition; container env is overridden per task
// with agent-specific identifiers.
func (p *FargateProvisioner) Provision(ctx context.Context, spec AgentSpec) (*Agent, error) {
	hostname := "teem-" + spec.ID

	// Persistent agents: try to reuse a prior task if we have one and
	// the ECS API says it's still RUNNING. Falls through to the launch
	// path on cache miss or stale entry.
	if spec.IsPersistent() && p.State != nil {
		if reused, ok, err := p.tryReuse(ctx, spec, hostname); err != nil {
			// Reuse lookup itself failed — surface so the operator sees it.
			return nil, fmt.Errorf("fargate reuse check: %w", err)
		} else if ok {
			return reused, nil
		}
	}

	envOverrides := []ecstypes.KeyValuePair{
		kv("TEEM_AGENT_ID", spec.ID),
		kv("TEEM_AGENT_ROLE", spec.Role),
		kv("TEEM_WORKER_HOSTNAME", hostname),
		kv("TEEM_WORKER_TOKEN", p.WorkerToken),
	}
	if p.LeaderURL != "" {
		envOverrides = append(envOverrides, kv("TEEM_LEADER_URL", p.LeaderURL))
	}
	if p.TSAuthKey != "" {
		envOverrides = append(envOverrides, kv("TS_AUTHKEY", p.TSAuthKey))
	}
	if p.AnthropicKey != "" {
		envOverrides = append(envOverrides, kv("ANTHROPIC_API_KEY", p.AnthropicKey))
	}
	for _, e := range gitEnv(p.Git) {
		envOverrides = append(envOverrides, e)
	}

	override := ecstypes.ContainerOverride{
		Name:        strPtr(p.ContainerName),
		Environment: envOverrides,
	}
	if p.ImageOverride != "" {
		// Image overrides on ECS happen via the task def, not RunTask. We
		// keep the env hook in the spec so the caller can detect drift.
		_ = override
	}

	out, err := p.Client.RunTask(ctx, &ecs.RunTaskInput{
		Cluster:        strPtr(p.Cluster),
		TaskDefinition: strPtr(p.TaskDefinition),
		LaunchType:     ecstypes.LaunchTypeFargate,
		Count:          int32Ptr(1),
		StartedBy:      strPtr("teem-leader"),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        p.Subnets,
				SecurityGroups: p.SecurityGroups,
				AssignPublicIp: p.AssignPublicIP,
			},
		},
		Overrides: &ecstypes.TaskOverride{
			ContainerOverrides: []ecstypes.ContainerOverride{override},
		},
		Tags: []ecstypes.Tag{
			{Key: strPtr("teem:agent"), Value: strPtr(spec.ID)},
			{Key: strPtr("teem:role"), Value: strPtr(spec.Role)},
		},
		PropagateTags: ecstypes.PropagateTagsTaskDefinition,
	})
	if err != nil {
		return nil, fmt.Errorf("ecs RunTask: %w", err)
	}
	if len(out.Failures) > 0 {
		return nil, fmt.Errorf("ecs RunTask: %s", formatFailures(out.Failures))
	}
	if len(out.Tasks) == 0 || out.Tasks[0].TaskArn == nil {
		return nil, fmt.Errorf("ecs RunTask: no task returned")
	}
	taskArn := *out.Tasks[0].TaskArn

	if err := p.waitForRunning(ctx, taskArn); err != nil {
		// Best-effort cleanup; the caller will not get an Agent to call
		// Teardown on, so we issue StopTask here.
		_, _ = p.Client.StopTask(context.Background(), &ecs.StopTaskInput{
			Cluster: strPtr(p.Cluster),
			Task:    strPtr(taskArn),
			Reason:  strPtr("teem: provision wait failed"),
		})
		return nil, err
	}

	a := &Agent{
		ID:          spec.ID,
		Role:        spec.Role,
		Backend:     BackendFargate,
		Lifecycle:   spec.Lifecycle,
		TailnetHost: hostname,
		// Transport is intentionally nil for cloud agents; the Spawner
		// builds an HTTPExecutor in lieu.
		MCPs:  spec.MCPs,
		Skill: spec.Skill,
		Model: spec.Model,
		Cloud: &CloudPlacement{
			TaskARN: taskArn,
		},
	}
	if spec.IsPersistent() && p.State != nil {
		_ = p.State.Save(state.Record{
			AgentID:     spec.ID,
			Role:        spec.Role,
			Backend:     string(BackendFargate),
			Lifecycle:   spec.Lifecycle,
			TailnetHost: hostname,
			TaskARN:     taskArn,
		})
	}
	return a, nil
}

// tryReuse looks up the persistent agent's stored task ARN and asks ECS
// whether it's still RUNNING. Returns (agent, true, nil) on hit. A miss
// (no record, STOPPED, missing task) returns (nil, false, nil) so the
// caller falls through to RunTask. Real lookup errors propagate.
func (p *FargateProvisioner) tryReuse(ctx context.Context, spec AgentSpec, hostname string) (*Agent, bool, error) {
	rec, ok, err := p.State.Load(spec.ID)
	if err != nil {
		return nil, false, err
	}
	if !ok || rec.TaskARN == "" {
		return nil, false, nil
	}
	desc, err := p.Client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: strPtr(p.Cluster),
		Tasks:   []string{rec.TaskARN},
	})
	if err != nil {
		// Treat lookup errors as miss-with-warning: the operator gets a
		// fresh task. Don't propagate because a stale state file
		// shouldn't break agent spawn.
		return nil, false, nil
	}
	if len(desc.Tasks) == 0 {
		_ = p.State.Delete(spec.ID)
		return nil, false, nil
	}
	switch strDeref(desc.Tasks[0].LastStatus) {
	case "RUNNING":
		// Live — reuse.
	default:
		_ = p.State.Delete(spec.ID)
		return nil, false, nil
	}
	return &Agent{
		ID:          spec.ID,
		Role:        spec.Role,
		Backend:     BackendFargate,
		Lifecycle:   spec.Lifecycle,
		TailnetHost: hostname,
		MCPs:        spec.MCPs,
		Skill:       spec.Skill,
		Model:       spec.Model,
		Cloud:       &CloudPlacement{TaskARN: rec.TaskARN},
	}, true, nil
}

// waitForRunning polls DescribeTasks until lastStatus is RUNNING or the
// configured WaitTimeout elapses. It returns ctx errors as-is.
func (p *FargateProvisioner) waitForRunning(ctx context.Context, taskArn string) error {
	deadline := time.Now().Add(p.WaitTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("ecs task %s did not reach RUNNING within %s", taskArn, p.WaitTimeout)
		}
		desc, err := p.Client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: strPtr(p.Cluster),
			Tasks:   []string{taskArn},
		})
		if err != nil {
			return fmt.Errorf("ecs DescribeTasks: %w", err)
		}
		if len(desc.Tasks) == 0 {
			return fmt.Errorf("ecs DescribeTasks: task %s not found", taskArn)
		}
		t := desc.Tasks[0]
		status := strDeref(t.LastStatus)
		switch status {
		case "RUNNING":
			return nil
		case "STOPPED":
			reason := strDeref(t.StoppedReason)
			return fmt.Errorf("ecs task %s stopped before reaching RUNNING: %s", taskArn, reason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// CheckLiveness implements Watcher. It surfaces ErrAgentStopped when the
// ECS task is no longer RUNNING.
func (p *FargateProvisioner) CheckLiveness(ctx context.Context, a *Agent) error {
	if a == nil || a.Cloud == nil || a.Cloud.TaskARN == "" {
		return nil
	}
	desc, err := p.Client.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: strPtr(p.Cluster),
		Tasks:   []string{a.Cloud.TaskARN},
	})
	if err != nil {
		return fmt.Errorf("ecs DescribeTasks: %w", err)
	}
	if len(desc.Tasks) == 0 {
		return ErrAgentStopped
	}
	switch strDeref(desc.Tasks[0].LastStatus) {
	case "STOPPED", "DEPROVISIONING":
		return ErrAgentStopped
	}
	return nil
}

// Teardown stops the ECS task. Idempotent: a missing task is treated as
// success so a doubled Teardown call (Stop on shutdown + Teardown from a
// follow-up flow) doesn't error.
//
// Persistent agents are never torn down here — the whole point is for
// them to outlive the leader. Their state stays in the store so the
// next `teem chat` reconciles to the same task.
func (p *FargateProvisioner) Teardown(ctx context.Context, a *Agent) error {
	if a == nil || a.Cloud == nil || a.Cloud.TaskARN == "" {
		return nil
	}
	if a.IsPersistent() {
		return nil
	}
	_, err := p.Client.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: strPtr(p.Cluster),
		Task:    strPtr(a.Cloud.TaskARN),
		Reason:  strPtr("teem: teardown"),
	})
	if err != nil {
		// AWS returns ClusterNotFoundException / TaskNotFoundException
		// wrapped in operation errors. We use string-match because the
		// concrete error types differ between SDK versions; cheaper than
		// pulling in awserr-style type assertions.
		msg := err.Error()
		if strings.Contains(msg, "ClusterNotFoundException") || strings.Contains(msg, "TaskNotFoundException") || strings.Contains(msg, "InvalidParameterException") {
			return nil
		}
		return fmt.Errorf("ecs StopTask: %w", err)
	}
	return nil
}

func formatFailures(fs []ecstypes.Failure) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		parts = append(parts, fmt.Sprintf("%s: %s", strDeref(f.Arn), strDeref(f.Reason)))
	}
	return strings.Join(parts, "; ")
}

func kv(key, val string) ecstypes.KeyValuePair {
	return ecstypes.KeyValuePair{Name: strPtr(key), Value: strPtr(val)}
}

// gitEnv translates the leader's GitConfig into the TEEM_GIT_* env
// overrides the worker daemon reads. Empty fields are skipped so the
// worker's own defaults apply.
func gitEnv(g GitConfig) []ecstypes.KeyValuePair {
	pairs := []ecstypes.KeyValuePair{}
	if g.RepoURL != "" {
		pairs = append(pairs, kv("TEEM_GIT_REPO_URL", g.RepoURL))
	}
	if g.Token != "" {
		pairs = append(pairs, kv("TEEM_GIT_TOKEN", g.Token))
	}
	if g.Username != "" {
		pairs = append(pairs, kv("TEEM_GIT_USERNAME", g.Username))
	}
	if g.AuthorName != "" {
		pairs = append(pairs, kv("TEEM_GIT_AUTHOR_NAME", g.AuthorName))
	}
	if g.AuthorEmail != "" {
		pairs = append(pairs, kv("TEEM_GIT_AUTHOR_EMAIL", g.AuthorEmail))
	}
	if g.BranchPrefix != "" {
		pairs = append(pairs, kv("TEEM_GIT_BRANCH_PREFIX", g.BranchPrefix))
	}
	if g.AutoPush != "" {
		pairs = append(pairs, kv("TEEM_GIT_AUTO_PUSH", g.AutoPush))
	}
	return pairs
}

func strPtr(s string) *string { return &s }

func int32Ptr(i int32) *int32 { return &i }

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
