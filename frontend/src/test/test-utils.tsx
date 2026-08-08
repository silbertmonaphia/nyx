import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';

/**
 * Minimal provider wrapper for component / hook tests that exercise
 * TanStack Query. The shape mirrors `src/main.tsx` but with retries
 * disabled — vitest tests should not retry on failure, since retries
 * mask transient bugs and blow up test runtime.
 */
export function TestProviders({ children }: { children: React.ReactNode }) {
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

/**
 * `renderHook` from `@testing-library/react` re-exported so hook tests
 * can pull both `TestProviders` and `renderHook` from the same module.
 */
export { renderHook };