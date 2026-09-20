import { QueryClient } from '@tanstack/react-query';

// Shared singleton so non-React modules (token refresh in
// services/api.ts, the user-change reset in features/feeds/services/
// resetFeedState.ts, etc.) can read and write the cache without
// having to thread a client through props or context.
//
// `main.tsx` imports this same instance and wires it into
// QueryClientProvider; every consumer (hooks, services) calls
// `useQueryClient()` from a component or imports this singleton
// directly when there's no React tree to hang a hook off.
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      refetchOnWindowFocus: false,
      retry: 1,
    },
  },
});