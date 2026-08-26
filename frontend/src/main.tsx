import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ReactQueryDevtools } from '@tanstack/react-query-devtools'
import './index.css'
import { initTelemetry } from './services/telemetry'
import { logger } from './services/logger'
import { ErrorBoundary } from './components/app/ErrorBoundary'
import App from './App.tsx'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      refetchOnWindowFocus: false,
      retry: 1,
    },
  },
})

initTelemetry()

// Catch uncaught errors so they show up in our log pipeline alongside
// any active trace context. Registered before mount() so we capture
// failures during initial render too.
window.addEventListener('error', (e) =>
  logger.error('window.error', {
    message: e.message,
    filename: e.filename,
    lineno: e.lineno,
  }),
)
window.addEventListener('unhandledrejection', (e) =>
  logger.error('unhandledrejection', { reason: String(e.reason) }),
)

function mount() {
  const rootElement = document.getElementById('root');
  if (!rootElement) throw new Error('Failed to find the root element');

  // ErrorBoundary wraps the whole tree so a render-time throw in
  // any descendant gets a fallback UI + a structured log line
  // instead of an unmounted white screen. SECURITY.md L6.
  createRoot(rootElement).render(
    <StrictMode>
      <ErrorBoundary>
        <QueryClientProvider client={queryClient}>
          <App />
          {import.meta.env.DEV ? <ReactQueryDevtools initialIsOpen={false} /> : null}
        </QueryClientProvider>
      </ErrorBoundary>
    </StrictMode>,
  )
}

mount()