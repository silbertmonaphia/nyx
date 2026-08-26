import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';

// The structured logger is replaced wholesale — ES-module
// exports are read-only, so `vi.spyOn(logger, 'error')` would
// fail. Mocking the module swaps the binding entirely so the
// boundary's logger.error call lands in a vi.fn() we control.
vi.mock('~/services/logger', () => ({
  logger: {
    debug: vi.fn(),
    info: vi.fn(),
    warn: vi.fn(),
    error: vi.fn(),
  },
}));
import { logger } from '~/services/logger';
import { ErrorBoundary } from './ErrorBoundary';

// Throws on render so the boundary has something to catch.
const Boom: React.FC = () => {
  throw new Error('boom from child');
};

describe('ErrorBoundary', () => {
  let errorSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    // React logs caught errors to console.error in dev; silence
    // them so the test runner output stays clean.
    vi.spyOn(console, 'error').mockImplementation(() => {});
    errorSpy = logger.error as ReturnType<typeof vi.fn>;
    errorSpy.mockClear();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders children when no error is thrown', () => {
    render(
      <ErrorBoundary>
        <div>safe child</div>
      </ErrorBoundary>,
    );
    expect(screen.getByText('safe child')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('renders the default fallback when a descendant throws', () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );
    const alert = screen.getByRole('alert');
    expect(alert).toBeInTheDocument();
    // The fallback deliberately carries no diagnostic detail —
    // SECURITY.md L6 calls this out: a panic UI is not the place
    // to ship PII or internal paths from the render tree.
    expect(alert.textContent).not.toContain('boom from child');
    expect(alert.textContent).not.toContain('Error');
  });

  it('logs the caught error via the structured logger', () => {
    render(
      <ErrorBoundary>
        <Boom />
      </ErrorBoundary>,
    );
    // The full error (message, name, stack) and React's component
    // stack land in the log pipeline — operators diagnose from
    // there, the user only sees the fallback.
    expect(errorSpy).toHaveBeenCalledWith(
      'react.error',
      expect.objectContaining({
        message: 'boom from child',
        name: 'Error',
        stack: expect.any(String),
        componentStack: expect.any(String),
      }),
    );
  });

  it('honors a custom fallback prop', () => {
    render(
      <ErrorBoundary fallback={<div>custom fallback</div>}>
        <Boom />
      </ErrorBoundary>,
    );
    expect(screen.getByText('custom fallback')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
