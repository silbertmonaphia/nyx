import { expect, type Page, type Route } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import http from 'node:http';
import type { AddressInfo } from 'node:net';

/**
 * Feed-tool approval flow (openspec/changes/agent-feed-tools task 7.1).
 *
 * What unit tests can't catch (this file exists to fill the gap):
 *   - the held-stream shape: the chat SSE response stays OPEN while the
 *     user decides, and the continued frames (`tool_call`/`tool_result`/
 *     `delta`/`done`) arrive on the SAME connection after the decision
 *     POST — a static route.fulfill cannot model that pause/resume, so
 *     an in-spec mock SSE server serves the stream and resumes it when
 *     the decision POST lands.
 *   - the wire bytes of the decision POST as the browser sends them.
 *   - the card's real DOM lifecycle across pending → decided states.
 *
 * The flow is driven end-to-end through the browser: real registration
 * + auth against the app, real fetch/stream parsing, real POST. Only
 * the chat stream itself is canned.
 *
 * Assumes the side stack is up for auth:
 *   ./scripts/e2e-side-stack.sh up
 *   PLAYWRIGHT_BASE_URL=http://localhost:5273 \
 *     npx playwright test tests/e2e/chat-feed-approval.spec.ts
 */

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-headers': 'authorization, content-type',
  'access-control-allow-methods': 'GET, POST, OPTIONS',
};

const APPROVAL_ID = 'e2e-approval-1';
const RUN_ID = 'e2e-run-1';
const FEED_NAME = 'E2E Feed';

const DENIAL_TEXT =
  'Confirmation required: the user denied this call. The `create_feed` tool was not executed.';

function frame(kind: string, payload: unknown): string {
  return `event: ${kind}\ndata: ${JSON.stringify(payload)}\n\n`;
}

/** Frames up to (and including) the confirm prompt — the stream pauses here. */
function preamble(expiresInSeconds = 300): string {
  return (
    frame('run_id', { run_id: RUN_ID }) +
    frame('note', { text: 'fresh_run', kind: 'fresh_run', run_id: RUN_ID }) +
    frame('plan', {
      steps: [
        { id: 'step_1', kind: 'tool', tool: 'create_feed', args: { name: FEED_NAME } },
        { id: 'step_2', kind: 'llm', prompt: 'Answer with what happened.' },
      ],
    }) +
    frame('step_start', { step_id: 'step_1', kind: 'tool' }) +
    frame('confirm_request', {
      run_id: RUN_ID,
      step_id: 'step_1',
      tool: 'create_feed',
      args: { name: FEED_NAME },
      approval_id: APPROVAL_ID,
      expires_at: Date.now() / 1000 + expiresInSeconds,
    })
  );
}

function approveContinuation(): string {
  return (
    frame('tool_call', {
      step_id: 'step_1',
      call_id: 'call_1',
      name: 'create_feed',
      args: { name: FEED_NAME },
    }) +
    frame('tool_result', {
      step_id: 'step_1',
      call_id: 'call_1',
      ok: true,
      result: { id: 1, name: FEED_NAME },
      error: null,
      retry_count: 0,
    }) +
    frame('step_end', { step_id: 'step_1', status: 'ok', reason: null }) +
    epilogue('Created your feed.')
  );
}

function denyContinuation(): string {
  return (
    frame('tool_result', {
      step_id: 'step_1',
      call_id: 'call_9',
      ok: false,
      result: null,
      error: DENIAL_TEXT,
      retry_count: 0,
    }) +
    frame('step_end', {
      step_id: 'step_1',
      status: 'failed',
      reason: DENIAL_TEXT,
    }) +
    epilogue('I did not create the feed.')
  );
}

/** Final answering step + terminal frames. */
function epilogue(text: string): string {
  return (
    frame('step_start', { step_id: 'step_2', kind: 'llm' }) +
    frame('delta', { delta: text }) +
    frame('step_end', { step_id: 'step_2', status: 'ok', reason: null }) +
    frame('done', {
      usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
    }) +
    frame('terminator', { reason: null }) +
    'data: [DONE]\n\n'
  );
}

/** A read-only plan: no confirmation anywhere in the stream. */
function readOnlyStream(): string {
  return (
    frame('run_id', { run_id: RUN_ID }) +
    frame('note', { text: 'fresh_run', kind: 'fresh_run', run_id: RUN_ID }) +
    frame('plan', {
      steps: [
        { id: 'step_1', kind: 'tool', tool: 'list_feeds', args: {} },
        { id: 'step_2', kind: 'llm', prompt: 'Answer with the feed list.' },
      ],
    }) +
    frame('step_start', { step_id: 'step_1', kind: 'tool' }) +
    frame('tool_call', {
      step_id: 'step_1',
      call_id: 'call_1',
      name: 'list_feeds',
      args: {},
    }) +
    frame('tool_result', {
      step_id: 'step_1',
      call_id: 'call_1',
      ok: true,
      result: { items: [{ id: 1, name: FEED_NAME }] },
      error: null,
      retry_count: 0,
    }) +
    frame('step_end', { step_id: 'step_1', status: 'ok', reason: null }) +
    epilogue('Here are your feeds: E2E Feed.')
  );
}

interface Scenario {
  /** Frames written immediately; the response is then HELD when `resume` is set. */
  stream: string;
  /** Frames written (and the response ended) when the decision POST lands. */
  resume?: (decision: 'approve' | 'deny') => string;
  /** HTTP status for the decision POST (404 models a dead approval id). */
  approvalStatus?: number;
}

function makeMockServer(scenario: Scenario) {
  const approvals: { body: Record<string, unknown> }[] = [];
  let held: http.ServerResponse | null = null;

  const server = http.createServer((req, res) => {
    if (req.method === 'OPTIONS') {
      res.writeHead(204, CORS);
      res.end();
      return;
    }
    if (req.url?.endsWith('/chat/approvals')) {
      let raw = '';
      req.on('data', (chunk) => (raw += chunk));
      req.on('end', () => {
        const body = raw ? JSON.parse(raw) : {};
        approvals.push({ body });
        const status = scenario.approvalStatus ?? 200;
        res.writeHead(status, { ...CORS, 'content-type': 'application/json' });
        res.end(
          JSON.stringify(
            status === 200
              ? { recorded: true }
              : { detail: 'approval not found or expired' },
          ),
        );
        if (status === 200 && held && scenario.resume) {
          held.write(scenario.resume(body.decision as 'approve' | 'deny'));
          held.end();
          held = null;
        }
      });
      return;
    }
    if (req.url?.endsWith('/chat')) {
      res.writeHead(200, {
        ...CORS,
        'content-type': 'text/event-stream',
        'cache-control': 'no-cache',
      });
      res.write(scenario.stream);
      if (scenario.resume) {
        held = res; // paused until the decision POST resumes it
      } else {
        res.end();
      }
      return;
    }
    res.writeHead(404, CORS);
    res.end();
  });

  return { server, approvals };
}

async function reroute(route: Route, url: string) {
  if (route.request().method() === 'OPTIONS') {
    return route.fulfill({ status: 204, headers: CORS });
  }
  return route.continue({ url });
}

async function startMock(scenario: Scenario) {
  const mock = makeMockServer(scenario);
  await new Promise<void>((resolve) => mock.server.listen(0, '127.0.0.1', resolve));
  const { port } = mock.server.address() as AddressInfo;
  const base = `http://127.0.0.1:${port}`;
  return {
    ...mock,
    base,
    stop: () => {
      mock.server.closeAllConnections?.();
      mock.server.close();
    },
  };
}

async function routeToMock(page: Page, base: string) {
  await page.route('**/api/chat/approvals', (route) =>
    reroute(route, `${base}/chat/approvals`),
  );
  await page.route('**/api/chat', (route) => reroute(route, `${base}/chat`));
}

// --- app-driving helpers (pattern: chat-rag.spec.ts) ------------------------

function makeTestUser() {
  return {
    username: `e2e_approval_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

async function registerAndOpenChat(page: Page): Promise<void> {
  const user = makeTestUser();
  await page.goto('/');
  await page.getByRole('button', { name: /Login \/ Register/i }).click();
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(user.username);
  await page.getByLabel(/Password/i).fill(user.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await page.getByRole('button', { name: 'Open chat' }).click();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

async function sendMessage(page: Page, text: string): Promise<void> {
  const input = page.getByTestId('chat-input');
  await input.fill(text);
  await page.getByTestId('chat-send').click();
}

// --- tests -------------------------------------------------------------------

test.describe('Feed-tool approval flow (browser-level)', () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('approve: card appears mid-stream, decision posts, continuation renders', async ({
    page,
  }) => {
    const mock = await startMock({
      stream: preamble(),
      resume: () => approveContinuation(),
    });
    await routeToMock(page, mock.base);

    await sendMessage(page, `create a feed called ${FEED_NAME}`);

    const card = page.getByTestId('chat-confirm-card');
    await expect(card).toBeVisible({ timeout: 10000 });
    await expect(card).toHaveAttribute('data-approval-id', APPROVAL_ID);
    await expect(card).toHaveAttribute('data-decision', 'pending');
    await expect(page.getByTestId('chat-confirm-tool')).toHaveText('create_feed');
    await expect(page.getByTestId('chat-confirm-args')).toContainText(FEED_NAME);

    await page.getByTestId('chat-confirm-approve').click();

    await expect(card).toHaveAttribute('data-decision', 'approved');
    await expect(page.getByTestId('chat-confirm-status')).toContainText('Approved');
    // The continued frames arrive on the held connection after the POST.
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('Created your feed.', { timeout: 10000 });
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 10000,
    });

    // The decision POST carried the exact wire contract.
    expect(mock.approvals).toHaveLength(1);
    expect(mock.approvals[0].body).toEqual({
      run_id: RUN_ID,
      approval_id: APPROVAL_ID,
      decision: 'approve',
    });
    mock.stop();
  });

  test('deny: card resolves denied and the denial renders in-line', async ({
    page,
  }) => {
    const mock = await startMock({
      stream: preamble(),
      resume: () => denyContinuation(),
    });
    await routeToMock(page, mock.base);

    await sendMessage(page, `create a feed called ${FEED_NAME}`);

    const card = page.getByTestId('chat-confirm-card');
    await expect(card).toBeVisible({ timeout: 10000 });
    await page.getByTestId('chat-confirm-deny').click();

    await expect(card).toHaveAttribute('data-decision', 'denied');
    await expect(page.getByTestId('chat-confirm-status')).toContainText('Denied');
    // No mutation happened: the continuation says so and renders the denial.
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('I did not create the feed.', { timeout: 10000 });

    expect(mock.approvals).toHaveLength(1);
    expect(mock.approvals[0].body).toEqual({
      run_id: RUN_ID,
      approval_id: APPROVAL_ID,
      decision: 'deny',
    });
    mock.stop();
  });

  test('read-only call streams with no card at all', async ({ page }) => {
    const mock = await startMock({ stream: readOnlyStream() });
    await routeToMock(page, mock.base);

    await sendMessage(page, 'list my feeds');
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('Here are your feeds', { timeout: 10000 });
    await expect(page.getByTestId('chat-confirm-card')).toHaveCount(0);
    expect(mock.approvals).toHaveLength(0);
    mock.stop();
  });

  test('dead approval id: a 404 on the decision resolves the card as expired', async ({
    page,
  }) => {
    const mock = await startMock({
      stream: preamble(),
      resume: () => approveContinuation(), // never reached: the POST 404s
      approvalStatus: 404,
    });
    await routeToMock(page, mock.base);

    await sendMessage(page, `create a feed called ${FEED_NAME}`);
    const card = page.getByTestId('chat-confirm-card');
    await expect(card).toBeVisible({ timeout: 10000 });

    await page.getByTestId('chat-confirm-approve').click();

    await expect(card).toHaveAttribute('data-decision', 'expired');
    await expect(page.getByTestId('chat-confirm-status')).toContainText(
      'expired before a decision',
    );
    // No retry affordance on a dead approval, and no error hint.
    await expect(page.getByTestId('chat-confirm-approve')).toHaveCount(0);
    await expect(page.getByTestId('chat-confirm-error')).toHaveCount(0);
    mock.stop();
  });

  test('countdown: the card expires at its deadline with no decision POST', async ({
    page,
  }) => {
    const mock = await startMock({
      stream: preamble(3),
      resume: () => denyContinuation(),
    });
    await routeToMock(page, mock.base);

    await sendMessage(page, `create a feed called ${FEED_NAME}`);

    const card = page.getByTestId('chat-confirm-card');
    await expect(card).toBeVisible({ timeout: 10000 });
    await expect(card).toHaveAttribute('data-decision', 'pending');
    await expect(page.getByTestId('chat-confirm-countdown')).toContainText(
      /Expires in 0:0[23]/,
    );

    await expect(card).toHaveAttribute('data-decision', 'expired', {
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-confirm-status')).toContainText(
      /expired/i,
    );
    await expect(page.getByTestId('chat-confirm-approve')).toHaveCount(0);
    await expect(page.getByTestId('chat-confirm-deny')).toHaveCount(0);
    expect(mock.approvals).toHaveLength(0);
    mock.stop();
  });
});
