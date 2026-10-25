import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat as registerAndOpenChatShared } from './helpers/llm-config';

async function registerAndOpenChat(page: Page): Promise<void> {
  await registerAndOpenChatShared(page, { prefix: 'e2e_tokens' });
}

async function sendAndWaitForReply(page: Page): Promise<void> {
  await page
    .getByTestId('chat-input')
    .fill(`Reply with one short sentence. (${Date.now()})`);
  await page.getByTestId('chat-send').click();

  await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
    timeout: 45000,
  });
  await expect(page.getByTestId('chat-error')).toHaveCount(0);
}

async function readThreePartUsage(page: Page): Promise<string> {
  const usageLine = page.getByText(/令牌：|tokens:/).last();
  await expect(usageLine).toBeVisible();

  const text = (await usageLine.textContent()) ?? '';
  const numbers = (text.match(/\d+/g) ?? []).map(Number);
  expect(numbers.length).toBe(3);

  const [total, prompt, completion] = numbers;
  expect(
    prompt,
    `expected prompt tokens > 0 in "${text}" (0 means the estimate path leaked through)`,
  ).toBeGreaterThan(0);
  expect(completion).toBeGreaterThan(0);
  expect(total).toBe(prompt + completion);
  return text;
}

test.describe('Chat / real token usage line', { tag: '@live' }, () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('assistant reply shows prompt/completion/total that add up', async ({
    page,
  }) => {
    await sendAndWaitForReply(page);
    await readThreePartUsage(page);
  });

  test('usage line survives closing and reopening the panel', async ({
    page,
  }) => {
    await sendAndWaitForReply(page);
    const liveUsage = await readThreePartUsage(page);

    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).toHaveCount(0);

    await page.getByRole('button', { name: /Open AI Agent/i }).click();
    await expect(page.getByTestId('chat-form')).toBeVisible();

    const replayedUsage = await readThreePartUsage(page);
    expect(replayedUsage).toBe(liveUsage);
  });
});
