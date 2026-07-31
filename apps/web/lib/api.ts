// lib/api.ts — typed fetch wrapper for services/api
// Attaches Bearer token, parses RFC 7807 errors, handles cursor pagination.

export const API_URL = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";

const TOKEN_KEY = "ai_auditor_access_token";
const REFRESH_KEY = "ai_auditor_refresh_token";

export interface Problem {
  type: string;
  title: string;
  status: number;
  detail: string;
}

export class ApiError extends Error {
  status: number;
  problem?: Problem;

  constructor(status: number, problem?: Problem) {
    super(problem?.detail || problem?.title || `Request failed with status ${status}`);
    this.status = status;
    this.problem = problem;
  }
}

export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}

// ---- token management ----
export function setTokens(access: string, refresh: string) {
  if (typeof window === "undefined") return;
  localStorage.setItem(TOKEN_KEY, access);
  localStorage.setItem(REFRESH_KEY, refresh);
}

export function getAccessToken(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem(TOKEN_KEY);
}

export function getRefreshToken(): string | null {
  if (typeof window === "undefined") return null;
  return localStorage.getItem(REFRESH_KEY);
}

export function clearTokens() {
  if (typeof window === "undefined") return;
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(REFRESH_KEY);
}

// ---- core request ----
async function request<T>(
  path: string,
  options: RequestInit = {},
  retryOnUnauthorized = true,
): Promise<T> {
  const headers = new Headers(options.headers);
  headers.set("Content-Type", "application/json");

  const token = getAccessToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);

  const res = await fetch(`${API_URL}${path}`, { ...options, headers });

  if (res.status === 401 && retryOnUnauthorized) {
    const refreshed = await tryRefresh();
    if (refreshed) return request<T>(path, options, false);
    clearTokens();
    throw new ApiError(401, { type: "unauthorized", title: "Unauthorized", status: 401, detail: "Session expired" });
  }

  if (!res.ok) {
    let problem: Problem | undefined;
    try {
      problem = (await res.json()) as Problem;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, problem);
  }

  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

async function tryRefresh(): Promise<boolean> {
  const refresh = getRefreshToken();
  if (!refresh) return false;
  try {
    const res = await fetch(`${API_URL}/v1/auth/refresh`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: refresh }),
    });
    if (!res.ok) return false;
    const data = await res.json();
    setTokens(data.access_token, data.refresh_token || refresh);
    return true;
  } catch {
    return false;
  }
}

// ---- typed endpoints ----
export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown, idempotencyKey?: string) =>
    request<T>(path, {
      method: "POST",
      body: body !== undefined ? JSON.stringify(body) : undefined,
      headers: idempotencyKey ? { "Idempotency-Key": idempotencyKey } : {},
    }),
  patch: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "PATCH", body: body ? JSON.stringify(body) : undefined }),
  delete: <T>(path: string) => request<T>(path, { method: "DELETE" }),
};

// ---- pagination helper ----
export function cursorParams(cursor?: string | null, limit = 25): string {
  const params = new URLSearchParams({ limit: String(limit) });
  if (cursor) params.set("cursor", cursor);
  return params.toString();
}
