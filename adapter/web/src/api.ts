// Typed wrappers over the adapter's localhost admin + read APIs. The SPA is
// served under /admin and talks to the same origin. Most endpoints use the
// {ok,data} envelope; dispatch/recent returns a bare array.

export interface Project { name: string; path: string; source: string; editable: boolean }
export interface FsEntry { name: string; path: string }
export interface WorkspaceFileMeta { name: string; exists: boolean; size: number }
export interface SessionInfo {
  id: string; title?: string; cwd?: string; cwdName?: string;
  driver?: string; role?: string; createdAt?: string; lastUsedAt?: string;
}
export interface ChatMessage { role: string; content: string; seq: number; timestamp?: string }
export interface DispatchEntry {
  taskId: string; cwd: string; title: string; status: string;
  elapsedMs?: number; error?: string; startedAt: string;
}

async function envelope<T>(path: string, init?: RequestInit): Promise<T> {
  const r = await fetch(path, init);
  let body: any = {};
  try { body = await r.json(); } catch { /* ignore */ }
  if (!r.ok || body.ok === false) throw new Error(body.detail || body.error || `HTTP ${r.status}`);
  return (body.data ?? {}) as T;
}

/* ── status ── */
export async function health(): Promise<any> { try { return (await (await fetch("/healthz")).json()).data ?? {}; } catch { return {}; } }

/* ── drivers (ADR-023) ── */
export interface DriverCaps { butler?: boolean; resume?: boolean; streaming?: boolean }
export interface DriverRow {
  name: string;
  capabilities?: DriverCaps;
  installed?: boolean;        // omitted when detection has no opinion
  butler_capable?: boolean;
  active_model?: string;
}
export interface DriversState {
  active_driver: string;
  butler_driver: string;
  drivers: DriverRow[];
}
// drivers() returns the full driver-management state. The admin SPA reaches it
// via the loopback-gated /v1/admin/drivers (no device token needed).
export async function drivers(): Promise<DriversState> {
  try {
    const d = await envelope<DriversState>("/v1/admin/drivers");
    return { active_driver: d.active_driver ?? "", butler_driver: d.butler_driver ?? "", drivers: d.drivers ?? [] };
  } catch { return { active_driver: "", butler_driver: "", drivers: [] }; }
}
export async function setActiveDriver(name: string): Promise<void> {
  await envelope("/v1/admin/active_driver", {
    method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name }),
  });
}

/* ── settings (ADR-025) ── */
export interface AsrSettings {
  provider: string; base_url: string; ws_url: string; app_id: string; api_key: string;
  resource_id: string; model: string; language: string;
  local_bin: string; local_args: string; local_text_path: string;
}
export interface TtsSettings {
  provider: string; token: string; app_id: string; cluster: string; voice: string;
  ws_url: string; local_bin: string; local_args: string; local_output_format: string;
}
export interface Settings {
  version?: number;
  topology: { cloud_relay_enabled: boolean; local_voice_enabled: boolean };
  ai: { anthropic_base_url: string; anthropic_auth_token: string };
  voice: { asr: AsrSettings; tts: TtsSettings; save_audio: boolean; save_input_on_finish: boolean };
  cloud: { ws_url: string; auth_token: string; home_site_id: string };
  openclaw: { ws_url: string; auth_token: string; node_id: string };
}
export interface SettingsState { settings: Settings; restart_required: boolean }

export async function getSettings(): Promise<SettingsState> {
  return envelope<SettingsState>("/v1/admin/settings");
}
// putSettings accepts a partial document; the server merges it over the current
// settings.json (present-then-modify), so each page can PUT only its own slice
// without clobbering another page's edits.
export async function putSettings(patch: Record<string, any>): Promise<{ restart_required: boolean }> {
  return envelope("/v1/admin/settings", {
    method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(patch),
  });
}
export async function restartAdapter(): Promise<void> {
  await envelope("/v1/admin/restart", { method: "POST" });
}

/* ── projects ── */
export async function listProjects(): Promise<Project[]> {
  return (await envelope<{ projects: Project[] }>("/v1/admin/projects")).projects ?? [];
}
export async function addProject(path: string): Promise<Project> {
  return (await envelope<{ project: Project }>("/v1/admin/projects", {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ path }),
  })).project;
}
export async function removeProject(name: string): Promise<void> {
  await envelope(`/v1/admin/projects/${encodeURIComponent(name)}`, { method: "DELETE" });
}

/* ── filesystem picker ── */
export async function browseDir(path?: string): Promise<{ path: string; parent: string; dirs: FsEntry[] }> {
  const q = path ? `?path=${encodeURIComponent(path)}` : "";
  return envelope(`/v1/admin/fs${q}`);
}
export async function searchDir(root: string, q: string): Promise<{ dirs: FsEntry[]; truncated: boolean }> {
  return envelope(`/v1/admin/fs/search?path=${encodeURIComponent(root)}&q=${encodeURIComponent(q)}`);
}

/* ── workspace files ── */
export async function workspaceFiles(): Promise<WorkspaceFileMeta[]> {
  return (await envelope<{ files: WorkspaceFileMeta[] }>("/v1/admin/workspace-files")).files ?? [];
}
export async function workspaceFile(name: string): Promise<{ name: string; exists: boolean; content: string; truncated?: boolean }> {
  return envelope(`/v1/admin/workspace-file?name=${encodeURIComponent(name)}`);
}

/* ── conversation records ── */
export async function listSessions(): Promise<SessionInfo[]> {
  // kind=logical → the butler's per-device logical sessions (the conversations).
  return (await envelope<{ sessions: SessionInfo[] }>("/v1/admin/sessions?kind=logical")).sessions ?? [];
}
export async function sessionMessages(id: string, driver: string, limit = 200, before = -1): Promise<{ messages: ChatMessage[]; total: number; hasMore: boolean }> {
  const d = driver ? `&driver=${encodeURIComponent(driver)}` : "";
  return envelope(`/v1/admin/sessions/${encodeURIComponent(id)}/messages?before=${before}&limit=${limit}${d}`);
}
export async function dispatchRecent(limit = 30): Promise<DispatchEntry[]> {
  // This endpoint returns a bare JSON array, not the {ok,data} envelope.
  try {
    const r = await fetch(`/v1/admin/dispatch/recent?limit=${limit}`);
    if (!r.ok) return [];
    const arr = await r.json();
    return Array.isArray(arr) ? arr : [];
  } catch { return []; }
}
