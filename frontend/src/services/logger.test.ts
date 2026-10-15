import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { trace } from '@opentelemetry/api';
import { WebTracerProvider } from '@opentelemetry/sdk-trace-web';
import { logger } from './logger';

describe('logger', () => {
  let infoSpy: ReturnType<typeof vi.spyOn>;
  let warnSpy: ReturnType<typeof vi.spyOn>;
  let errorSpy: ReturnType<typeof vi.spyOn>;
  let debugSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    infoSpy = vi.spyOn(console, 'info').mockImplementation(() => {});
    warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    debugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});
  });

  afterEach(() => {
    infoSpy.mockRestore();
    warnSpy.mockRestore();
    errorSpy.mockRestore();
    debugSpy.mockRestore();
  });

  it('writes one JSON line per call, parseable + carrying attrs', () => {
    logger.info('hi', { a: 1 });

    expect(infoSpy).toHaveBeenCalledTimes(1);
    const line = infoSpy.mock.calls[0][0] as string;
    expect(typeof line).toBe('string');
    const parsed = JSON.parse(line);
    expect(parsed.level).toBe('info');
    expect(parsed.msg).toBe('hi');
    expect(parsed.a).toBe(1);
    expect(parsed.ts).toMatch(
      /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/,
    );
  });

  it('routes error level to console.error', () => {
    logger.error('boom');
    expect(errorSpy).toHaveBeenCalledTimes(1);
    const parsed = JSON.parse(errorSpy.mock.calls[0][0] as string);
    expect(parsed.level).toBe('error');
    expect(parsed.msg).toBe('boom');
  });

  it('routes warn level to console.warn', () => {
    logger.warn('careful');
    expect(warnSpy).toHaveBeenCalledTimes(1);
    const parsed = JSON.parse(warnSpy.mock.calls[0][0] as string);
    expect(parsed.level).toBe('warn');
  });

  it('routes debug level to console.debug', () => {
    logger.debug('trace');
    expect(debugSpy).toHaveBeenCalledTimes(1);
    const parsed = JSON.parse(debugSpy.mock.calls[0][0] as string);
    expect(parsed.level).toBe('debug');
  });

  it('omits trace_id/span_id when there is no active span', () => {
    logger.info('no-span');
    const parsed = JSON.parse(infoSpy.mock.calls[0][0] as string);
    expect(parsed.trace_id).toBeUndefined();
    expect(parsed.span_id).toBeUndefined();
  });

  it('includes trace_id and span_id when a span is active', () => {
    // WebTracerProvider.register() installs a stack context manager so
    // `startActiveSpan` actually creates a current span that
    // `getActiveSpan()` can see.
    const provider = new WebTracerProvider();
    provider.register();
    trace.setGlobalTracerProvider(provider);
    const tracer = trace.getTracer('test');

    tracer.startActiveSpan('s', (span) => {
      logger.info('inside');
      const parsed = JSON.parse(infoSpy.mock.calls[0][0] as string);
      expect(parsed.trace_id).toMatch(/^[0-9a-f]+$/);
      expect(parsed.trace_id.length).toBe(32);
      expect(parsed.span_id).toMatch(/^[0-9a-f]+$/);
      expect(parsed.span_id.length).toBe(16);
      span.end();
    });
  });
});