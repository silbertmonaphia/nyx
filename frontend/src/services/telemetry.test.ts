import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { trace } from '@opentelemetry/api';

// `vi.resetModules()` in beforeEach ensures each test gets a fresh
// `telemetry` module so we can re-evaluate the `initialized` flag
// against a freshly stubbed `import.meta.env`.
describe('telemetry', () => {
  beforeEach(() => {
    vi.resetModules();
    vi.unstubAllEnvs();
  });

  afterEach(() => {
    vi.unstubAllEnvs();
    vi.restoreAllMocks();
  });

  it('initTelemetry returns false when VITE_OTEL_ENABLED is unset', async () => {
    const { initTelemetry } = await import('./telemetry');
    expect(initTelemetry()).toBe(false);
  });

  it('initTelemetry returns false when VITE_OTEL_ENABLED is "false"', async () => {
    vi.stubEnv('VITE_OTEL_ENABLED', 'false');
    const { initTelemetry } = await import('./telemetry');
    expect(initTelemetry()).toBe(false);
  });

  it('initTelemetry does not throw when OTel setup is invoked', async () => {
    // The OTLP exporter URL validator may reject relative paths in
    // Node test envs; initTelemetry catches that and degrades to
    // noop. The contract: never throws, always returns a boolean.
    vi.stubEnv('VITE_OTEL_ENABLED', 'true');
    const { initTelemetry } = await import('./telemetry');
    expect(() => initTelemetry()).not.toThrow();
  });

  it('initTelemetry is idempotent (never throws on repeat calls)', async () => {
    vi.stubEnv('VITE_OTEL_ENABLED', 'true');
    const { initTelemetry } = await import('./telemetry');
    expect(() => initTelemetry()).not.toThrow();
    expect(() => initTelemetry()).not.toThrow();
  });

  it('after a real provider is installed, getActiveSpan returns non-null inside startActiveSpan', async () => {
    // This exercises the SDK plumbing (provider + propagator + context
    // manager) the way initTelemetry would — without going through the
    // OTLP exporter (which can't be constructed in Node tests because
    // its URL validator rejects relative URLs without a base).
    const { WebTracerProvider } = await import('@opentelemetry/sdk-trace-web');
    const provider = new WebTracerProvider();
    provider.register();
    trace.setGlobalTracerProvider(provider);

    const tracer = trace.getTracer('test');
    let captured: unknown = null;
    tracer.startActiveSpan('s', (span) => {
      captured = trace.getActiveSpan();
      span.end();
    });
    expect(captured).not.toBeNull();
  });

  it('injectTraceparent is a noop when there is no active span', async () => {
    const { injectTraceparent } = await import('./telemetry');
    const headers: Record<string, unknown> = { foo: 'bar' };
    expect(() => injectTraceparent(headers)).not.toThrow();
    expect(headers).toEqual({ foo: 'bar' });
  });
});