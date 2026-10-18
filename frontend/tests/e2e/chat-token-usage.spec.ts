import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

function makeTestUser() {
  return {
    username: `e2e_tokens_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

async function registerAndOpenChat(page: Page): Promise<void> {
  const TEST_USER = makeTestUser();
  await page.goto('/');
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(TEST_USER.username);
  await page.getByLabel(/Password/i).fill(TEST_USER.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

test.describe('Chat / real token usage line', { tag: '@live' }, () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('assistant reply shows prompt/completion/total that add up', async ({
    page,
  }) => {
    const input = page.getByTestId('chat-input');
    await input.fill(`Reply with one short sentence. (${Date.now()})`);
    await page.getByTestId('chat-send').click();

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 45000,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    const usageLine = page
      .getByText(/令牌：|tokens:/)
      .last();
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
  });
});
