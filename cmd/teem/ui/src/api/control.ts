// Typed fetch wrappers for the /control/teams/<id>/... endpoints
// driven by the SPA's action-bearing panels (ApprovalCard,
// PulseControls). Each call throws APIError on non-2xx so the caller
// can roll back its optimistic state patch in a single catch.
//
// Auth model matches the rest of the SPA: tailnet boundary, no bearer
// token, same-origin fetch.

import { APIError } from './client';

interface JSONInit {
  body?: unknown;
  signal?: AbortSignal;
}

async function postJSON<T = unknown>(url: string, init: JSONInit = {}): Promise<T> {
  const r = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: init.body === undefined ? '{}' : JSON.stringify(init.body),
    signal: init.signal,
    cache: 'no-store',
  });
  if (!r.ok) {
    const text = await r.text().catch(() => '');
    throw new APIError(r.status, text.trim() || `POST ${url} → HTTP ${r.status}`);
  }
  // Some endpoints return an empty body on success; tolerate that.
  const text = await r.text();
  if (!text) return {} as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    return {} as T;
  }
}

// ---- Task decisions (approve / reject / comment) -------------------

export interface TaskActionResult {
  // The daemon returns the updated plan.Task; the SPA mostly only
  // needs to know "the call succeeded" so callers usually ignore this.
  id?: string;
  stage?: string;
  status?: string;
}

export function approveTask(
  teamID: string,
  taskID: string,
  comment: string,
  signal?: AbortSignal,
): Promise<TaskActionResult> {
  return postJSON<TaskActionResult>(
    `/control/teams/${encodeURIComponent(teamID)}/tasks/${encodeURIComponent(taskID)}/approve`,
    { body: { comment }, signal },
  );
}

export function rejectTask(
  teamID: string,
  taskID: string,
  reason: string,
  signal?: AbortSignal,
): Promise<TaskActionResult> {
  return postJSON<TaskActionResult>(
    `/control/teams/${encodeURIComponent(teamID)}/tasks/${encodeURIComponent(taskID)}/reject`,
    { body: { reason }, signal },
  );
}

export function commentTask(
  teamID: string,
  taskID: string,
  comment: string,
  signal?: AbortSignal,
): Promise<TaskActionResult> {
  return postJSON<TaskActionResult>(
    `/control/teams/${encodeURIComponent(teamID)}/tasks/${encodeURIComponent(taskID)}/comment`,
    { body: { comment }, signal },
  );
}

// ---- Pulse controls -----------------------------------------------

// PulseStatus mirrors cmd/teem/daemon.go pulseStatus (the JSON-shaped
// return value of every pulse POST). Kept loose: callers usually only
// need `running` / `paused` / `last_tick` after an action.
export interface PulseStatus {
  running: boolean;
  paused: boolean;
  interval: string;
  last_tick?: string;
  tick_count: number;
  wake_prompt: string;
  use_default_wake_prompt: boolean;
  default_wake_prompt: string;
}

export function startPulse(
  teamID: string,
  opts: { interval?: string; wakePrompt?: string | null } = {},
  signal?: AbortSignal,
): Promise<PulseStatus> {
  const body: Record<string, unknown> = {};
  if (opts.interval) body.interval = opts.interval;
  if (opts.wakePrompt !== undefined) body.wake_prompt = opts.wakePrompt;
  return postJSON<PulseStatus>(
    `/control/teams/${encodeURIComponent(teamID)}/pulse/start`,
    { body, signal },
  );
}

export function stopPulse(teamID: string, signal?: AbortSignal): Promise<PulseStatus> {
  return postJSON<PulseStatus>(
    `/control/teams/${encodeURIComponent(teamID)}/pulse/stop`,
    { body: {}, signal },
  );
}

export function pausePulse(
  teamID: string,
  reason: string,
  signal?: AbortSignal,
): Promise<PulseStatus> {
  return postJSON<PulseStatus>(
    `/control/teams/${encodeURIComponent(teamID)}/pulse/pause`,
    { body: { reason }, signal },
  );
}

export function resumePulse(teamID: string, signal?: AbortSignal): Promise<PulseStatus> {
  return postJSON<PulseStatus>(
    `/control/teams/${encodeURIComponent(teamID)}/pulse/resume`,
    { body: {}, signal },
  );
}

// configPulse drives /pulse/config — used by the interval form. An
// empty `interval` leaves the existing interval alone; passing
// wakePrompt=null clears the override and falls back to the default.
export function configPulse(
  teamID: string,
  opts: { interval?: string; wakePrompt?: string | null } = {},
  signal?: AbortSignal,
): Promise<PulseStatus> {
  const body: Record<string, unknown> = {};
  if (opts.interval) body.interval = opts.interval;
  if (opts.wakePrompt !== undefined) body.wake_prompt = opts.wakePrompt;
  return postJSON<PulseStatus>(
    `/control/teams/${encodeURIComponent(teamID)}/pulse/config`,
    { body, signal },
  );
}
