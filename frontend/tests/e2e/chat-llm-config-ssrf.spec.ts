import { expect } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { makeTestUser, registerViaUi } from './helpers/llm-config';

const GATE_TITLE = 'LLM configuration required';

const SAVE_RESPONSE_TIMEOUT = 15_000;

const PRIVATE_DENIED = process.env.E2E_BYOK_PRIVATE_DENIED !== 'false';

test.describe('BYOK base URL SSRF guard (browser-level)', () => {
  test('a loopback base URL follows the server BYOK private-URL posture', async ({
    page,
  }) => {
    const user = makeTestUser('e2e_llmcfg_ssrf_real');
    await registerViaUi(page, user);
    await expect(
      page.getByRole('dialog', { name: GATE_TITLE }),
    ).toBeVisible();

    await page.locator('#llm-base-url').fill('http://127.0.0.1:8000/v1');
    await page.locator('#llm-model').fill('e2e-ssrf-model');
    await page.locator('#llm-api-key').fill('sk-e2e-ssrf-abcd');

    const saveResponse = page.waitForResponse(
      (r) =>
        r.request().method() === 'PUT' &&
        r.url().includes('/me/llm-config'),
      { timeout: SAVE_RESPONSE_TIMEOUT },
    );
    await page.getByTestId('llm-config-save').click();
    const status = (await saveResponse).status();

    if (PRIVATE_DENIED) {
      expect(
        status,
        'the server must refuse a loopback BYOK base URL (BYOK_ALLOW_PRIVATE_URL must stay off)',
      ).toBe(400);
      await expect(
        page
          .getByTestId('toast')
          .filter({ hasText: 'Invalid LLM configuration' }),
      ).toBeVisible({ timeout: SAVE_RESPONSE_TIMEOUT });
      await expect(page.getByTestId('llm-config-save')).toBeVisible();
      await expect(page.getByTestId('chat-form')).not.toBeVisible();
      return;
    }

    expect(
      status,
      'the stack allows private BYOK targets (E2E_BYOK_PRIVATE_DENIED=false)',
    ).toBe(200);
    await expect(
      page.getByTestId('toast').filter({ hasText: 'LLM configuration saved' }),
    ).toBeVisible({ timeout: SAVE_RESPONSE_TIMEOUT });
  });
});
