export type NodeDef = { name: string; kind: string; attrs: Record<string, unknown> };
export type EdgeDef = { from: string; to: string; when?: string; kind?: 'control' | 'data'; from_port?: string; to_port?: string };
export type Pipeline = { name: string; entry: string; nodes: Record<string, NodeDef>; edges: EdgeDef[]; timeout_ms: number; concurrency: number };
export type Catalog = {
  upstreams: Record<string, unknown>;
  surfaces: Record<string, any>;
  pipelines: Record<string, Pipeline>;
  hooks: Record<string, unknown>;
  policies: Record<string, unknown>;
  interceptors: unknown[];
};
export type Draft = { id: string; base: string; source: string; created: string };

let authToken = '';
export function setToken(value: string) { authToken = value; }
export function getToken() { return authToken; }

export async function api<T = any>(endpoint: string, method: 'GET'|'POST'|'PUT' = 'GET', body?: unknown): Promise<T> {
  const response = await fetch('/api/' + endpoint, {
    method,
    headers: { Authorization: `Bearer ${authToken}`, ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
    cache: 'no-store'
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}
