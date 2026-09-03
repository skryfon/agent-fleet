const TOKEN_KEY = "af_admin_token";

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token);
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY);
}

export class UnauthorizedError extends Error {
  constructor() {
    super("unauthorized");
  }
}

// api<T> is every view's plain fetch — Authorization: Bearer, same
// admin-token gate GET /v1/projects already enforces
// (internal/api/middleware.go's authAdmin). A 401 clears the stored token so
// App re-shows the gate on the next render.
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const token = getToken();

  const res = await fetch(path, {
    ...init,
    headers: {
      ...(init?.headers ?? {}),
      Authorization: `Bearer ${token ?? ""}`,
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
    },
  });

  if (res.status === 401) {
    clearToken();
    throw new UnauthorizedError();
  }

  if (!res.ok) {
    const body = await res.text();
    throw new Error(`${res.status}: ${body}`);
  }

  if (res.status === 204) {
    return undefined as T;
  }

  return (await res.json()) as T;
}

export interface SSEFrame {
  id?: string;
  data: string;
}

// parseSSE splits a growing text buffer into complete "\n\n"-terminated SSE
// frames plus whatever trailing partial frame hasn't arrived yet — pure so
// it can be unit tested against chunk boundaries without a real stream.
export function parseSSE(buffer: string): [SSEFrame[], string] {
  const parts = buffer.split("\n\n");
  const remainder = parts.pop() ?? "";

  const frames: SSEFrame[] = [];

  for (const part of parts) {
    if (!part.trim()) continue;

    let id: string | undefined;
    const dataLines: string[] = [];

    for (const line of part.split("\n")) {
      if (line.startsWith("id: ")) {
        id = line.slice(4);
      } else if (line.startsWith("data: ")) {
        dataLines.push(line.slice(6));
      }
    }

    frames.push({ id, data: dataLines.join("\n") });
  }

  return [frames, remainder];
}

// streamEvents tails GET /v1/events?since= with a plain fetch + reader, not
// EventSource — EventSource cannot send an Authorization header, and the
// alternative (?token=) would put the admin token in every access log line
// (internal/api/middleware.go's slogRequest) and in browser history. signal
// aborts the read loop when the caller's effect cleans up.
export async function streamEvents(
  since: string | undefined,
  onEvent: (frame: SSEFrame) => void,
  signal: AbortSignal,
): Promise<void> {
  const token = getToken();
  const url = since ? `/v1/events?since=${encodeURIComponent(since)}` : "/v1/events";

  const res = await fetch(url, {
    headers: { Authorization: `Bearer ${token ?? ""}` },
    signal,
  });

  if (res.status === 401) {
    clearToken();
    throw new UnauthorizedError();
  }

  if (!res.ok || !res.body) {
    throw new Error(`GET ${url}: ${res.status}`);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  for (;;) {
    const { done, value } = await reader.read();
    if (done) return;

    buffer += decoder.decode(value, { stream: true });

    const [frames, remainder] = parseSSE(buffer);
    buffer = remainder;

    for (const frame of frames) onEvent(frame);
  }
}
