/// <reference types="vite/client" />

import { trace, propagation, context, type Tracer } from '@opentelemetry/api';
import type { AxiosHeaders } from 'axios';
import {
  WebTracerProvider,
  BatchSpanProcessor,
} from '@opentelemetry/sdk-trace-web';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { Resource } from '@opentelemetry/resources';
import { SEMRESATTRS_SERVICE_NAME } from '@opentelemetry/semantic-conventions';
import { W3CTraceContextPropagator } from '@opentelemetry/core';
import { registerInstrumentations } from '@opentelemetry/instrumentation';
import { FetchInstrumentation } from '@opentelemetry/instrumentation-fetch';
import { XMLHttpRequestInstrumentation } from '@opentelemetry/instrumentation-xml-http-request';

let initialized = false;

/**
 * Initialize OpenTelemetry tracing for the browser. Reads
 * `VITE_OTEL_ENABLED` at build time (Vite inlines `import.meta.env`
 * values into the bundle); when unset or anything other than the
 * literal string `"true"`, this is a noop so the bundle pays no
 * runtime cost.
 *
 * Returns `true` only when OTel was actually wired up — callers can
 * use that to gate `injectTraceparent` calls (no need to read the
 * active span if the global provider is still the noop tracer).
 */
export function initTelemetry(): boolean {
  if (initialized) return true;
  if (import.meta.env.VITE_OTEL_ENABLED !== 'true') {
    return false;
  }

  try {
    const serviceName =
      import.meta.env.VITE_OTEL_SERVICE_NAME || 'nyx-frontend';

    // The OTLP exporter's URL validator calls `new URL(...)` — in the
    // browser that resolves a relative path against `location.origin`
    // (so nginx can proxy same-origin), but in Node (tests) the URL
    // constructor needs an absolute base. Resolve to an absolute URL
    // when we have one, otherwise pass the relative path and let the
    // validator throw (caught below — telemetry is a noop then).
    const exporterUrl =
      typeof window !== 'undefined' && window.location?.origin
        ? new window.URL('/otlp/v1/traces', window.location.origin).toString()
        : '/otlp/v1/traces';

    const provider = new WebTracerProvider({
      resource: new Resource({
        [SEMRESATTRS_SERVICE_NAME]: serviceName,
      }),
      spanProcessors: [
        new BatchSpanProcessor(new OTLPTraceExporter({ url: exporterUrl })),
      ],
    });

    trace.setGlobalTracerProvider(provider);
    propagation.setGlobalPropagator(new W3CTraceContextPropagator());

    // For browser we register instrumentations via the dedicated
    // helper — `WebTracerProvider.register({instrumentations: ...})`
    // is a NodeSDK-shaped API and the type doesn't accept it.
    registerInstrumentations({
      instrumentations: [
        new FetchInstrumentation(),
        new XMLHttpRequestInstrumentation(),
      ],
    });

    initialized = true;
    return true;
  } catch (err) {
    // Never let telemetry kill the app — degrade to noop.
    console.warn('Telemetry init failed:', err);
    return false;
  }
}

/**
 * Stamp the W3C `traceparent` header on an outgoing carrier so the
 * server can continue the trace. The `headers` arg may be either a
 * plain object or an axios `AxiosHeaders` (which is a Map-like wrapper
 * with `set`/`get`/index-signature access). We treat it as a plain
 * map and write the property the same way — both shapes accept that.
 *
 * Silently does nothing if there's no active span (e.g. telemetry
 * disabled, or called from outside a traced operation).
 */
export function injectTraceparent(
  headers: Record<string, any> | AxiosHeaders,
): void {
  const activeSpan = trace.getActiveSpan();
  if (!activeSpan) return;
  const carrier = headers as Record<string, any>;
  propagation.inject(context.active(), carrier);
}

export const tracer: Tracer = trace.getTracer('nyx-frontend');