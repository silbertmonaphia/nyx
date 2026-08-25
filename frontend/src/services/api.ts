import axios from "axios";
import type { AxiosError, InternalAxiosRequestConfig } from "axios";
import { injectTraceparent } from "./telemetry";
import { useUiStore } from "../store/uiStore";
import { useAuthStore } from "../store/authStore";
import type { ApiError, AuthResponse } from "~/api/openapi";

const api = axios.create({
  baseURL: import.meta.env.VITE_API_URL || "http://localhost:8080/api",
  headers: {
    "Content-Type": "application/json",
  },
  // httpOnly __Host-nyx-access / __Host-nyx-refresh cookies
  // auto-attach on every request. Same-origin (Vite dev server
  // proxy / prod nginx location /api/) means the browser sends them
  // on same-site XHR without the cross-site SameSite=None escape
  // hatch. Authorization: Bearer injection is gone — the cookie
  // carries the access JWT instead.
  withCredentials: true,
});

// Single-flight refresh: while a refresh is in flight, every other
// concurrent 401 awaits the same promise instead of triggering a
// second `/api/refresh` call. The promise is cleared in `finally`
// so the next 401 after a settled refresh kicks off a fresh one.
let refreshing: Promise<boolean> | null = null;

/**
 * Exchange the refresh cookie for a fresh pair of auth cookies.
 * Uses raw `axios` (not the wrapped `api` instance) so the response
 * interceptor cannot recurse into another 401 retry. The cookie
 * rides on the request automatically because we still set
 * withCredentials=true on this bare instance; the body is empty
 * (the backend reads from `__Host-nyx-refresh` via the cookie
 * header). On success, push the refreshed user profile into the
 * store so any UI hint keyed off `user` stays in sync.
 *
 * Returns true on success, false if the refresh itself failed
 * (the store has already been logged out in that case).
 */
async function performRefresh(): Promise<boolean> {
  try {
    const resp = await axios.post<AuthResponse>(
      `${api.defaults.baseURL}/refresh`,
      {},
      { withCredentials: true },
    );
    if (resp.data.user) {
      useAuthStore.getState().setAuth({ user: resp.data.user });
    }
    return true;
  } catch {
    return false;
  }
}

/**
 * Request interceptor: stamps the W3C traceparent on every outbound
 * request so the backend can stitch traces back to the SPA. No
 * longer injects `Authorization: Bearer` — the httpOnly cookie
 * carries the access JWT now. The traceparent stamping runs on
 * every request including the 401 retry path.
 */
api.interceptors.request.use(
  (config) => {
    config.headers = config.headers ?? {};
    injectTraceparent(config.headers);
    return config;
  },
  (error) => Promise.reject(error),
);

/**
 * Pulls the WWW-Authenticate header off an axios error in a way that
 * tolerates both the lowercase `headers` map (browser / axios defaults)
 * and any case axios may have preserved.
 */
function getWwwAuthenticate(error: AxiosError): string | undefined {
  const headers = error.response?.headers;
  if (!headers) return undefined;
  const raw =
    (headers as Record<string, string | undefined>)["www-authenticate"] ??
    (headers as Record<string, string | undefined>)["WWW-Authenticate"];
  return raw;
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
      const isExpiredChallenge =
        !skipRefresh &&
        /error_description="expired"/i.test(getWwwAuthenticate(error) ?? "");

      if (error.response.status === 401) {
        if (isExpiredChallenge) {
          // Single-flight: if no refresh is in progress, kick one off;
          // everyone else awaits the same promise.
          if (!refreshing) {
            refreshing = performRefresh().finally(() => {
              refreshing = null;
            });
          }
          const ok = await refreshing;
          if (ok && config) {
            config._retried = true;
            // The browser auto-attaches the freshly rotated
            // __Host-nyx-access cookie on this replay, so we don't
            // stamp anything ourselves. Replay the original
            // request — the request interceptor re-runs (stamping
            // traceparent), the response interceptor sees `_retried`
            // and bails immediately if it 401s again.
            return api.request(config);
          }
          // Refresh itself failed (or there was no refresh cookie
          // to begin with). Fall through to the same logout path as
          // a hard 401.
        }

        useAuthStore.getState().logout();
        message = "Session expired. Please login again.";
      } else {
        const backendError = error.response.data as ApiError | undefined;
        if (backendError?.error) {
          message = backendError.error;
        } else {
          message = `Server error: ${error.response.status}`;
        }
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
