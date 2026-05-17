import { APIError } from './client';

// Mirrors cmd/teem/api_task_detail.go apiTaskRecord. Times come over
// the wire as RFC3339 strings; the SPA renders them via the existing
// agoShort-derived strings (UpdatedAgo, StageAgo) and shows the raw
// ISO in tooltips where useful.
export interface TaskRecord {
  id: string;
  title: string;
  status: string;
  stage: string;
  assigned_to?: string;
  notes?: string;
  // Origin records who filed the task. Legacy tasks default to
  // "operator" server-side; SPA falls back to "operator" too when the
  // field is missing for very old snapshots.
  origin?: string;
  parent_id?: string;
  evidence?: string[];
  created_at: string;
  updated_at: string;
  updated_ago?: string;
  stage_entered_at?: string;
  stage_ago?: string;
}

export interface TaskTimelineEvent {
  ts: string;
  kind: string;
  agent_id?: string;
  job_id?: string;
  message?: string;
  // Server-rendered human-readable line composed from kind + meta.
  // Prefer this over `message` for rendering; older snapshots may
  // omit it, in which case the SPA falls back to `message`.
  summary?: string;
  source: 'task' | 'job';
  meta?: Record<string, unknown>;
}

export interface TaskAgentRollup {
  agent_id: string;
  job_count: number;
  done: number;
  errored: number;
  pending: number;
  first_seen_at?: string;
  last_seen_at?: string;
  last_seen_ago?: string;
}

export interface TaskJob {
  job_id: string;
  agent_id?: string;
  status: string;
  started_at?: string;
  completed_at?: string;
  duration_ms?: number;
  summary?: string;
  transcript_bytes?: number;
  transcript_url?: string;
}

export interface TaskTokenJobUsage {
  job_id: string;
  agent_id?: string;
  model?: string;
  input: number;
  output: number;
  cache_create: number;
  cache_read: number;
}

export interface TaskTokens {
  input: number;
  output: number;
  cache_create: number;
  cache_read: number;
  jobs: TaskTokenJobUsage[];
}

export interface TaskDetailPayload {
  now: string;
  task: TaskRecord;
  timeline: TaskTimelineEvent[];
  agents: TaskAgentRollup[];
  jobs: TaskJob[];
  tokens?: TaskTokens;
}

export async function fetchTaskDetail(
  teamID: string,
  taskID: string,
  signal?: AbortSignal,
): Promise<TaskDetailPayload> {
  const path = `/api/teams/${encodeURIComponent(teamID)}/tasks/${encodeURIComponent(taskID)}`;
  const r = await fetch(path, {
    signal,
    headers: { Accept: 'application/json' },
    cache: 'no-store',
  });
  if (!r.ok) throw new APIError(r.status, `GET /tasks/${taskID} → HTTP ${r.status}`);
  return (await r.json()) as TaskDetailPayload;
}
