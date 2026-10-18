import { describe, it, expect, vi, beforeEach } from 'vitest';
import { chatService } from './chatService';
import { ChatApiError, ChatStreamError } from '../types/chat';
import api, { tokenStore, refreshTokensAndReplay } from '~/services/api';
import * as telemetry from '~/services/telemetry';

// Captured producer fixture (openspec/changes/agent-feed-tools §6.1):
// the exact bytes the Python emitter produces for a `confirm_request`
// frame — GENERATED and byte-pinned by
// backend/agent_py/tests/test_confirm_frame_wire.py, which encodes the
// frame through the real `main._sse_encode` and asserts this file is
// byte-identical (design.md D8 — a producer-side rename or type change
// fails that test before it can silently degrade the UI to "no card").
// Regenerate it from that test with
// PYTHON_AGENT_UPDATE_WIRE_FIXTURES=1; the expected literals below move
// with it.
//
// Imported through Vite's `?raw` (rather than readFileSync): the file
// has to resolve the same way whether vitest runs from `frontend/`
// (`npm test`) or from the repo root (the pre-commit hook runs
// `vitest --root frontend … related`, which leaves process.cwd() at the
// repo root). `?raw` is a module resolution, so it is cwd-independent —
// `import.meta.url` is not an option (jsdom rewrites it to an `http://`
// URL, which `fileURLToPath` rejects).
import CONFIRM_FRAME from './__fixtures__/confirm_request.sse?raw';

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

  it('parses event:note frames and yields ChatNote events between deltas', async () => {
    const sse =
      'event: delta\ndata: {"delta":"Hel"}\n\n' +
      'event: note\ndata: {"text":"[continued on OpenAI]","provider":"openai"}\n\n' +
      'event: delta\ndata: {"delta":"lo"}\n\n' +
      'event: done\ndata: {"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}\n\n' +
      'data: [DONE]\n\n';
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
      { kind: 'note', text: '[continued on OpenAI]', provider: 'openai' },
      { kind: 'delta', delta: 'lo' },
      {
        kind: 'done',
        usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
      },
      { kind: 'terminator' },
    ]);
  });

  it('drops a malformed event:note frame without stopping the loop', async () => {
    const sse =
      'event: delta\ndata: {"delta":"Hel"}\n\n' +
      'event: note\ndata: not-json\n\n' +
      'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    // The bad note frame is silently dropped; the surrounding delta
    // + done flow through unchanged.
    expect(events).toEqual([
      { kind: 'delta', delta: 'Hel' },
      {
        kind: 'done',
        usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
      },
      { kind: 'terminator' },
    ]);
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

  it('omits the rag field from the request body', async () => {
    const sse =
      'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      void e;
    }

    const body = JSON.parse(fetchMock.mock.calls[0][1].body as string);
    expect(body).toEqual({ messages: [{ role: 'user', content: 'hi' }] });
  });

  // ---- Agent-loop SSE kinds (openspec/changes/agent-loop) ----
  // The new frames are additive on the same /api/chat stream. Each
  // variant below verifies the parser reconstructs the wire shape
  // byte-for-byte; old single-turn events (delta / note / error /
  // done / terminator) still parse identically.

  it('parses a plan event with mixed step kinds', async () => {
    const sse =
      'event: plan\ndata: {"steps":[{"id":"step_1","kind":"rag","query":"hi"},{"id":"step_2","kind":"tool","tool":"list_feeds","args":{"limit":10}},{"id":"step_3","kind":"llm","prompt":"answer"},{"id":"step_4","kind":"react","tool":"get_feed","args":{"id":"x"},"max_iterations":3}]}\n\n' +
      'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events[0]).toEqual({
      kind: 'plan',
      steps: [
        { id: 'step_1', kind: 'rag', query: 'hi', tool: undefined, args: undefined, prompt: undefined, max_iterations: undefined },
        { id: 'step_2', kind: 'tool', tool: 'list_feeds', args: { limit: 10 }, query: undefined, prompt: undefined, max_iterations: undefined },
        { id: 'step_3', kind: 'llm', prompt: 'answer', tool: undefined, args: undefined, query: undefined, max_iterations: undefined },
        { id: 'step_4', kind: 'react', tool: 'get_feed', args: { id: 'x' }, max_iterations: 3, query: undefined, prompt: undefined },
      ],
    });
    expect(events.at(-1)).toEqual({ kind: 'terminator' });
  });

  it('parses step_start / tool_call / tool_result / step_end in sequence', async () => {
    const sse =
      'event: plan\ndata: {"steps":[{"id":"step_1","kind":"tool","tool":"list_feeds","args":{"limit":10}}]}\n\n' +
      'event: step_start\ndata: {"step_id":"step_1","kind":"tool"}\n\n' +
      'event: tool_call\ndata: {"step_id":"step_1","call_id":"c1","name":"list_feeds","args":{"limit":10}}\n\n' +
      'event: tool_result\ndata: {"step_id":"step_1","call_id":"c1","ok":true,"result":[{"id":1}]}\n\n' +
      'event: step_end\ndata: {"step_id":"step_1","status":"ok","reason":""}\n\n' +
      'event: delta\ndata: {"delta":"done"}\n\n' +
      'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events).toEqual([
      {
        kind: 'plan',
        steps: [
          { id: 'step_1', kind: 'tool', tool: 'list_feeds', args: { limit: 10 }, query: undefined, prompt: undefined, max_iterations: undefined },
        ],
      },
      { kind: 'step_start', step_id: 'step_1', step_kind: 'tool' },
      {
        kind: 'tool_call',
        step_id: 'step_1',
        call_id: 'c1',
        name: 'list_feeds',
        args: { limit: 10 },
      },
      {
        kind: 'tool_result',
        step_id: 'step_1',
        call_id: 'c1',
        ok: true,
        result: [{ id: 1 }],
        error: undefined,
      },
      { kind: 'step_end', step_id: 'step_1', status: 'ok', reason: '' },
      { kind: 'delta', delta: 'done' },
      {
        kind: 'done',
        usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
      },
      { kind: 'terminator' },
    ]);
  });

  it('parses a failed tool_result frame with the safe error marker', async () => {
    const sse =
      'event: tool_result\ndata: {"step_id":"step_1","call_id":"c1","ok":false,"error":"tool_call_failed"}\n\n' +
      'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events[0]).toEqual({
      kind: 'tool_result',
      step_id: 'step_1',
      call_id: 'c1',
      ok: false,
      result: undefined,
      error: 'tool_call_failed',
    });
  });

  it('parses a step_end with status failed and reason', async () => {
    const sse =
      'event: step_end\ndata: {"step_id":"step_2","status":"failed","reason":"react_max_iterations"}\n\n' +
      'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events[0]).toEqual({
      kind: 'step_end',
      step_id: 'step_2',
      status: 'failed',
      reason: 'react_max_iterations',
    });
  });

  it('parses an error frame with the new typed code field', async () => {
    const sse =
      'event: error\ndata: {"error":"plan too large","request_id":"req-1","code":"plan_too_large"}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events).toEqual([
      {
        kind: 'error',
        error: 'plan too large',
        request_id: 'req-1',
        code: 'plan_too_large',
      },
      { kind: 'terminator' },
    ]);
  });

  it('drops malformed plan / step_start / tool_call / tool_result / step_end frames', async () => {
    // Each malformed frame is dropped silently; surrounding well-
    // formed frames still flow through. The stream parser must not
    // tear down the response on a single bad agent frame.
    const sse =
      'event: plan\ndata: not-json\n\n' +
      'event: step_start\ndata: {"step_id":"x"}\n\n' + // missing kind
      'event: tool_call\ndata: {"name":"x"}\n\n' + // missing step_id + call_id
      'event: tool_result\ndata: {"step_id":"x"}\n\n' + // missing call_id
      'event: step_end\ndata: {"step_id":"x","status":"weird"}\n\n' + // bad status
      'event: delta\ndata: {"delta":"alive"}\n\n' +
      'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events).toEqual([
      { kind: 'delta', delta: 'alive' },
      {
        kind: 'done',
        usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
      },
      { kind: 'terminator' },
    ]);
  });

  // --- agent-tool-dispatch-retry wire frames ------------------------
  // The two "frozen fixture" frames below are byte-for-byte the strings
  // asserted by the Python producer tests
  // (backend/agent_py/tests/test_executor.py :: test_tool_result_wire_fixture_retry_count
  //  and :: test_partial_failure_note_wire_fixture). If either runtime
  // changes the wire shape, both sides fail together — the producer and
  // the consumer cannot drift silently.

  it('parses the frozen tool_result retry_count fixture', async () => {
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'event: tool_result\ndata: {"step_id":"step_1","call_id":"c1","ok":true,"result":{"items":[]},"error":null,"retry_count":2}\n\n' +
          'event: done\ndata: {"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}\n\n' +
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

    expect(events[0]).toEqual({
      kind: 'tool_result',
      step_id: 'step_1',
      call_id: 'c1',
      ok: true,
      result: { items: [] },
      error: undefined,
      retryCount: 2,
    });
  });

  it('parses the frozen partial_failure note fixture', async () => {
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'event: note\ndata: {"text":"Run diverged from the plan","provider":null,"kind":"partial_failure","last_step_id":null,"run_id":null,"failed_steps":["step_3","step_5"]}\n\n' +
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

    expect(events[0]).toEqual({
      kind: 'partial_failure',
      failedSteps: ['step_3', 'step_5'],
    });
  });

  it('drops malformed partial_failure frames without stopping the loop', async () => {
    const sse =
      'event: delta\ndata: {"delta":"ok"}\n\n' +
      // failed_steps missing
      'event: note\ndata: {"text":"x","provider":null,"kind":"partial_failure"}\n\n' +
      // failed_steps contains a non-string
      'event: note\ndata: {"kind":"partial_failure","failed_steps":["a",5]}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events).toEqual([
      { kind: 'delta', delta: 'ok' },
      { kind: 'terminator' },
    ]);
  });

  it('leaves retryCount undefined when the server frame predates the field', async () => {
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'event: tool_result\ndata: {"step_id":"step_1","call_id":"c1","ok":false,"error":"tool_call_failed"}\n\n' +
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

    expect(events[0]).toEqual({
      kind: 'tool_result',
      step_id: 'step_1',
      call_id: 'c1',
      ok: false,
      result: undefined,
      error: 'tool_call_failed',
      retryCount: undefined,
    });
  });

  // --- session id echo (agent-chat-history-ui) ----------------------

  it('omits run_id from the request body when no session is active', async () => {
    fetchMock.mockResolvedValueOnce(sseResponse(['data: [DONE]\n\n']));

    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      void e;
    }

    const body = JSON.parse(fetchMock.mock.calls[0][1].body as string);
    expect(body).toEqual({ messages: [{ role: 'user', content: 'hi' }] });
    expect('run_id' in body).toBe(false);
  });

  it('echoes the active session id as run_id in the request body', async () => {
    fetchMock.mockResolvedValueOnce(sseResponse(['data: [DONE]\n\n']));

    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
      { runId: 'sess-123' },
    )) {
      void e;
    }

    const body = JSON.parse(fetchMock.mock.calls[0][1].body as string);
    expect(body).toEqual({
      messages: [{ role: 'user', content: 'hi' }],
      run_id: 'sess-123',
    });
  });

  it('surfaces the first-frame run_id through onRunId without yielding it', async () => {
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'event: run_id\ndata: {"run_id":"srv-minted-1"}\n\n' +
          'event: delta\ndata: {"delta":"hi"}\n\n' +
          'data: [DONE]\n\n',
      ]),
    );
    const onRunId = vi.fn();

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
      { onRunId },
    )) {
      events.push(e);
    }

    expect(onRunId).toHaveBeenCalledTimes(1);
    expect(onRunId).toHaveBeenCalledWith('srv-minted-1');
    // The frame is state, not content: the event union is unchanged.
    expect(events).toEqual([
      { kind: 'delta', delta: 'hi' },
      { kind: 'terminator' },
    ]);
  });

  it('drops a malformed run_id frame without stopping the stream', async () => {
    fetchMock.mockResolvedValueOnce(
      sseResponse([
        'event: run_id\ndata: not-json\n\n' +
          'event: run_id\ndata: {"run_id":42}\n\n' +
          'event: delta\ndata: {"delta":"alive"}\n\n' +
          'data: [DONE]\n\n',
      ]),
    );
    const onRunId = vi.fn();

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
      { onRunId },
    )) {
      events.push(e);
    }

    expect(onRunId).not.toHaveBeenCalled();
    expect(events).toEqual([
      { kind: 'delta', delta: 'alive' },
      { kind: 'terminator' },
    ]);
  });

  // --- agent-feed-tools: confirm_request frame (task 6.1) -----------

  it('parses the captured confirm_request fixture inside the paused step', async () => {
    // Frame order per the spec: step_start, then confirm_request, and
    // only after the decision the tool_call / tool_result pair, then
    // step_end. Every pre-change frame keeps its position + shape.
    expect(CONFIRM_FRAME.endsWith('\n\n')).toBe(true);
    const sse =
      'event: plan\ndata: {"steps":[{"id":"step_2","kind":"tool","tool":"create_feed","args":{"title":"Rust Blog","url":"https://blog.rust-lang.org/feed.xml"}}]}\n\n' +
      'event: step_start\ndata: {"step_id":"step_2","kind":"tool"}\n\n' +
      CONFIRM_FRAME +
      'event: tool_call\ndata: {"step_id":"step_2","call_id":"c1","name":"create_feed","args":{"title":"Rust Blog","url":"https://blog.rust-lang.org/feed.xml"}}\n\n' +
      'event: tool_result\ndata: {"step_id":"step_2","call_id":"c1","ok":true,"result":{"id":7}}\n\n' +
      'event: step_end\ndata: {"step_id":"step_2","status":"ok","reason":""}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'add the rust blog' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    // The literal expected values are the contract; the fixture holds
    // the producer bytes. Changing one without the other fails here.
    expect(events).toEqual([
      {
        kind: 'plan',
        steps: [
          {
            id: 'step_2',
            kind: 'tool',
            tool: 'create_feed',
            args: { title: 'Rust Blog', url: 'https://blog.rust-lang.org/feed.xml' },
            query: undefined,
            prompt: undefined,
            max_iterations: undefined,
          },
        ],
      },
      { kind: 'step_start', step_id: 'step_2', step_kind: 'tool' },
      {
        kind: 'confirm_request',
        run_id: '2f6c1a94-8d3e-4b57-9c02-7e5a1d8b3f60',
        step_id: 'step_2',
        tool: 'create_feed',
        args: { title: 'Rust Blog', url: 'https://blog.rust-lang.org/feed.xml' },
        approval_id: 'ap_9c4d2f80b1e34a7db6f5c3a2e10d7b45',
        expires_at: 1791979500.0,
      },
      {
        kind: 'tool_call',
        step_id: 'step_2',
        call_id: 'c1',
        name: 'create_feed',
        args: { title: 'Rust Blog', url: 'https://blog.rust-lang.org/feed.xml' },
      },
      {
        kind: 'tool_result',
        step_id: 'step_2',
        call_id: 'c1',
        ok: true,
        result: { id: 7 },
        error: undefined,
      },
      { kind: 'step_end', step_id: 'step_2', status: 'ok', reason: '' },
      { kind: 'terminator' },
    ]);
  });

  it('drops malformed confirm_request frames without stopping the loop', async () => {
    const sse =
      // missing approval_id
      'event: confirm_request\ndata: {"run_id":"r1","step_id":"step_2","tool":"create_feed","args":{},"expires_at":1791979500.0}\n\n' +
      // empty step_id
      'event: confirm_request\ndata: {"run_id":"r1","step_id":"","tool":"create_feed","args":{},"approval_id":"ap-1","expires_at":1791979500.0}\n\n' +
      // expires_at as an ISO string — the producer emits epoch seconds
      'event: confirm_request\ndata: {"run_id":"r1","step_id":"step_2","tool":"create_feed","args":{},"approval_id":"ap-1","expires_at":"2026-10-14T12:05:00Z"}\n\n' +
      // not JSON at all
      'event: confirm_request\ndata: not-json\n\n' +
      'event: delta\ndata: {"delta":"alive"}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events).toEqual([
      { kind: 'delta', delta: 'alive' },
      { kind: 'terminator' },
    ]);
  });

  it('skips unknown event kinds so pre-change parsers keep every frame in place', async () => {
    // Spec scenario "Clients that ignore the new kind are unaffected":
    // the parser branches on known kinds only, so an unknown kind
    // (yesterday: confirm_request on an old bundle; today: whatever
    // comes next) is skipped without disturbing its neighbours.
    const sse =
      'event: step_start\ndata: {"step_id":"s1","kind":"llm"}\n\n' +
      'event: some_future_kind\ndata: {"whatever":true}\n\n' +
      'event: delta\ndata: {"delta":"hi"}\n\n' +
      'data: [DONE]\n\n';
    fetchMock.mockResolvedValueOnce(sseResponse([sse]));

    const events: unknown[] = [];
    for await (const e of chatService.streamMessage(
      [{ role: 'user', content: 'hi' }],
      new AbortController().signal,
    )) {
      events.push(e);
    }

    expect(events).toEqual([
      { kind: 'step_start', step_id: 's1', step_kind: 'llm' },
      { kind: 'delta', delta: 'hi' },
      { kind: 'terminator' },
    ]);
  });
});

// ---- Approval decisions (openspec change agent-feed-tools §6.2) -----
// The decision rides on the wrapped axios client, so the JWT request
// interceptor + single-flight 401 refresh + global error toast all
// apply. The store owns the card's pending/decided state.

describe('chatService.submitApproval', () => {
  it('POSTs the decision body to /chat/approvals through the axios client', async () => {
    const postSpy = vi
      .spyOn(api, 'post')
      .mockResolvedValueOnce({ status: 200 } as never);

    await chatService.submitApproval({
      run_id: 'run-1',
      approval_id: 'ap-1',
      decision: 'approve',
    });

    expect(postSpy).toHaveBeenCalledWith('/chat/approvals', {
      run_id: 'run-1',
      approval_id: 'ap-1',
      decision: 'approve',
    });
    postSpy.mockRestore();
  });

  it('rejects when the proxy rejects the decision', async () => {
    const postSpy = vi
      .spyOn(api, 'post')
      .mockRejectedValueOnce(new Error('denied'));

    await expect(
      chatService.submitApproval({
        run_id: 'run-1',
        approval_id: 'ap-1',
        decision: 'deny',
      }),
    ).rejects.toThrow('denied');
    postSpy.mockRestore();
  });
});

// ---- Session REST surface (openspec/changes/agent-chat-history-ui) ----
// Raw-fetch JSON endpoints behind the Go chat proxy: list / replay /
// create / resume / delete. Errors must carry the response status so
// the store can distinguish 404 (session gone) from a transient
// failure.

describe('chatService session endpoints', () => {
  beforeEach(() => {
    fetchMock.mockReset();
    mockedRefresh.mockReset();
    tokenStore.clear();
  });

  it('lists sessions, mapping the wire rows and cursor', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(200, {
        sessions: [
          {
            id: 's2',
            user_id: 7,
            title: 'Second',
            started_at: '2026-10-14T10:00:00Z',
            last_active_at: '2026-10-14T11:00:00Z',
            ended_at: '2026-10-14T11:05:00Z',
            message_count: 4,
          },
          {
            id: 's1',
            user_id: 7,
            title: '',
            started_at: '2026-10-14T09:00:00Z',
            last_active_at: '2026-10-14T09:30:00Z',
            ended_at: null,
            message_count: 2,
          },
          // No id — dropped rather than rendered as a broken row.
          { title: 'malformed' },
        ],
        next_cursor: 'cursor-abc',
      }),
    );

    const page = await chatService.listSessions();

    // First page carries no params — the server default page size
    // (`PYTHON_AGENT_SESSIONS_PAGE_SIZE`) stays authoritative.
    expect(fetchMock.mock.calls[0][0]).toMatch(/\/chat\/sessions$/);
    expect(page.sessions.map((s) => s.id)).toEqual(['s2', 's1']);
    expect(page.sessions[1]).toEqual({
      id: 's1',
      user_id: 7,
      title: '',
      started_at: '2026-10-14T09:00:00Z',
      last_active_at: '2026-10-14T09:30:00Z',
      ended_at: null,
      message_count: 2,
    });
    expect(page.nextCursor).toBe('cursor-abc');
  });

  it('passes limit + before as query params and normalises a missing cursor', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(200, { sessions: [] }));

    const page = await chatService.listSessions({ limit: 25, before: 'cur/1+x' });

    const url = fetchMock.mock.calls[0][0] as string;
    expect(url).toContain('limit=25');
    // The cursor is opaque but URL-unsafe characters must be encoded.
    expect(url).toContain('before=cur%2F1%2Bx');
    expect(page.nextCursor).toBeNull();
    expect(page.sessions).toEqual([]);
  });

  it('throws ChatApiError carrying the status on a non-2xx list', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(500, { error: 'boom' }));

    const err = await chatService
      .listSessions()
      .then(() => null)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ChatApiError);
    expect((err as ChatApiError).status).toBe(500);
    // The backend's message is never echoed into the client error.
    expect((err as ChatApiError).message).not.toContain('boom');
  });

  it('replays a session’s messages in wire order, keeping tool_calls opaque', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(200, {
        messages: [
          {
            id: 1,
            role: 'user',
            content: 'hello',
            tool_calls: null,
            tokens: 0,
            created_at: '2026-10-14T10:00:00Z',
          },
          {
            id: 2,
            role: 'assistant',
            content: 'hi',
            tool_calls: [
              { step_id: 's1', call_id: 'c1', name: 'list_feeds', args: {} },
            ],
            tokens: 12,
            prompt_tokens: 9,
            completion_tokens: 3,
            created_at: '2026-10-14T10:00:01Z',
          },
          // Dropped: no content.
          { id: 3, role: 'assistant', tool_calls: null },
        ],
      }),
    );

    const messages = await chatService.getMessages('sess-1');

    expect(fetchMock.mock.calls[0][0]).toMatch(/\/chat\/sessions\/sess-1\/messages$/);
    expect(messages.map((m) => m.id)).toEqual([1, 2]);
    expect(messages[1].tool_calls).toEqual([
      { step_id: 's1', call_id: 'c1', name: 'list_feeds', args: {} },
    ]);
    expect(messages[0].tool_calls).toBeNull();
    expect(messages[1].tokens).toBe(12);
    expect(messages[1].prompt_tokens).toBe(9);
    expect(messages[1].completion_tokens).toBe(3);
    expect(messages[0].prompt_tokens).toBeNull();
    expect(messages[0].completion_tokens).toBeNull();
  });

  it('encodes the session id in the replay URL', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(200, { messages: [] }));

    await chatService.getMessages('a/b c');

    expect(fetchMock.mock.calls[0][0]).toContain(
      '/chat/sessions/a%2Fb%20c/messages',
    );
  });

  it('surfaces a 404 replay as ChatApiError(404)', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(404, { error: 'not found' }));

    const err = await chatService
      .getMessages('gone')
      .then(() => null)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ChatApiError);
    expect((err as ChatApiError).status).toBe(404);
  });

  it('creates a session and returns its id', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(201, {
        session_id: 'srv-1',
        started_at: '2026-10-14T12:00:00Z',
      }),
    );

    const created = await chatService.createSession();

    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect(fetchMock.mock.calls[0][0]).toMatch(/\/chat\/sessions$/);
    expect(init.method).toBe('POST');
    expect(created).toEqual({
      session_id: 'srv-1',
      started_at: '2026-10-14T12:00:00Z',
    });
  });

  it('throws when create answers without a session id', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(200, { started_at: 'x' }));

    const err = await chatService
      .createSession()
      .then(() => null)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ChatApiError);
  });

  it('resumes a session and resolves on an empty 200', async () => {
    fetchMock.mockResolvedValueOnce(new Response('', { status: 200 }));

    await expect(chatService.resumeSession('sess-1')).resolves.toBeUndefined();
    expect(fetchMock.mock.calls[0][0]).toContain('/chat/sessions/sess-1/resume');
    expect((fetchMock.mock.calls[0][1] as RequestInit).method).toBe('POST');
  });

  it('deletes a session and tolerates a 204 with no body', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));

    await expect(chatService.deleteSession('sess-1')).resolves.toBeUndefined();
    expect(fetchMock.mock.calls[0][0]).toMatch(/\/chat\/sessions\/sess-1$/);
    expect((fetchMock.mock.calls[0][1] as RequestInit).method).toBe('DELETE');
  });

  it('runs the shared single-flight refresh once and replays a 401', async () => {
    tokenStore.setTokens('expired.access', 'good.refresh');
    mockedRefresh.mockResolvedValueOnce(true);
    fetchMock
      .mockResolvedValueOnce(jsonResponse(401, { error: 'expired' }))
      .mockResolvedValueOnce(jsonResponse(200, { sessions: [], next_cursor: null }));

    await chatService.listSessions();

    expect(mockedRefresh).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const secondHeaders = (fetchMock.mock.calls[1][1] as RequestInit)
      .headers as Record<string, string>;
    expect(secondHeaders.Authorization).toBe('Bearer expired.access');
    expect(secondHeaders.Accept).toBe('application/json');

    tokenStore.clear();
  });

  it('does not attempt a refresh without an access token', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(401, { error: 'unauthorized' }));

    const err = await chatService
      .listSessions()
      .then(() => null)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ChatApiError);
    expect((err as ChatApiError).status).toBe(401);
    expect(mockedRefresh).not.toHaveBeenCalled();
  });
});
