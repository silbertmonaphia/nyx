import { expect } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { makeTestUser, registerViaUi } from './helpers/llm-config';

const GATE_TITLE = 'LLM configuration required';

const LONG_REQUEST_BODY = {
  model: 'e2e-layout-model',
  messages: [{ role: 'user', content: 'probe '.repeat(400) }],
  max_tokens: 64,
};

test.describe('LLM config modal layout (browser-level)', () => {
  test.use({ viewport: { width: 1280, height: 520 } });

  test('keeps the save action pinned while an expanded probe request scrolls', async ({
    page,
  }) => {
    await page.route('**/api/me/llm-config/validate', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          slots: {
            chat: {
              probed: true,
              status: 'ok',
              http_status: 200,
              detail: 'Connected',
              request: {
                method: 'POST',
                url: 'https://api.deepseek.com/v1/chat/completions',
                headers: {
                  'Content-Type': 'application/json',
                  Authorization: 'Bearer ••••9876',
                },
                body: LONG_REQUEST_BODY,
              },
            },
            embedding: null,
            rerank: null,
            fallback: null,
            tavily: null,
          },
        }),
      }),
    );

    const user = makeTestUser('e2e_llmcfg_layout');
    await registerViaUi(page, user);
    const gate = page.getByRole('dialog', { name: GATE_TITLE });
    await expect(gate).toBeVisible();

    await page.locator('#llm-base-url').fill('https://api.deepseek.com/v1');
    await page.locator('#llm-model').fill('e2e-layout-model');
    await page.locator('#llm-api-key').fill('sk-e2e-layout-key');

    await page.getByTestId('llm-probe-chat-button').click();
    await expect(page.getByTestId('llm-probe-chat-status')).toBeVisible();
    await page.getByTestId('llm-probe-chat-toggle').click();
    await expect(page.getByTestId('llm-probe-chat-request')).toBeVisible();

    const body = page.getByTestId('llm-config-body');
    const save = page.getByTestId('llm-config-save');

    expect(await body.evaluate((el) => el.scrollHeight > el.clientHeight)).toBe(
      true,
    );
    expect(
      await body.evaluate(
        (el) =>
          el.contains(
            document.querySelector('[data-testid="llm-config-save"]'),
          ),
      ),
    ).toBe(false);
    await expect(save).toBeInViewport({ ratio: 1 });
    await save.click({ trial: true });

    await body.evaluate((el) => {
      el.scrollTop = el.scrollHeight;
    });
    expect(await body.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
    await expect(save).toBeInViewport({ ratio: 1 });
    await save.click({ trial: true });
    await expect(page.getByTestId('llm-probe-chat-request')).toBeVisible();
  });
});
