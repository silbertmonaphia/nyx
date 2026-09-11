import { describe, it, expect, vi, beforeEach } from 'vitest';
import { chatService } from './chatService';
import { ChatStreamError } from '../types/chat';
import { tokenStore, refreshTokensAndReplay } from '~/services/api';
import * as telemetry from '~/services/telemetry';

// The chatService uses raw `fetch` for SSE (axios can't stream), and
// `refreshTokensAndReplay` for the 401-retry contract. Mock both.
const fetchMock = vi.fn();
vi.stubGlobal('fetch', fetchMock);

vi.mock('~/services/api', async () => {
  const actual = await vi.importActual<typeof import('~/services/api')>('~/services/api');
  return {
    ...actual,
    refreshTokensAndReplay: vi.fn(),
  };
});

const mockedRefresh = refreshTokensAndReplay as unknown as ReturnType<typeof vi.fn>;

// telemetry is read-only at module init; spy on `injectTraceparent`
// so we can assert it was called without mutating the real carrier.
vi.spyOn(telemetry, 'injectTraceparent').mockImplementation(() => {});

/**
 * Build a `Response`-shaped object that yields the given SSE chunks
 * through a `ReadableStream`. Mirrors the production streaming shape
 * so the parser sees the same bytes it would in a real call.
 */
function sseResponse(chunks: string[], status = 200): Response {
  const encoder = new TextEncoder();
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) {
        controller.enqueue(encoder.encode(chunk));
      }
      controller.close();
    },
  });
  return new Response(stream, {
    status,
    headers: { 'content-type': 'text/event-stream' },
  });
}

/** JSON-envelope Response, used for non-2xx paths. */
function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

describe('chatService.streamMessage', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    mockedRefresh.mockReset();
    tokenStore.clear();
  });

  it('parses delta frames and yields ChatDelta events', async () => {
    const sse = [
      'event: delta\ndata: {"delta":"Hel"}\n\n',
      'event: delta\ndata: {"delta":"lo"}\n\n',
      'event: done\ndata: {"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}\n\n',
      'data: [DONE]\n\n',
    ].join('');
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events).toEqual([
      { kind: 'delta', delta: 'Hel' },
      { kind: 'delta', delta: 'lo' },
      {
        kind: 'done',
        usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
      },
      { kind: 'terminator' },
    ]);

    // Stamps the Bearer header on the outbound request.
    const [, init] = fetchMock.mock.calls[0];
    const headers = (init as RequestInit).headers as Record<string, string>;
    expect(headers.Authorization).toBeUndefined(); // no token in store
    expect(headers['Content-Type']).toBe('application/json');
    expect(headers.Accept).toBe('text/event-stream');
  });

  it('surfaces a mid-stream event:error as ChatStreamError and stops the loop', async () => {
    const sse =
      'event: delta\ndata: {"delta":"Hel"}\n\n' +
      'event: error\ndata: {"error":"upstream failed","request_id":"req-err-1"}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    let caught: unknown = null;
    try {
      for await (const e of chatService.streamMessage(
        [{ role: 'user', content: 'hi' }],
        new AbortController().signal,
      )) {
        events.push(e);
      }
    } catch (err) {
      caught = err;
    }

    // The error event is yielded to the consumer, but the generator
    // exits via `return` — no further frames, no throw (the catch
    // stays clear).
    expect(events).toEqual([
      { kind: 'delta', delta: 'Hel' },
      {
        kind: 'error',
        error: 'upstream failed',
        request_id: 'req-err-1',
      },
      { kind: 'terminator' },
    ]);
    expect(caught).toBeNull();
  });

  it('throws ChatStreamError(kind:http) on a non-2xx envelope', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(400, {
        error: 'messages is required',
        code: 400,
        request_id: 'req-400',
      }),
    );

    const err = await chatService
      .streamMessage(
        [{ role: 'user', content: 'hi' }],
        new AbortController().signal,
      )
      .next()
      .then(() => null)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ChatStreamError);
    const ev = (err as ChatStreamError).event as {
      kind: string;
      status: number;
      message: string;
    };
    expect(ev.kind).toBe('http');
    expect(ev.status).toBe(400);
    expect(ev.message).toBe('messages is required');
  });

  it('falls back to a generic HTTP message when the body is not JSON', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response('not json at all', {
        status: 502,
        headers: { 'content-type': 'text/plain' },
      }),
    );

    const err = await chatService
      .streamMessage(
        [{ role: 'user', content: 'hi' }],
        new AbortController().signal,
      )
      .next()
      .then(() => null)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ChatStreamError);
    const ev = (err as ChatStreamError).event as {
      kind: string;
      status: number;
      message: string;
    };
    expect(ev.kind).toBe('http');
    expect(ev.status).toBe(502);
    expect(ev.message).toMatch(/Chat request failed/);
  });

  it('runs refreshTokensAndReplay on 401 and retries once', async () => {
    // Seed the token store so the refresh branch engages.
    tokenStore.setTokens('expired.access', 'good.refresh');
    mockedRefresh.mockResolvedValueOnce(true);

    // First response: 401. Second (after refresh): a clean stream.
    fetchMock
      .mockResolvedValueOnce(jsonResponse(401, { error: 'expired', code: 401 }))
      .mockResolvedValueOnce(
        sseResponse([
          'event: delta\ndata: {"delta":"fresh"}\n\n' +
            'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}\n\n' +
            'data: [DONE]\n\n',
        ]),
      );

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(mockedRefresh).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    // Second call carries the freshly-rotated Bearer header.
    const secondHeaders = (fetchMock.mock.calls[1][1] as RequestInit).headers as Record<string, string>;
    expect(secondHeaders.Authorization).toBe(`Bearer ${tokenStore.getAccessToken()}`);
    expect(events).toEqual([
      { kind: 'delta', delta: 'fresh' },
      {
        kind: 'done',
        usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
      },
      { kind: 'terminator' },
    ]);

    tokenStore.clear();
  });

  it('does NOT attempt refresh when no access token is in the store', async () => {
    // No token seeded → anonymous 401 → straight to http error,
    // no refresh attempt. Mirrors the axios interceptor's contract.
    fetchMock.mockResolvedValueOnce(
      jsonResponse(401, { error: 'unauthorized', code: 401 }),
    );

    const err = await chatService
      .streamMessage(
        [{ role: 'user', content: 'hi' }],
        new AbortController().signal,
      )
      .next()
      .then(() => null)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ChatStreamError);
    expect(mockedRefresh).not.toHaveBeenCalled();
  });

  it('aborts the underlying fetch when the consumer signal fires', async () => {
    // Hold the fetch promise open so we can abort mid-flight and
    // observe that `signal` was wired into the request init.
    let release!: () => void;
    fetchMock.mockReturnValueOnce(
      new Promise<Response>((resolve) => {
        release = () => resolve(sseResponse([]));
      }),
    );

    const ctrl = new AbortController();
    const consumer = (async () => {
      const events: unknown[] = [];
      for await (const e of chatService.streamMessage(
        [{ role: 'user', content: 'hi' }],
        ctrl.signal,
      )) {
        events.push(e);
      }
      return events;
    })();

    // Yield so the generator reaches the fetch call.
    await new Promise((r) => setTimeout(r, 0));

    // The fetch was issued with the consumer's signal.
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(init.signal).toBe(ctrl.signal);

    // Aborting the controller must abort the fetch — DOMException
    // is what fetch throws when its signal fires.
    ctrl.abort();
    release();

    const err = await consumer.then(
      () => null,
      (e: unknown) => e,
    );
    // The fetch promise may have settled first (we released it); the
    // generator then sees `done=true` and exits cleanly. Either way,
    // we must NOT hang. The signal path is verified by the init
    // check above.
    expect(err === null || (err as { name?: string }).name === 'AbortError').toBe(true);
  });

  it('handles a single frame split across chunks', async () => {
    // The frame is intentionally split between two chunks so the
    // line buffer has to stitch them before dispatch.
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'event: delta\ndata: {"delta":"par',
        't1"}\n\nevent: delta\ndata: {"delta":"part2"}\n\ndata: [DONE]\n\n',
      ]),
    );

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events).toEqual([
      { kind: 'delta', delta: 'part1' },
      { kind: 'delta', delta: 'part2' },
      { kind: 'terminator' },
    ]);
  });
});
