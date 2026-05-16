import {
  AuditEvent,
  AwaitingApprovalTask,
  DashboardTask,
  DecisionRow,
  Envelope,
  ModelUsage,
  StateSnapshot,
  TaskBuckets,
  useTeamStore,
} from './team';

// applyAuditEnvelope routes a wsbus `audit` envelope into a per-kind
// patch against the live snapshot. Replaces the previous debounced
// /state refetch — no HTTP round-trip on every burst. Kinds the SPA
// doesn't model are silently ignored; the next snapshot_invalidate (or
// page reload) eventually reconciles anything we miss.
//
// Mutation pattern matches ReadyAffordance in TasksTable: read current
// snapshot via getState(), build a new snapshot object with replaced
// sub-slices, and call setState({ snapshot: next }). Going through
// setState (not the store's setSnapshot) preserves lastSeq so the
// WebSocket loop keeps backfilling correctly across reconnects.
export function applyAuditEnvelope(env: Envelope): void {
  if (env.kind !== 'audit' || !env.event) return;
  const ev = env.event;
  const handler = handlers[ev.kind];
  if (!handler) return;
  const snap = useTeamStore.getState().snapshot;
  if (!snap) return;
  const next = handler(snap, ev);
  if (next && next !== snap) {
    useTeamStore.setState({ snapshot: next });
  }
}

type Handler = (snap: StateSnapshot, ev: AuditEvent) => StateSnapshot | null;

const handlers: Record<string, Handler> = {
  task_stage_changed: handleTaskStageChanged,
  decision_note: handleDecisionNote,
  blocker_note: handleBlockerNote,
  job_received: handleJobLifecycle,
  job_complete: handleJobLifecycle,
  job_error: handleJobLifecycle,
  job_interrupted: handleJobLifecycle,
  job_transcript_ready: handleJobLifecycle,
  worker_stopped: handleWorkerStopped,
  pulse_tick: handlePulseTick,
  channels_state: handleChannelsState,
  usage_event: handleUsageEvent,
  leader_status_changed: handleLeaderStatusChanged,
};

// bucketForStage mirrors cmd/teem/ui.go's per-task switch (Stage ==
// awaiting_approval → awaiting_approval; Status.IsShelved → shelved;
// StatusDone/StatusAbandoned → recent_done; everything else open). We
// only know the stage on the wire, but Status is a 1:1 derivation from
// the terminal stages so this is sound.
type Bucket = 'open' | 'awaiting_approval' | 'recent_done' | 'shelved';

function bucketForStage(stage: string): Bucket {
  switch (stage) {
    case 'awaiting_approval':
      return 'awaiting_approval';
    case 'verified':
    case 'abandoned':
      return 'recent_done';
    case 'shelved':
      return 'shelved';
    default:
      return 'open';
  }
}

function handleTaskStageChanged(snap: StateSnapshot, ev: AuditEvent): StateSnapshot | null {
  const meta = ev.meta ?? {};
  const taskID = typeof meta.task_id === 'string' ? meta.task_id : '';
  const toStage = typeof meta.to === 'string' ? meta.to : '';
  if (!taskID || !toStage) return null;
  return moveTaskStage(snap, taskID, toStage);
}

function handleBlockerNote(snap: StateSnapshot, ev: AuditEvent): StateSnapshot | null {
  const meta = ev.meta ?? {};
  const taskID = typeof meta.task_id === 'string' ? meta.task_id : '';
  if (!taskID) return null;
  // Stage → blocked AND prepend a BLOCKER decision row.
  const stagePatched = moveTaskStage(snap, taskID, 'blocked') ?? snap;
  const title = lookupTaskTitle(stagePatched, taskID) ?? taskID;
  const summary = (typeof meta.summary === 'string' && meta.summary) || ev.message || '';
  const row: DecisionRow = {
    type: 'BLOCKER',
    type_class: 'blocker',
    task_id: taskID,
    title,
    summary,
    age: '0s ago',
    url: taskURL(stagePatched, taskID),
    stripe: '',
    timestamp: ev.ts || '',
    actions: [],
  };
  return { ...stagePatched, decisions: [row, ...(stagePatched.decisions ?? [])] };
}

function handleDecisionNote(snap: StateSnapshot, ev: AuditEvent): StateSnapshot | null {
  const meta = ev.meta ?? {};
  const severity = typeof meta.severity === 'string' ? meta.severity : '';
  // DecisionsList only renders type_class === 'question'. Non-question
  // severities (info, warn, etc.) drive other UI; skip them rather
  // than polluting the panel with rows the operator can't act on.
  if (severity !== 'question') return null;
  const taskID = typeof meta.task_id === 'string' ? meta.task_id : '';
  const title = lookupTaskTitle(snap, taskID) ?? taskID;
  const summary = (typeof meta.summary === 'string' && meta.summary) || ev.message || '';
  const row: DecisionRow = {
    type: 'QUESTION',
    type_class: 'question',
    task_id: taskID,
    title,
    summary,
    age: '0s ago',
    url: taskURL(snap, taskID),
    stripe: '',
    timestamp: ev.ts || '',
    actions: [],
  };
  return { ...snap, decisions: [row, ...(snap.decisions ?? [])] };
}

// handleJobLifecycle bumps the matching worker row's `age` to "0s" as a
// proxy for last_seen. Worker.Activity is sourced server-side from
// leader_status / open-task assignment and isn't something we can
// derive from a single audit event, so we leave it alone.
function handleJobLifecycle(snap: StateSnapshot, ev: AuditEvent): StateSnapshot | null {
  const agent = ev.agent_id;
  if (!agent) return null;
  const workers = snap.workers ?? [];
  const idx = workers.findIndex((w) => w.agent_id === agent);
  if (idx < 0) return null;
  const cur = workers[idx];
  if (cur.age === '0s') return null;
  const next = workers.slice();
  next[idx] = { ...cur, age: '0s' };
  return { ...snap, workers: next };
}

function handleWorkerStopped(snap: StateSnapshot, ev: AuditEvent): StateSnapshot | null {
  const agent = ev.agent_id;
  if (!agent) return null;
  const workers = snap.workers ?? [];
  const filtered = workers.filter((w) => w.agent_id !== agent);
  if (filtered.length === workers.length) return null;
  return { ...snap, workers: filtered };
}

function handlePulseTick(snap: StateSnapshot, _ev: AuditEvent): StateSnapshot | null {
  if (!snap.pulse) return null;
  return {
    ...snap,
    pulse: {
      ...snap.pulse,
      tick_count: (snap.pulse.tick_count ?? 0) + 1,
      // PulseControls.computeCountdown reads "0s ago" as "now" and
      // restarts the countdown from the snapshot anchor.
      last_tick: '0s ago',
    },
  };
}

function handleChannelsState(snap: StateSnapshot, ev: AuditEvent): StateSnapshot | null {
  const meta = ev.meta ?? {};
  const state = typeof meta.state === 'string' ? meta.state : '';
  if (state !== 'live' && state !== 'fallback') return null;
  if (snap.channels_state === state) return null;
  return { ...snap, channels_state: state };
}

function handleLeaderStatusChanged(snap: StateSnapshot, ev: AuditEvent): StateSnapshot | null {
  const meta = ev.meta ?? {};
  const text = typeof meta.text === 'string' ? meta.text : '';
  if (!text) return null;
  const agentID =
    (typeof meta.agent_id === 'string' && meta.agent_id) || ev.agent_id || 'leader';
  const prev = snap.leader_status;
  const nextLeader = {
    ...(prev ?? { agent_id: agentID, text: '', updated_ago: '0s ago' }),
    agent_id: agentID,
    text,
    updated_ago: '0s ago',
  };
  return { ...snap, leader_status: nextLeader, status_headline: text };
}

function handleUsageEvent(snap: StateSnapshot, ev: AuditEvent): StateSnapshot | null {
  if (!snap.usage) return null;
  const meta = ev.meta ?? {};
  const input = numField(meta.input_tokens);
  const output = numField(meta.output_tokens);
  const cacheCreate = numField(meta.cache_create_tokens);
  const cacheRead = numField(meta.cache_read_tokens);
  const total = input + output + cacheCreate + cacheRead;
  if (total === 0) return null;
  const model = typeof meta.model === 'string' ? meta.model : '';
  const prevPer = snap.usage.per_model ?? [];
  let nextPer: ModelUsage[];
  if (model) {
    const idx = prevPer.findIndex((m) => m.model === model);
    if (idx >= 0) {
      const cur = prevPer[idx];
      const updated: ModelUsage = {
        model,
        input: cur.input + input,
        output: cur.output + output,
        cache_create: cur.cache_create + cacheCreate,
        cache_read: cur.cache_read + cacheRead,
        total: cur.total + total,
      };
      nextPer = prevPer.slice();
      nextPer[idx] = updated;
    } else {
      nextPer = [
        ...prevPer,
        {
          model,
          input,
          output,
          cache_create: cacheCreate,
          cache_read: cacheRead,
          total,
        },
      ];
    }
  } else {
    nextPer = prevPer;
  }
  const used = snap.usage.used + total;
  const cap = snap.usage.cap;
  const percent = cap > 0 ? Math.round((used / cap) * 1000) / 10 : snap.usage.percent_used;
  return {
    ...snap,
    usage: {
      ...snap.usage,
      used,
      percent_used: percent,
      per_model: nextPer,
    },
  };
}

// moveTaskStage updates a task's stage in whichever bucket holds it
// and moves it across buckets when the new stage demands. open ↔
// shelved ↔ recent_done share the DashboardTask shape and round-trip
// cleanly. awaiting_approval transitions are best-effort: we drop the
// task from its old bucket and synthesise a placeholder row with the
// fields we have (id, title, notes). Server-side fields (evidence
// rows, approve/reject/comment URLs) come back on the next /state
// fetch; until then ApprovalCard renders the row with empty buttons.
function moveTaskStage(snap: StateSnapshot, taskID: string, newStage: string): StateSnapshot | null {
  const tasks = snap.tasks;
  const target = bucketForStage(newStage);
  const stageAgo = '0s ago';

  // Search standard buckets first.
  let foundTask: DashboardTask | null = null;
  let foundIn: 'open' | 'shelved' | 'recent_done' | null = null;
  for (const b of ['open', 'shelved', 'recent_done'] as const) {
    const list = (tasks[b] ?? []) as DashboardTask[];
    const hit = list.find((r) => r.id === taskID);
    if (hit) {
      foundTask = hit;
      foundIn = b;
      break;
    }
  }
  let foundApproval: AwaitingApprovalTask | null = null;
  if (!foundTask) {
    const list = tasks.awaiting_approval ?? [];
    foundApproval = list.find((r) => r.id === taskID) ?? null;
  }
  if (!foundTask && !foundApproval) return null;

  let nextOpen = tasks.open ?? [];
  let nextShelved = tasks.shelved ?? [];
  let nextRecent = tasks.recent_done ?? [];
  let nextAwaiting = tasks.awaiting_approval ?? [];

  if (foundTask && foundIn) {
    if (foundIn === 'open') nextOpen = nextOpen.filter((r) => r.id !== taskID);
    else if (foundIn === 'shelved') nextShelved = nextShelved.filter((r) => r.id !== taskID);
    else nextRecent = nextRecent.filter((r) => r.id !== taskID);

    if (target === 'awaiting_approval') {
      nextAwaiting = [taskToAwaiting(foundTask, stageAgo), ...nextAwaiting];
    } else {
      const updated: DashboardTask = { ...foundTask, stage: newStage, stage_ago: stageAgo };
      if (target === 'open') nextOpen = appendIfMissing(nextOpen, updated);
      else if (target === 'shelved') nextShelved = [updated, ...nextShelved];
      else nextRecent = [updated, ...nextRecent];
    }
  } else if (foundApproval) {
    nextAwaiting = nextAwaiting.filter((r) => r.id !== taskID);
    if (target === 'awaiting_approval') {
      nextAwaiting = [{ ...foundApproval, stage_ago: stageAgo }, ...nextAwaiting];
    } else {
      const updated = approvalToTask(foundApproval, newStage, stageAgo);
      if (target === 'open') nextOpen = appendIfMissing(nextOpen, updated);
      else if (target === 'shelved') nextShelved = [updated, ...nextShelved];
      else nextRecent = [updated, ...nextRecent];
    }
  }

  const nextTasks: TaskBuckets = {
    ...tasks,
    open: nextOpen,
    shelved: nextShelved,
    recent_done: nextRecent,
    awaiting_approval: nextAwaiting,
  };
  return { ...snap, tasks: nextTasks };
}

function taskToAwaiting(t: DashboardTask, stageAgo: string): AwaitingApprovalTask {
  return {
    id: t.id,
    title: t.title,
    notes: t.notes ?? '',
    evidence_rows: null,
    has_plan_artifact: false,
    stage_ago: stageAgo,
    url: t.url ?? '',
    approve_url: '',
    reject_url: '',
    comment_url: '',
  };
}

function approvalToTask(a: AwaitingApprovalTask, stage: string, stageAgo: string): DashboardTask {
  return {
    id: a.id,
    title: a.title,
    status: '',
    stage,
    stage_ago: stageAgo,
    assigned_to: '',
    url: a.url,
    notes: a.notes,
  };
}

function appendIfMissing(list: DashboardTask[], row: DashboardTask): DashboardTask[] {
  if (list.some((r) => r.id === row.id)) return list;
  return [...list, row];
}

function lookupTaskTitle(snap: StateSnapshot, taskID: string): string | null {
  if (!taskID) return null;
  const tasks = snap.tasks;
  for (const b of ['open', 'shelved', 'recent_done'] as const) {
    const list = (tasks[b] ?? []) as DashboardTask[];
    const hit = list.find((r) => r.id === taskID);
    if (hit) return hit.title;
  }
  const ap = (tasks.awaiting_approval ?? []).find((r) => r.id === taskID);
  if (ap) return ap.title;
  return null;
}

function taskURL(snap: StateSnapshot, taskID: string): string {
  if (!taskID) return '';
  const teamID = snap.team?.id;
  if (!teamID) return '';
  return `/teams/${teamID}/tasks/${taskID}`;
}

function numField(v: unknown): number {
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (typeof v === 'string') {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }
  return 0;
}
