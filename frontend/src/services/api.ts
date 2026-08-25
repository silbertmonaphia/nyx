import axios from 'axios';
import type { AxiosError, InternalAxiosRequestConfig } from 'axios';
import { injectTraceparent } from './telemetry';
import { useUiStore } from '../store/uiStore';
import { useAuthStore } from '../store/authStore';
import type { ApiError, AuthResponse } from '~/api/openapi';

const api = axios.create({
  baseURL: import.meta.env.VITE_API_URL || 'http://localhost:8080/api',
  headers: {
    'Content-Type': 'application/json',
  },
});

// Single-flight refresh: while a refresh is in flight, every other
// concurrent 401 awaits the same promise instead of triggering a
// second `/api/refresh` call. The promise is cleared in `finally`
// so the next 401 after a settled refresh kicks off a fresh one.
let refreshing: Promise<string | null> | null = null;

/**
 * Exchange the stored refresh token for a fresh access + refresh pair.
 * Uses raw `axios` (not the wrapped `api` instance) so the response
 * interceptor cannot recurse into another 401 retry.
 *
 * Returns the new access token on success, or `null` if the refresh
 * itself failed (in which case the store has already been logged out).
 */
async function performRefresh(): Promise<string | null> {
  const refreshToken = useAuthStore.getState().refreshToken;
  if (!refreshToken) {
    return null;
  }
  try {
    const resp = await axios.post<AuthResponse>(
      `${api.defaults.baseURL}/refresh`,
      { refresh_token: refreshToken },
    );
    useAuthStore.getState().setAuth(resp.data);
    return resp.data.token;
  } catch {
    return null;
  }
}

// Request interceptor for adding auth token.
// Skips Authorization injection if the request already carries one —
// the 401 retry path injects the freshly minted access token there
// before replaying the original request.
api.interceptors.request.use(
  (config) => {
    if (!config.headers.Authorization) {
      const token = useAuthStore.getState().token;
      if (token) {
        config.headers.Authorization = `Bearer ${token}`;
      }
    }
    // Stamp the W3C traceparent on every outbound request so the
    // backend can stitch traces back to the SPA. Runs AFTER the auth
    // header (whether we stamped it or the retry path supplied it)
    // so both end up on the wire; noop when telemetry is disabled
    // or there is no active span.
    config.headers = config.headers ?? {};
    injectTraceparent(config.headers);
    return config;
  },
  (error) => Promise.reject(error)
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
    (headers as Record<string, string | undefined>)['www-authenticate'] ??
    (headers as Record<string, string | undefined>)['WWW-Authenticate'];
  return raw;
}

// Response interceptor for global error handling
api.interceptors.response.use(
  (response) => {
    return response;
  },
  async (error: AxiosError) => {
    let message = 'An unexpected error occurred';

    if (error.response) {
      // Handle 401 Unauthorized.
      const config = error.config as (InternalAxiosRequestConfig & {
        skipAuthRefresh?: boolean;
        _retried?: boolean;
      }) | undefined;
      const skipRefresh = !config || config.skipAuthRefresh || config._retried;
      const isExpiredChallenge =
        !skipRefresh && /error_description="expired"/i.test(getWwwAuthenticate(error) ?? '');

      if (error.response.status === 401) {
        if (isExpiredChallenge) {
          // Single-flight: if no refresh is in progress, kick one off;
          // everyone else awaits the same promise.
          if (!refreshing) {
            refreshing = performRefresh().finally(() => {
              refreshing = null;
            });
          }
          const newToken = await refreshing;
          if (newToken && config) {
            config._retried = true;
            config.headers = config.headers ?? {};
            config.headers.Authorization = `Bearer ${newToken}`;
            // Replay the original request with the fresh token. `api.request`
            // re-runs the request interceptor (which now sees the header
            // and skips re-injection) and the response interceptor (which
            // sees `_retried` and bails immediately if it 401s again).
            return api.request(config);
          }
          // Refresh itself failed (or there was no refresh token to begin
          // with). Fall through to the same logout path as a hard 401.
        }

        useAuthStore.getState().logout();
        message = 'Session expired. Please login again.';
      } else {
        const backendError = error.response.data as ApiError | undefined;
        if (backendError?.error) {
          message = backendError.error;
        } else {
          message = `Server error: ${error.response.status}`;
        }
      }
    } else if (error.request) {
      message = 'No response from server. Please check your connection.';
    } else {
      message = error.message;
    }

    useUiStore.getState().addToast(message, 'error');

    return Promise.reject(error);
  }
);

export default api;