import { expect, type Page, type Route } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

const LONG_TOKEN = 'https://cdn.example.com/assets/' + 'a1b2c3d4'.repeat(30);

function makeTestUser() {
  return {
    username: `e2e_layout_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

async function registerAndOpenChat(page: Page): Promise<void> {
  const user = makeTestUser();
  await page.goto('/');
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(user.username);
  await page.getByLabel(/Password/i).fill(user.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

async function stubChatSse(
  page: Page,
  frames: Array<Record<string, unknown>>,
): Promise<void> {
  const body =
    frames
      .map((f) => `event: ${f.kind}\ndata: ${JSON.stringify(f)}\n\n`)
      .join('') + 'data: [DONE]\n\n';
  await page.route('**/api/chat', async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: { 'cache-control': 'no-cache' },
      body,
    });
  });
}

async function sendLongTokenTurn(page: Page): Promise<void> {
  await stubChatSse(page, [
    { kind: 'run_id', run_id: '00000000-0000-4000-8000-0000000000aa' },
    { kind: 'delta', delta: `Link: ${LONG_TOKEN}` },
    {
      kind: 'done',
      usage: { prompt_tokens: 1, completion_tokens: 2, total_tokens: 3 },
    },
    { kind: 'terminator', reason: null },
  ]);

  await page.getByTestId('chat-input').fill('show me the link');
  await page.getByTestId('chat-send').click();
  await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
    timeout: 15000,
  });
  await expect(page.getByTestId('chat-message-assistant').last()).toContainText(
    LONG_TOKEN,
  );
}

async function expectInteriorContained(page: Page): Promise<void> {
  const geometry = await page.evaluate(() => {
    const panel = document.querySelector(
      '[data-testid=chat-panel]',
    ) as HTMLElement;
    const list = document.querySelector(
      '[data-testid=chat-message-list]',
    ) as HTMLElement;
    const input = document.querySelector(
      '[data-testid=chat-input]',
    ) as HTMLElement;
    const p = panel.getBoundingClientRect();
    const l = list.getBoundingClientRect();
    const i = input.getBoundingClientRect();
    return {
      panelWidth: p.width,
      panelRight: p.right,
      listRight: l.right,
      inputRight: i.right,
      listScrollWidth: list.scrollWidth,
      listClientWidth: list.clientWidth,
    };
  });

  expect(geometry.panelWidth).toBeLessThanOrEqual(897);
  expect(geometry.listRight).toBeLessThanOrEqual(geometry.panelRight - 23);
  expect(geometry.inputRight).toBeLessThanOrEqual(geometry.panelRight - 23);
  expect(geometry.listScrollWidth).toBeLessThanOrEqual(
    geometry.listClientWidth + 1,
  );
}

test.describe('Chat panel layout', () => {
  test('a long unbreakable token does not widen the panel interior', async ({
    page,
  }) => {
    await registerAndOpenChat(page);
    await sendLongTokenTurn(page);
    await expectInteriorContained(page);
  });

  test('a long unbreakable token does not widen the panel interior below md', async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await registerAndOpenChat(page);
    await sendLongTokenTurn(page);
    await expectInteriorContained(page);
  });

  test('the panel height stays fixed while messages accumulate', async ({
    page,
  }) => {
    await registerAndOpenChat(page);
    const panel = page.getByTestId('chat-panel');
    await page.waitForTimeout(300);
    const before = await panel.boundingBox();
    expect(before).not.toBeNull();

    for (let turn = 0; turn < 4; turn++) {
      await sendLongTokenTurn(page);
    }

    const after = await panel.boundingBox();
    expect(after).not.toBeNull();
    expect(Math.abs(after!.height - before!.height)).toBeLessThanOrEqual(1);

    const listMetrics = await page.getByTestId('chat-message-list').evaluate(
      (el) => ({
        scrollHeight: el.scrollHeight,
        clientHeight: el.clientHeight,
      }),
    );
    expect(listMetrics.scrollHeight).toBeGreaterThan(listMetrics.clientHeight);
  });
});
