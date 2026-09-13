// API client for the admin surface (plan §5.2, research §10 C-1..C-6).
//
// - Every call is same-origin (connect-src 'self', P-5); cookies ride the
//   session automatically (credentials: 'same-origin').
// - Mutations carry Content-Type: application/json (C-4 — a non-simple
//   content type forces a CORS preflight) and the X-CSRF-Token header
//   (C-3). The token comes from login/bootstrap responses and from the
//   X-CSRF-Token header on every authenticated response — the server
//   re-issues it there, so a page refresh (memory lost, session cookie
//   alive) recovers the token with the first authenticated GET.
// - Error bodies are the admin API's uniform {"error": "..."} shape.

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

// The CSRF token for this session, captured from login/bootstrap responses.
let csrfToken: string | null = null;

export function setCsrfToken(token: string): void {
  csrfToken = token;
}

export function clearCsrfToken(): void {
  csrfToken = null;
}

async function authCall(path: string, username: string, password: string): Promise<void> {
  const res = await fetch(path, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  const payload = (await parseBody(res)) as { csrf_token?: unknown } | null;
  const token = payload?.csrf_token;
  if (typeof token === 'string' && token !== '') {
    csrfToken = token;
  }
}

// adoptReissuedToken picks up the server's re-issued CSRF token from an
// authenticated response. A non-empty header always refreshes the stored
// token (the server re-issues per session; staying current costs nothing).
function adoptReissuedToken(res: Response): void {
  const token = res.headers.get('X-CSRF-Token');
  if (token !== null && token !== '') {
    csrfToken = token;
  }
}

export async function login(username: string, password: string): Promise<void> {
  await authCall('/api/admin/login', username, password);
}

// bootstrap creates the FIRST admin on a fresh instance; login is for
// every later one. Both yield the same logged-in session state.
export async function bootstrap(username: string, password: string): Promise<void> {
  await authCall('/api/admin/bootstrap', username, password);
}

export async function logout(): Promise<void> {
  try {
    await apiCall('POST', '/api/admin/logout');
  } finally {
    csrfToken = null;
  }
}

// apiCall is the generic session-authenticated admin call. Every
// non-GET/HEAD request needs the CSRF header (C-3) — logout included.
// `signal` lets route components abort in-flight loads on unmount.
export async function apiCall<T>(
  method: string,
  path: string,
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }
  if (method !== 'GET' && method !== 'HEAD') {
    if (csrfToken === null) {
      throw new ApiError(0, 'Not authenticated — no CSRF token for this session');
    }
    headers['X-CSRF-Token'] = csrfToken;
  }
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });
  adoptReissuedToken(res);
  return (await parseBody(res)) as T;
}

// parseBody turns a response into its JSON payload, throwing the uniform
// ApiError on any non-2xx status (204 → null).
async function parseBody(res: Response): Promise<unknown> {
  if (res.status === 204) {
    return null;
  }
  const payload = await res.json().catch(() => null);
  if (!res.ok) {
    const message =
      payload !== null && typeof payload === 'object' && 'error' in payload
        ? String((payload as { error: unknown }).error)
        : `Request failed (${res.status})`;
    throw new ApiError(res.status, message);
  }
  return payload;
}