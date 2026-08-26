import React from 'react';
import { logger } from '~/services/logger';

/**
 * Render-fallback for any uncaught error inside the React tree.
 *
 * SECURITY.md L6. The window `error` / `unhandledrejection`
 * listeners in main.tsx cover errors that escape the React tree
 * (event handlers, async code, top-level throws). This boundary
 * is the in-tree counterpart — it catches errors thrown during
 * render, in lifecycle methods, and in constructors of any
 * descendant component, and converts them into a stable UI
 * instead of an unmounted white screen.
 *
 * We log the full error + component stack via the structured
 * logger so the trace lands in the same log pipeline as every
 * other error in the app. The user-visible fallback deliberately
 * carries no diagnostic detail (no stack, no message text) — the
 * render tree can contain PII or internal paths, and a panic UI
 * is not the place to ship it.
 */
interface Props {
  children: React.ReactNode;
  fallback?: React.ReactNode;
}

interface State {
  hasError: boolean;
}

export class ErrorBoundary extends React.Component<Props, State> {
  state: State = { hasError: false };

  static getDerivedStateFromError(): State {
    // The next render shows the fallback. The actual error object
    // is captured separately in componentDidCatch below — keeping
    // it out of state means we don't accidentally leak it into
    // a future render path.
    return { hasError: true };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo): void {
    logger.error('react.error', {
      message: error.message,
      name: error.name,
      stack: error.stack,
      componentStack: info.componentStack,
    });
  }

  render(): React.ReactNode {
    if (this.state.hasError) {
      return (
        this.props.fallback ?? (
          <div
            role="alert"
            className="min-h-screen flex flex-col items-center justify-center gap-3 p-8 text-center"
          >
            <h1 className="text-2xl font-semibold">Something went wrong</h1>
            <p className="text-muted-foreground max-w-md">
              An unexpected error stopped the page from rendering. Reload to
              try again.
            </p>
          </div>
        )
      );
    }
    return this.props.children;
  }
}
