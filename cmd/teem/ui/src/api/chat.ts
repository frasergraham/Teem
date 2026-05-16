// SSE client for POST /control/teams/<id>/chat. The endpoint streams
// Server-Sent Events (see cmd/teem/chat_handler.go): default-channel
// frames carry assistant text chunks; an `event: done` frame closes a
// successful turn; an `event: error` frame carries an error string.
//
// AbortController-driven cancellation: the caller passes `signal`, and
// aborting it both halts the fetch read loop and trips a flag that
// suppresses any in-flight callbacks. There is no server-side cancel
// endpoint — closing the response body via abort propagates through
// the request context inside the daemon (r.Context() cancel kills the
// claude subprocess in chat_handler.go).

export interface ChatStreamHandlers {
  onText(chunk: string): void;
  onDone(): void;
  onError(msg: string): void;
}

export async function streamChat(
  teamID: string,
  message: string,
  signal: AbortSignal,
  handlers: ChatStreamHandlers,
): Promise<void> {
  let resp: Response;
  try {
    resp = await fetch(`/control/teams/${encodeURIComponent(teamID)}/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message }),
      signal,
    });
  } catch (err) {
    if (signal.aborted) return;
    handlers.onError((err as Error).message || String(err));
    return;
  }

  if (!resp.ok || !resp.body) {
    let text = '';
    try {
      text = await resp.text();
    } catch {
      // ignore
    }
    handlers.onError(text.trim() || `HTTP ${resp.status}`);
    return;
  }

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  let sawError = false;
  let sawDone = false;

  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        const parsed = parseFrame(frame);
        if (parsed === null) continue;
        if (parsed.event === 'done') {
          sawDone = true;
        } else if (parsed.event === 'error') {
          sawError = true;
          handlers.onError(parsed.data);
        } else {
          // Default channel — assistant text chunk.
          handlers.onText(parsed.data);
        }
      }
    }
  } catch (err) {
    if (signal.aborted) return;
    handlers.onError((err as Error).message || String(err));
    return;
  }

  if (signal.aborted) return;
  if (!sawError) handlers.onDone();
  // sawDone is informational; success path emits onDone regardless so
  // the UI flips out of the pending state even if the server closed
  // without an explicit done frame.
  void sawDone;
}

function parseFrame(frame: string): { event: string; data: string } | null {
  let event = '';
  const dataLines: string[] = [];
  for (const raw of frame.split('\n')) {
    if (!raw) continue;
    if (raw.startsWith('event:')) {
      event = raw.slice(6).trim();
    } else if (raw.startsWith('data:')) {
      // SSE spec: a single leading space after the colon is stripped.
      const rest = raw.slice(5);
      dataLines.push(rest.startsWith(' ') ? rest.slice(1) : rest);
    }
  }
  if (event === '' && dataLines.length === 0) return null;
  return { event, data: dataLines.join('\n') };
}
