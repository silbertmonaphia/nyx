import { expect, type Locator, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

const CURRENT_SESSION_KEY = 'nyx.chat.currentSessionId';

function makeTestUser() {
  return {
    username: `e2e_steps_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

async function registerAndOpenChat(page: Page): Promise<void> {
  const testUser = makeTestUser();
  await page.goto('/');
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(testUser.username);
  await page.getByLabel(/Password/i).fill(testUser.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

async function sendAndWaitForReply(page: Page, prompt: string): Promise<void> {
  await page.getByTestId('chat-input').fill(prompt);
  await page.getByTestId('chat-send').click();
  await expect(page.getByTestId('chat-message-assistant').last()).not.toHaveText(
    /^[\s…]*$/u,
    { timeout: 45000 },
  );
  await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
    timeout: 45000,
  });
  await expect(page.getByTestId('chat-error')).toHaveCount(0);
}

async function expandSteps(page: Page): Promise<Locator> {
  const disclosure = page.getByTestId('chat-agent-steps').last();
  await expect(disclosure).toBeVisible({ timeout: 15000 });
  const isOpen = await disclosure.evaluate(
    (el) => (el as HTMLDetailsElement).open,
  );
  if (!isOpen) {
    await disclosure.locator(':scope > summary').click();
  }
  await expect(disclosure).toHaveAttribute('open', '', { timeout: 5000 });
  return disclosure;
}

interface StepSnapshot {
  id: string | null;
  status: string | null;
  text: string;
}

async function readSteps(page: Page): Promise<StepSnapshot[]> {
  const disclosure = await expandSteps(page);
  const rows = disclosure.getByTestId('chat-agent-step');
  await expect(rows.first()).toBeVisible({ timeout: 10000 });
  const snapshot = await rows.evaluateAll((elements) =>
    elements.map((el) => ({
      id: el.getAttribute('data-step-id'),
      status: el.getAttribute('data-step-status'),
      text: (el.textContent ?? '').replace(/\s+/g, ' ').trim(),
    })),
  );
  expect(snapshot.length).toBeGreaterThan(0);
  for (const row of snapshot) {
    expect(row.id).not.toBeNull();
    expect(['ok', 'failed']).toContain(row.status);
  }
  return snapshot;
}

async function readThreePartUsage(page: Page): Promise<string> {
  const usageLine = page.getByText(/令牌：|tokens:/).last();
  await expect(usageLine).toBeVisible({ timeout: 15000 });
  const text = (await usageLine.textContent()) ?? '';
  const numbers = (text.match(/\d+/g) ?? []).map(Number);
  expect(numbers.length).toBe(3);
  const [total, prompt, completion] = numbers;
  expect(prompt).toBeGreaterThan(0);
  expect(completion).toBeGreaterThan(0);
  expect(total).toBe(prompt + completion);
  return text;
}

async function persistedSessionId(page: Page): Promise<string | null> {
  return page.evaluate(
    (key) => window.sessionStorage.getItem(key),
    CURRENT_SESSION_KEY,
  );
}

async function waitForPersistedSessionId(page: Page): Promise<string> {
  await expect
    .poll(() => persistedSessionId(page), { timeout: 15000 })
    .not.toBeNull();
  const id = await persistedSessionId(page);
  if (id === null) {
    throw new Error('session id was not persisted');
  }
  return id;
}

test.describe('Chat / step trace replay (live backend)', { tag: '@live' }, () => {
  test.describe.configure({ timeout: 120000 });

  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('step rows and the token split survive closing and reopening the panel', async ({
    page,
  }) => {
    const prompt = `Say exactly: hello-from-browser (${Date.now()}-reopen)`;
    await sendAndWaitForReply(page, prompt);
    const liveSteps = await readSteps(page);
    const liveUsage = await readThreePartUsage(page);

    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).toHaveCount(0);

    await page.getByRole('button', { name: /Open AI Agent/i }).click();
    await expect(page.getByTestId('chat-form')).toBeVisible();

    await expect(page.getByTestId('chat-message-user').last()).toHaveText(
      prompt,
      { timeout: 15000 },
    );
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).not.toHaveText(/^[\s…]*$/u, { timeout: 15000 });

    expect(await readSteps(page)).toEqual(liveSteps);
    expect(await readThreePartUsage(page)).toBe(liveUsage);
  });

  test('step rows and the token split survive a session rail switch', async ({
    page,
  }) => {
    const prompt = `Say exactly: hello-from-browser (${Date.now()}-rail)`;
    await sendAndWaitForReply(page, prompt);
    const liveSteps = await readSteps(page);
    const liveUsage = await readThreePartUsage(page);
    const sessionId = await waitForPersistedSessionId(page);

    await page.getByTestId('chat-new-session').click();
    await expect(page.getByText(/No messages yet/)).toBeVisible();
    const newSessionId = await waitForPersistedSessionId(page);
    expect(newSessionId).not.toBe(sessionId);

    const row = page.locator(
      `[data-testid="chat-session-row"][data-session-id="${sessionId}"]`,
    );
    await expect(row).toBeVisible({ timeout: 15000 });
    await row.click();

    await expect(page.getByTestId('chat-message-user').last()).toHaveText(
      prompt,
      { timeout: 15000 },
    );
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).not.toHaveText(/^[\s…]*$/u, { timeout: 15000 });

    expect(await readSteps(page)).toEqual(liveSteps);
    expect(await readThreePartUsage(page)).toBe(liveUsage);
  });
});
