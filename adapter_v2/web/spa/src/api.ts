// Typed fetch client for the adapter_v2 agent-session HTTP API.
//
// Every endpoint returns the envelope {ok, data} (or {ok:false, error}); the
// helper below unwraps `data` and throws on a non-ok response so views only
// deal with the payload.

export interface SessionMeta {
  id: string;
  title: string;
  lastUsedAt: number; // unix epoch seconds (server emits an integer)
  active: boolean;
}

export interface SessionsPayload {
  sessions: SessionMeta[];
  active: string;
}

export interface Message {
  role: "user" | "assistant";
  content: string;
  timestamp: string; // rfc3339
  seq: number;
}

export interface MessagesPayload {
  messages: Message[];
  total: number;
  hasMore: boolean;
}

interface Envelope<T> {
  ok: boolean;
  data?: T;
  error?: string;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  let body: Envelope<T>;
  try {
    body = (await resp.json()) as Envelope<T>;
  } catch {
    throw new Error(`${resp.status} ${resp.statusText}: invalid JSON response`);
  }
  if (!resp.ok || !body.ok) {
    throw new Error(body.error || `${resp.status} ${resp.statusText}`);
  }
  return body.data as T;
}

export const api = {
  health(): Promise<string> {
    return fetch("/healthz").then((r) => r.text());
  },

  listSessions(): Promise<SessionsPayload> {
    return request<SessionsPayload>("/v1/agent/sessions");
  },

  messages(
    id: string,
    opts: { before?: number; limit?: number } = {}
  ): Promise<MessagesPayload> {
    const q = new URLSearchParams();
    if (opts.before != null) q.set("before", String(opts.before));
    if (opts.limit != null) q.set("limit", String(opts.limit));
    const qs = q.toString();
    return request<MessagesPayload>(
      `/v1/agent/sessions/${encodeURIComponent(id)}/messages${
        qs ? `?${qs}` : ""
      }`
    );
  },

  activate(id: string): Promise<{ active: string }> {
    return request<{ active: string }>(
      `/v1/agent/sessions/${encodeURIComponent(id)}/activate`,
      { method: "POST" }
    );
  },

  // sendInput types a text turn into a conversation (the web composer's
  // equivalent of a voice turn). Only the active session has a live PTY, so the
  // server rejects input to any other id with 409 — switch (activate) first.
  sendInput(id: string, text: string): Promise<{ sent: boolean }> {
    return request<{ sent: boolean }>(
      `/v1/agent/sessions/${encodeURIComponent(id)}/input`,
      { method: "POST", body: JSON.stringify({ text }) }
    );
  },

  newSession(): Promise<{ active: string }> {
    return request<{ active: string }>("/v1/agent/sessions/new", {
      method: "POST",
    });
  },

  deleteSession(id: string): Promise<{ active: string }> {
    return request<{ active: string }>(
      `/v1/agent/sessions/${encodeURIComponent(id)}`,
      { method: "DELETE" }
    );
  },
};
