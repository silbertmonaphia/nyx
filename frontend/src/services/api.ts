import axios from "axios";
import type { AxiosError, InternalAxiosRequestConfig } from "axios";
import { injectTraceparent } from "./telemetry";
import { useUiStore } from "../store/uiStore";
import { useAuthStore } from "../store/authStore";
import { logger } from "./logger";
import type { ApiError, AuthResponse } from "~/api/openapi";

const api = axios.create({
  baseURL: import.meta.env.VITE_API_URL || "http://localhost:8080/api",
  headers: {
    "Content-Type": "application/json",
  },
  // Bearer-only transport (RFC 6750). Tokens ride on the request
  // body for /api/login, /api/register, /api/refresh, /api/logout and
  // on the Authorization header for every other endpoint. Cookies are
  // not used — native clients (iOS / Android / Unity / Unreal / PS5)
  // don't share the browser cookie jar and `__Host-` constraints
  // (Domain="") forbid cross-subdomain cookies anyway.
  withCredentials: false,
});

// tokenStore is the single source of truth for the in-memory access
// + refresh tokens. It is intentionally a module-level singleton
// rather than part of the Zustand auth store: tokens must NEVER be
// persisted to localStorage (persisted tokens are an XSS-exfiltration
// target — see authStore.partialize), and they must NEVER travel
// through React state (React re-renders would race the refresh-on-
// 401 single-flight logic).
//
// sessionStorage is consulted on store init so a page reload within
// the same tab keeps the session — per-tab, not shared across tabs.
// Multi-tab sharing is a known limitation documented in FUTURE.md.
const SESSION_STORAGE_KEY = "nyx-token-store";

interface PersistedTokens {
  accessToken: string;
  refreshToken: string;
}

function readSessionTokens(): PersistedTokens | null {
  try {
    const raw = window.sessionStorage.getItem(SESSION_STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<PersistedTokens>;
    if (
      typeof parsed.accessToken === "string" &&
      typeof parsed.refreshToken === "string" &&
      parsed.accessToken !== "" &&
      parsed.refreshToken !== ""
    ) {
      return {
        accessToken: parsed.accessToken,
        refreshToken: parsed.refreshToken,
      };
    }
    return null;
  } catch {
    return null;
  }
}

function writeSessionTokens(at: string, rt: string): void {
  try {
    window.sessionStorage.setItem(
      SESSION_STORAGE_KEY,
      JSON.stringify({ accessToken: at, refreshToken: rt }),
    );
  } catch {
    // sessionStorage unavailable (private mode quota, SSR) — fall
    // back to in-memory only. Page reload will require a fresh
    // login.
  }
}

function clearSessionTokens(): void {
  try {
    window.sessionStorage.removeItem(SESSION_STORAGE_KEY);
  } catch {
    // ignored
  }
}

let accessToken: string | null = null;
let refreshToken: string | null = null;

export const tokenStore = {
  getAccessToken(): string | null {
    return accessToken;
  },
  getRefreshToken(): string | null {
    return refreshToken;
  },
  setTokens(at: string, rt: string): void {
    accessToken = at;
    refreshToken = rt;
    writeSessionTokens(at, rt);
  },
  clear(): void {
    accessToken = null;
    refreshToken = null;
    clearSessionTokens();
  },
};

// Hydrate from sessionStorage on module load so a page reload within
// the same tab keeps the session. Subsequent setTokens / clear calls
// keep the in-memory and sessionStorage views in lockstep.
(function hydrate() {
  const persisted = readSessionTokens();
  if (persisted) {
    accessToken = persisted.accessToken;
    refreshToken = persisted.refreshToken;
  }
})();

// Single-flight refresh: while a refresh is in flight, every other
// concurrent 401 awaits the same promise instead of triggering a
// second `/api/refresh` call. The promise is cleared in `finally`
// so the next 401 after a settled refresh kicks off a fresh one.
//
// The slot is module-private and the helper below (`refreshTokensAndReplay`)
// is the only way to enter it — keeping the single-flight invariant
// intact even when non-axios callers (chatService) need to participate.
let refreshing: Promise<boolean> | null = null;

/**
 * Exchange the refresh token (in the request body) for a fresh
 * Bearer pair. Uses raw `axios` (not the wrapped `api` instance) so
 * the response interceptor cannot recurse into another 401 retry. On
 * success, push the new tokens into the tokenStore and update the
 * user profile on the auth store so any UI hint keyed off `user`
 * stays in sync.
 *
 * Returns true on success, false if the refresh itself failed
 * (the store has already been logged out in that case).
 */
async function performRefresh(): Promise<boolean> {
  const rt = tokenStore.getRefreshToken();
  if (!rt) {
    return false;
  }
  try {
    const resp = await axios.post<AuthResponse>(
      `${api.defaults.baseURL}/refresh`,
      { refresh_token: rt },
      { withCredentials: false },
    );
    if (resp.data.access_token && resp.data.refresh_token) {
      tokenStore.setTokens(resp.data.access_token, resp.data.refresh_token);
    }
    if (resp.data.user) {
      useAuthStore.getState().setAuth({ user: resp.data.user });
    }
    return true;
  } catch {
    return false;
  }
}

/**
 * Public refresh entry point. Runs the single-flight refresh and
 * returns `true` when the tokenStore holds a fresh pair, `false`
 * otherwise. On a `false` return the caller MUST stop retrying —
 * the refresh token is gone (or rejected) and the only correct
 * outcome is to surface a logged-out state.
 *
 * Used by:
 *  - The axios response interceptor (the original caller).
 *  - Non-axios callers (chatService) that need to replicate the
 *    refresh-on-401 contract on raw `fetch` — axios's interceptors
 *    don't run on `fetch`, so chat runs the same single-flight
 *    machinery manually via this helper.
 */
export async function refreshTokensAndReplay(): Promise<boolean> {
  if (!refreshing) {
    refreshing = performRefresh().finally(() => {
      refreshing = null;
    });
  }
  return refreshing;
}

/**
 * Request interceptor: stamps the W3C traceparent + the Bearer
 * access token on every outbound request. The Bearer stamp is a
 * no-op when no token is present (the public endpoints — register,
 * login, refresh, health, get-movies — work anonymously). The
 * traceparent stamping runs on every request including the 401
 * retry path.
 */
api.interceptors.request.use(
  (config) => {
    config.headers = config.headers ?? {};
    injectTraceparent(config.headers);
    const at = tokenStore.getAccessToken();
    if (at) {
      // axios stores headers on an AxiosHeaders instance; set() works
      // for both the instance form and the plain-object form.
      (config.headers as Record<string, string>).Authorization =
        `Bearer ${at}`;
    }
    return config;
  },
  (error) => Promise.reject(error),
);

/**
 * Pulls a header off an axios error response in a way that
 * tolerates both the lowercase `headers` map (browser / axios
 * defaults) and any case axios may have preserved.
 */
function getHeader(error: AxiosError, name: string): string | undefined {
  const headers = error.response?.headers;
  if (!headers) return undefined;
  const lower = name.toLowerCase();
  for (const [k, v] of Object.entries(headers as Record<string, unknown>)) {
    if (k.toLowerCase() === lower && typeof v === "string") {
      return v;
    }
  }
  return undefined;
}

/**
 * Build the user-visible toast text for a non-401 server response.
 *
 * SECURITY.md H7: the backend's `error` field is NEVER echoed
 * directly — server-side strings are trusted less than client-side
 * ones (a future regression could leak DB fragments, validator
 * paths, or internal messages). The toast only carries a generic
 * label plus the request id, so the user has something to quote
 * when reporting an issue but the wire payload stays server-side.
 * The full payload is logged via the structured logger, where the
 * log-pipeline access controls apply.
 */
function buildServerErrorToast(
  status: number,
  requestId: string | undefined,
): string {
  const ref = requestId ? ` (ref: ${requestId})` : "";
  return `Server error (${status})${ref}`;
}

const MAX_LOG_PAYLOAD_CHARS = 2_000;

/**
 * Truncate and sanitise a server payload before logging. The
 * server payload could in principle contain control characters
 * (CRLF for log injection), HTML, or oversized blobs. zerolog
 * escapes JSON, so the log injection risk is low, but we still
 * cap the size to keep the log pipeline healthy and to bound the
 * PII surface if a payload ever carries user data. The cap
 * applies to the LOG view only — the toast never sees this
 * payload (see buildServerErrorToast).
 */
function sanitiseForLog(value: unknown): unknown {
  if (typeof value === "string") {
    return value
      .replace(/[\x00-\x1F\x7F-\x9F]/g, "")
      .slice(0, MAX_LOG_PAYLOAD_CHARS);
  }
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      out[k] = sanitiseForLog(v);
    }
    return out;
  }
  return value;
}

// Response interceptor for global error handling
api.interceptors.response.use(
  (response) => {
    return response;
  },
  async (error: AxiosError) => {
    let message = "An unexpected error occurred";

    if (error.response) {
      // Handle 401 Unauthorized.
      const config = error.config as
        | (InternalAxiosRequestConfig & {
            skipAuthRefresh?: boolean;
            _retried?: boolean;
          })
        | undefined;
      const skipRefresh = !config || config.skipAuthRefresh || config._retried;

      if (error.response.status === 401) {
        // SECURITY.md H8: any 401 triggers a refresh attempt unless
        // we've already retried this request, the caller opted out
        // via `skipAuthRefresh`, or the response came back without
        // a config object at all. Only attempt refresh when an
        // access token is actually present — anonymous requests
        // (e.g. /api/me on cold start with no session) should
        // skip straight to logout.
        const hasAccessToken = tokenStore.getAccessToken() !== null;
        if (!skipRefresh && hasAccessToken) {
          // Single-flight refresh shared with non-axios callers
          // (chatService). Concurrent 401s collapse onto the same
          // `refreshing` promise; `refreshTokensAndReplay` returns
          // `false` on refresh failure (no replay attempted).
          const ok = await refreshTokensAndReplay();
          if (ok && config) {
            config._retried = true;
            // Replay the original request — the request interceptor
            // re-runs and stamps the freshly rotated Bearer token
            // onto the retry. If the retry 401s again, the response
            // interceptor sees `_retried` and bails immediately to
            // logout (no infinite loop).
            return api.request(config);
          }
          // Refresh itself failed (e.g. the refresh token was
          // revoked server-side). Fall through to the same logout
          // path as a hard 401.
        }

        tokenStore.clear();
        useAuthStore.getState().logout();
        message = "Session expired. Please login again.";
      } else {
        // SECURITY.md H7: never echo the backend `error` field
        // to the toast. Pull the request id off the response
        // headers (chi's RequestID middleware sets X-Request-Id),
        // log the full payload for the operator, and show a
        // generic label to the user.
        const status = error.response.status;
        const requestId = getHeader(error, "X-Request-Id");
        const payload = error.response.data as ApiError | undefined;
        logger.error("api.error", {
          status,
          requestId,
          payload: sanitiseForLog(payload),
        });
        message = buildServerErrorToast(status, requestId);
      }
    } else if (error.request) {
      message = "No response from server. Please check your connection.";
    } else {
      message = error.message;
    }

    useUiStore.getState().addToast(message, "error");

    return Promise.reject(error);
  },
);

export default api;