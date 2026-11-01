import { expect, type Page, type Route } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat } from './helpers/llm-config';

const RUN_ID = '22222222-2222-4222-8222-222222222222';

const UNINTERRUPTED_ANSWER = 'Hello brave world';

interface WireFrame {
  id: number;
  kind: string;
  data: string;
}

const RUN_FRAMES: WireFrame[] = [
  { id: 1, kind: 'delta', data: '{"delta":"Hello "}' },
  { id: 2, kind: 'delta', data: '{"delta":"brave "}' },
  { id: 3, kind: 'delta', data: '{"delta":"world"}' },
  {
    id: 4,
    kind: 'done',
    data: '{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}',
  },
];

function frameText(frame: WireFrame): string {
  return `id: ${frame.id}\nevent: ${frame.kind}\ndata: ${frame.data}\n\n`;
}

const CUT_STREAM =
  `event: run_id\ndata: {"run_id":"${RUN_ID}"}\n\n` +
  RUN_FRAMES.slice(0, 2).map(frameText).join('');

const REATTACHED_NOTE =
  `event: note\ndata: {"text":"reattached","kind":"reattached","run_id":"${RUN_ID}"}\n\n`;

const TRUNCATED_STREAM =
  `event: note\ndata: {"text":"replay_truncated","kind":"replay_truncated","run_id":"${RUN_ID}"}\n\n` +
  frameText(RUN_FRAMES[3]) +
  'data: [DONE]\n\n';

interface FrameReplayStub {
  reattachCursors: () => (string | null)[];
  resumeCalls: () => number;
}

async function stubFrameReplayRoutes(
  page: Page,
  opts: { truncated?: boolean } = {},
): Promise<FrameReplayStub> {
  const cursors: (string | null)[] = [];
  let resumeCalls = 0;
  let primaryPosts = 0;

  await page.route('**/api/chat/runs/*/stream', async (route: Route) => {
    const cursorRaw = route.request().headers()['last-event-id'] ?? null;
    cursors.push(cursorRaw);
    if (opts.truncated) {
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        headers: { 'cache-control': 'no-cache' },
        body: TRUNCATED_STREAM,
      });
      return;
    }
    if (cursorRaw === null) {
      await route.fulfill({
        status: 409,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'Run is not live', code: 'run_not_live' }),
      });
      return;
    }
    const cursor = Number.parseInt(cursorRaw, 10);
    const missed = RUN_FRAMES.filter((frame) => frame.id > cursor);
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: { 'cache-control': 'no-cache' },
      body:
        REATTACHED_NOTE +
        missed.map(frameText).join('') +
        'data: [DONE]\n\n',
    });
  });

  await page.route('**/api/chat/sessions/*/messages', async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ messages: [] }),
    });
  });

  await page.route('**/api/chat', async (route: Route) => {
    if (route.request().method() !== 'POST') {
      await route.fallback();
      return;
    }
    primaryPosts += 1;
    if (primaryPosts === 1) {
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        headers: { 'cache-control': 'no-cache' },
        body: CUT_STREAM,
      });
      return;
    }
    resumeCalls += 1;
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: { 'cache-control': 'no-cache' },
      body: CUT_STREAM,
    });
  });

  return {
    reattachCursors: () => cursors,
    resumeCalls: () => resumeCalls,
  };
}

test.describe('Chat stream frame replay (browser-level)', () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page, { prefix: 'e2e_frame_replay' });
  });

  test('a cut stream resumes from the last frame id with no gap', async ({
    page,
  }) => {
    const stub = await stubFrameReplayRoutes(page);

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    const bubble = page.getByTestId('chat-message-assistant').last();
    await expect(bubble).toContainText('Hello brave', { timeout: 15_000 });
    await expect(page.getByTestId('chat-message-content').last()).toHaveText(
      UNINTERRUPTED_ANSWER,
      { timeout: 20_000 },
    );

    const cursors = stub.reattachCursors();
    expect(cursors.length).toBeGreaterThan(0);
    expect(
      cursors.every((cursor) => cursor === '2'),
      `every re-attach must present the last rendered frame id, got ${JSON.stringify(cursors)}`,
    ).toBe(true);
    expect(
      stub.resumeCalls(),
      'the cursor-driven re-attach must complete the turn on its own',
    ).toBe(0);
    await expect(page.getByTestId('chat-message-assistant')).toHaveCount(1);
    await expect(page.getByTestId('chat-possibly-incomplete')).toHaveCount(0);
    await expect(page.getByTestId('chat-error')).toHaveCount(0);
  });

  test('a truncated cursor keeps the partial answer and marks the turn', async ({
    page,
  }) => {
    const stub = await stubFrameReplayRoutes(page, { truncated: true });

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    const bubble = page.getByTestId('chat-message-assistant').last();
    await expect(bubble).toContainText('Hello', { timeout: 15_000 });
    await expect(page.getByTestId('chat-possibly-incomplete')).toBeVisible({
      timeout: 20_000,
    });

    const cursors = stub.reattachCursors();
    expect(cursors.length).toBeGreaterThan(0);
    expect(cursors.every((cursor) => cursor === '2')).toBe(true);
    await expect(page.getByTestId('chat-message-content').last()).toHaveText(
      'Hello brave',
    );
  });
});
