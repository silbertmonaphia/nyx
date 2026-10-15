import { trace } from '@opentelemetry/api';

/**
 * Minimal structured logger. One JSON line per call, written to the
 * matching `console.*` so it shows up in DevTools and in whatever
 * log pipeline scrapes the browser console. Trace context is pulled
 * off the active span so log lines can be correlated with traces.
 *
 * Intentionally tiny — the goal is to stop scattering `console.log`
 * across the app, not to ship a pino replacement.
 */
type Level = 'debug' | 'info' | 'warn' | 'error';

function emit(level: Level, msg: string, attrs?: Record<string, unknown>): void {
  const span = trace.getActiveSpan();
  const spanCtx = span?.spanContext();
  const record: Record<string, unknown> = {
    ts: new Date().toISOString(),
    level,
    msg,
  };
  if (spanCtx && spanCtx.traceId && spanCtx.traceId !== '00000000000000000000000000000000') {
    record.trace_id = spanCtx.traceId;
    record.span_id = spanCtx.spanId;
  }
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      record[k] = typeof v === 'object' && v !== null ? JSON.stringify(v) : v;
    }
  }
  const line = JSON.stringify(record);
  switch (level) {
    case 'debug':
      console.debug(line);
      break;
    case 'info':
      console.info(line);
      break;
    case 'warn':
      console.warn(line);
      break;
    case 'error':
      console.error(line);
      break;
  }
}

export const logger = {
  debug(msg: string, attrs?: Record<string, unknown>): void {
    emit('debug', msg, attrs);
  },
  info(msg: string, attrs?: Record<string, unknown>): void {
    emit('info', msg, attrs);
  },
  warn(msg: string, attrs?: Record<string, unknown>): void {
    emit('warn', msg, attrs);
  },
  error(msg: string, attrs?: Record<string, unknown>): void {
    emit('error', msg, attrs);
  },
};