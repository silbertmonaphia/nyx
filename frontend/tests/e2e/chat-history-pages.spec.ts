import { expect, type Page, type Route } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat as registerAndOpenChatShared } from './helpers/llm-config';

async function registerAndOpenChat(page: Page): Promise<void> {
  await registerAndOpenChatShared(page, { prefix: 'e2e_history_pages' });
}

interface SessionRow {
  id: string;
  title?: string;
  message_count?: number;
}

interface ReplayMessage {
  id: number;
  role: 'user' | 'assistant';
  content: string;
}

function sessionJson(row: SessionRow): Record<string, unknown> {
  return {
    id: row.id,
    user_id: 1,
    title: row.title ?? '',
    started_at: '2026-11-13T00:00:00Z',
    last_active_at: '2026-11-13T00:00:00Z',
    ended_at: null,
    message_count: row.message_count ?? 2,
  };
}

async function json(route: Route, status: number, body: unknown): Promise<void> {
  await route.fulfill({
    status,
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
}

interface SessionsApiStub {
  listCalls: number;
  messageCalls: number;
  setRows: (rows: SessionRow[]) => void;
}

async function stubSessionsApi(
  page: Page,
  opts: {
    rows: SessionRow[];
    messagesFor: (url: URL) => {
      messages: ReplayMessage[];
      has_more: boolean;
      next_before: string | null;
    };
    messagesDelayMs?: number;
  },
): Promise<SessionsApiStub> {
  const state = { rows: opts.rows };
  const stub: SessionsApiStub = {
    listCalls: 0,
    messageCalls: 0,
    setRows: (rows) => {
      state.rows = rows;
    },
  };
  await page.route('**/api/chat/sessions**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const method = request.method();
    if (method === 'POST' && url.pathname.endsWith('/resume')) {
      await json(route, 200, { session_id: 's', resumed_at: '2026-11-13T00:00:00Z' });
      return;
    }
    if (method === 'POST' && /\/api\/chat\/sessions$/.test(url.pathname)) {
      await json(route, 200, { session_id: 's-new', started_at: '2026-11-13T00:00:00Z' });
      return;
    }
    if (method === 'DELETE') {
      await route.fulfill({ status: 204, body: '' });
      return;
    }
    if (method === 'GET' && url.pathname.endsWith('/messages')) {
      stub.messageCalls += 1;
      const body = opts.messagesFor(url);
      if (opts.messagesDelayMs) {
        await new Promise((resolve) => setTimeout(resolve, opts.messagesDelayMs));
      }
      await json(route, 200, body);
      return;
    }
    if (method === 'GET' && /\/api\/chat\/sessions$/.test(url.pathname)) {
      stub.listCalls += 1;
      await json(route, 200, {
        sessions: state.rows.map(sessionJson),
        next_cursor: null,
      });
      return;
    }
    await route.continue();
  });
  return stub;
}

function pageOne(): ReplayMessage[] {
  return Array.from({ length: 30 }, (_, i) => ({
    id: 31 + i,
    role: i % 2 === 0 ? ('user' as const) : ('assistant' as const),
    content: `m-${31 + i}`,
  }));
}

function pageOlder(): ReplayMessage[] {
  return Array.from({ length: 30 }, (_, i) => ({
    id: 1 + i,
    role: i % 2 === 0 ? ('user' as const) : ('assistant' as const),
    content: `m-${1 + i}`,
  }));
}

async function messageList(page: Page) {
  return page.getByTestId('chat-message-list');
}

test.describe('Chat replay paging (browser-level, stubbed sessions API)', () => {
  test('click → skeleton → latest page; scroll-up prepends older pages without a jump', async ({
    page,
  }) => {
    await stubSessionsApi(page, {
      rows: [
        { id: 's2', title: 'Newest', message_count: 60 },
        { id: 's1', title: 'Older', message_count: 2 },
      ],
      messagesDelayMs: 400,
      messagesFor: (url) => {
        const before = url.searchParams.get('before');
        if (before === null) {
          return { messages: pageOne(), has_more: true, next_before: '31' };
        }
        return { messages: pageOlder(), has_more: false, next_before: null };
      },
    });
    await registerAndOpenChat(page);

    const row = page.locator('[data-testid="chat-session-row"][data-session-id="s2"]');
    await expect(row).toBeVisible({ timeout: 15000 });
    await row.click();

    await expect(page.getByTestId('chat-replay-skeleton')).toBeVisible();
    await expect(page.getByTestId('chat-message-user').first()).toBeVisible({
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-replay-skeleton')).toHaveCount(0);
    expect(await page.getByTestId('chat-message-content').count()).toBe(30);
    await expect(page.getByTestId('chat-replay-top-sentinel')).toBeAttached();

    const list = await messageList(page);
    await list.evaluate((el) => {
      el.scrollTop = 0;
    });
    const anchorBefore = await page.getByText('m-31', { exact: true }).boundingBox();
    expect(anchorBefore).not.toBeNull();

    await expect
      .poll(() => page.getByTestId('chat-message-content').count(), {
        timeout: 15000,
      })
      .toBe(60);
    await expect(page.getByTestId('chat-replay-top-sentinel')).toHaveCount(0);

    const anchorAfter = await page.getByText('m-31', { exact: true }).boundingBox();
    expect(anchorAfter).not.toBeNull();
    expect(Math.abs(anchorAfter!.y - anchorBefore!.y)).toBeLessThan(60);
    await expect(page.getByText('m-1', { exact: true })).toBeAttached();
    await expect(page.getByText('m-60', { exact: true })).toBeAttached();
  });

  test('a single-page history never arms the sentinel', async ({ page }) => {
    const stub = await stubSessionsApi(page, {
      rows: [{ id: 's1', title: 'Short', message_count: 2 }],
      messagesFor: () => ({
        messages: [
          { id: 1, role: 'user', content: 'only question' },
          { id: 2, role: 'assistant', content: 'only answer' },
        ],
        has_more: false,
        next_before: null,
      }),
    });
    await registerAndOpenChat(page);

    await page
      .locator('[data-testid="chat-session-row"][data-session-id="s1"]')
      .click();
    await expect(page.getByText('only question', { exact: true })).toBeVisible({
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-replay-top-sentinel')).toHaveCount(0);

    const list = await messageList(page);
    await list.evaluate((el) => {
      el.scrollTop = 0;
      el.dispatchEvent(new Event('scroll'));
    });
    await page.waitForTimeout(500);
    expect(stub.messageCalls).toBe(1);
  });

  test('regaining focus refreshes the rail without a reload', async ({ page }) => {
    const stub = await stubSessionsApi(page, {
      rows: [{ id: 's1', title: 'Existing', message_count: 0 }],
      messagesFor: () => ({ messages: [], has_more: false, next_before: null }),
    });
    await registerAndOpenChat(page);

    await expect(page.getByTestId('chat-session-row')).toHaveCount(1);
    await page.waitForTimeout(500);
    await page.clock.install();
    const listsBefore = stub.listCalls;
    stub.setRows([
      { id: 's2', title: 'From another tab', message_count: 0 },
      { id: 's1', title: 'Existing', message_count: 0 },
    ]);

    await page.clock.fastForward(31_000);
    await page.evaluate(() => {
      window.dispatchEvent(new Event('focus'));
    });

    await expect
      .poll(() => page.getByTestId('chat-session-row').count(), {
        timeout: 15000,
      })
      .toBe(2);
    expect(stub.listCalls).toBeGreaterThan(listsBefore);
    await expect(
      page.locator('[data-testid="chat-session-row"][data-session-id="s2"]'),
    ).toBeVisible();
  });
});
