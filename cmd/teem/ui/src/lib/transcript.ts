// Shared transcript parsing helpers used by both <TranscriptPage>
// (the static rendered page at /teams/<id>/transcripts/<a>/<j>) and
// <WatchTranscriptModal> (the live SSE-driven modal). Keeping one
// parser means the live-streaming view and the post-mortem page render
// the same shape of summary for the same line — no drift in what an
// event looks like depending on which surface you open.
//
// claude stream-json shape (one JSON object per NDJSON line):
//   {type:"system",  subtype, ...}                 — session init / hook events
//   {type:"user",    message:{content: …}}         — user/tool-result turns
//   {type:"assistant", message:{content: …}}       — assistant turns (text or tool_use)
//   {type:"result",  subtype, total_cost_usd, num_turns, …}
// Anything else falls into the "unknown" bucket; malformed JSON lines
// are dropped (a half-written line in mid-write would otherwise show
// as an "unknown" row).

export type EventKind = 'system' | 'user' | 'assistant' | 'tool' | 'result' | 'unknown';

export interface TranscriptEvent {
  kind: EventKind;
  text: string;                       // glance-friendly summary line
  raw: string;                        // full JSON for the title= tooltip / <details>
  ts: number;                         // arrival time (wall clock, ms)
  toolName?: string;                  // populated when kind=='assistant' and the turn is a tool_use
  toolInput?: string;                 // pretty-printed JSON of the tool call's input
  toolResultPreview?: string;         // populated when kind=='tool' and the turn is a tool_result
}

// parseStreamLine maps one claude stream-json line to a TranscriptEvent.
// Returns null when the line is empty or malformed JSON; callers drop
// those so a truncated mid-write line doesn't render as "unknown".
export function parseStreamLine(raw: string): TranscriptEvent | null {
  const trimmed = raw.trim();
  if (!trimmed) return null;
  let obj: Record<string, unknown>;
  try {
    obj = JSON.parse(trimmed) as Record<string, unknown>;
  } catch {
    return null;
  }
  const type = typeof obj.type === 'string' ? (obj.type as string) : '';
  const now = Date.now();
  switch (type) {
    case 'system':
      return {
        kind: 'system',
        text: summarizeSystem(obj),
        raw: trimmed,
        ts: now,
      };
    case 'user': {
      const ev: TranscriptEvent = {
        kind: 'user',
        text: summarizeUser(obj),
        raw: trimmed,
        ts: now,
      };
      const msg = obj.message as Record<string, unknown> | undefined;
      if (msg) {
        const tr = extractToolResultPreview(msg);
        if (tr) {
          // tool_result rides on a `user` stream-json row but is really
          // the tool side of the conversation — surface as kind='tool'
          // so renderers and the .watch-event.tool styling pick it up.
          ev.kind = 'tool';
          ev.toolResultPreview = tr;
        }
      }
      return ev;
    }
    case 'assistant': {
      const msg = obj.message as Record<string, unknown> | undefined;
      const ev: TranscriptEvent = {
        kind: 'assistant',
        text: summarizeAssistant(obj),
        raw: trimmed,
        ts: now,
      };
      if (msg) {
        const tool = extractToolUse(msg);
        if (tool) {
          ev.kind = 'tool';
          ev.toolName = tool.name;
          ev.toolInput = tool.input;
        }
      }
      return ev;
    }
    case 'result':
      return {
        kind: 'result',
        text: summarizeResult(obj),
        raw: trimmed,
        ts: now,
      };
    default:
      return {
        kind: 'unknown',
        text: type || '(unknown)',
        raw: trimmed,
        ts: now,
      };
  }
}

function summarizeSystem(obj: Record<string, unknown>): string {
  const sub = stringField(obj, 'subtype');
  if (sub) return sub;
  return 'system';
}

function summarizeUser(obj: Record<string, unknown>): string {
  // claude stream-json wraps user messages in {message: {content: ...}}
  const msg = obj.message as Record<string, unknown> | undefined;
  if (msg) {
    const text = extractText(msg);
    if (text) return truncate(text, 200);
    const toolResult = extractToolResultPreview(msg);
    if (toolResult) return `tool_result: ${truncate(toolResult, 180)}`;
  }
  return 'user turn';
}

function summarizeAssistant(obj: Record<string, unknown>): string {
  const msg = obj.message as Record<string, unknown> | undefined;
  if (msg) {
    const text = extractText(msg);
    if (text) return truncate(text, 200);
    const tool = extractToolUse(msg);
    if (tool) return `Tool: ${tool.name}`;
  }
  return 'assistant turn';
}

function summarizeResult(obj: Record<string, unknown>): string {
  const sub = stringField(obj, 'subtype');
  const cost = obj.total_cost_usd;
  const turns = obj.num_turns;
  const bits: string[] = [];
  if (sub) bits.push(sub);
  if (typeof turns === 'number') bits.push(`${turns} turn${turns === 1 ? '' : 's'}`);
  if (typeof cost === 'number' && cost > 0) bits.push(`$${cost.toFixed(4)}`);
  return bits.length ? bits.join(' · ') : 'result';
}

export function extractText(msg: Record<string, unknown>): string {
  const content = msg.content;
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    const parts: string[] = [];
    for (const block of content) {
      if (block && typeof block === 'object') {
        const b = block as Record<string, unknown>;
        if (b.type === 'text' && typeof b.text === 'string') parts.push(b.text);
      }
    }
    return parts.join('\n');
  }
  return '';
}

export interface ToolUse {
  name: string;
  input: string;     // pretty-printed JSON, empty string if no input
}

export function extractToolUse(msg: Record<string, unknown>): ToolUse | null {
  const content = msg.content;
  if (!Array.isArray(content)) return null;
  for (const block of content) {
    if (block && typeof block === 'object') {
      const b = block as Record<string, unknown>;
      if (b.type === 'tool_use' && typeof b.name === 'string') {
        let input = '';
        if (b.input !== undefined) {
          try {
            input = JSON.stringify(b.input, null, 2);
          } catch {
            input = String(b.input);
          }
        }
        return { name: b.name, input };
      }
    }
  }
  return null;
}

export function extractToolResultPreview(msg: Record<string, unknown>): string {
  const content = msg.content;
  if (!Array.isArray(content)) return '';
  for (const block of content) {
    if (block && typeof block === 'object') {
      const b = block as Record<string, unknown>;
      if (b.type === 'tool_result') {
        const inner = b.content;
        if (typeof inner === 'string') return inner;
        if (Array.isArray(inner)) {
          for (const sub of inner) {
            if (sub && typeof sub === 'object') {
              const s = sub as Record<string, unknown>;
              if (typeof s.text === 'string') return s.text;
            }
          }
        }
        return 'result';
      }
    }
  }
  return '';
}

function stringField(obj: Record<string, unknown>, key: string): string {
  const v = obj[key];
  return typeof v === 'string' ? v : '';
}

export function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 1) + '…';
}

// parseNDJSON parses a full transcript body and returns one
// TranscriptEvent per non-empty, well-formed line. Malformed lines are
// silently dropped (callers can compare events.length against the raw
// line count if they want to surface a "parse skipped N lines" hint).
export function parseNDJSON(body: string): TranscriptEvent[] {
  const lines = body.split('\n');
  const out: TranscriptEvent[] = [];
  for (const line of lines) {
    const ev = parseStreamLine(line);
    if (ev) out.push(ev);
  }
  return out;
}
