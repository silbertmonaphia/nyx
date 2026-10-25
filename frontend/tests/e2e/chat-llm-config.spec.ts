import { expect } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import {
  closeChatPanel,
  makeTestUser,
  registerAndOpenChat,
  registerViaUi,
  seedLlmConfig,
} from './helpers/llm-config';

const GATE_TITLE = 'LLM configuration required';

test.describe('LLM configuration gate (browser-level)', () => {
  test('a fresh user sees the blocking modal instead of the chat panel', async ({
    page,
  }) => {
    const user = makeTestUser('e2e_llmcfg_gate');
    await registerViaUi(page, user);

    const gate = page.getByRole('dialog', { name: GATE_TITLE });
    await expect(gate).toBeVisible();
    await expect(gate.getByRole('button', { name: 'Close' })).toHaveCount(0);
    await expect(page.getByTestId('chat-form')).not.toBeVisible();
    await expect(page.getByTestId('llm-config-clear')).toHaveCount(0);
  });

  test('saving all eight required fields opens the chat panel', async ({
    page,
  }) => {
    const user = makeTestUser('e2e_llmcfg_save');
    await registerViaUi(page, user);
    await expect(
      page.getByRole('dialog', { name: GATE_TITLE }),
    ).toBeVisible();

    await page.locator('#llm-base-url').fill('https://llm.example.test/v1');
    await page.locator('#llm-model').fill('e2e-main-model');
    await page.locator('#llm-api-key').fill('sk-e2e-main-abcd');
    await page
      .locator('#embedding-base-url')
      .fill('https://embed.example.test/v1');
    await page.locator('#embedding-model').fill('e2e-embed-model');
    await page.locator('#embedding-api-key').fill('sk-e2e-embed-ef12');
    await page
      .locator('#tavily-base-url')
      .fill('https://tavily.example.test/search');
    await page.locator('#tavily-api-key').fill('tvly-e2e-tavily-wxyz');

    await page.getByTestId('llm-config-save').click();

    await expect(
      page.getByTestId('toast').filter({ hasText: 'LLM configuration saved' }),
    ).toBeVisible();
    await expect(page.getByTestId('chat-form')).toBeVisible();
  });

  test('the gear reopens with non-key fields prefilled and keys masked', async ({
    page,
  }) => {
    await registerAndOpenChat(page, { prefix: 'e2e_llmcfg_gear' });
    await seedLlmConfig(page, {
      llm: {
        base_url: 'https://llm.example.test/v1',
        model: 'e2e-main-model',
        api_key: 'sk-e2e-main-abcd',
      },
      embedding: {
        base_url: 'https://embed.example.test/v1',
        model: 'e2e-embed-model',
        api_key: 'sk-e2e-embed-ef12',
      },
      tavily: {
        base_url: 'https://tavily.example.test/search',
        api_key: 'tvly-e2e-tavily-wxyz',
      },
    });
    await page.reload();
    await expect(page.getByTestId('chat-form')).toBeVisible();
    await closeChatPanel(page);

    await page.getByTestId('llm-config-button').click();

    await expect(page.locator('#llm-base-url')).toHaveValue(
      'https://llm.example.test/v1',
    );
    await expect(page.locator('#llm-model')).toHaveValue('e2e-main-model');
    await expect(page.locator('#embedding-base-url')).toHaveValue(
      'https://embed.example.test/v1',
    );
    await expect(page.locator('#embedding-model')).toHaveValue(
      'e2e-embed-model',
    );
    await expect(page.locator('#tavily-base-url')).toHaveValue(
      'https://tavily.example.test/search',
    );

    const savedKeys = [
      { id: '#llm-api-key', tail: 'abcd' },
      { id: '#embedding-api-key', tail: 'ef12' },
      { id: '#tavily-api-key', tail: 'wxyz' },
    ];
    for (const { id, tail } of savedKeys) {
      const input = page.locator(id);
      await expect(input).toHaveValue('');
      await expect(input).toHaveAttribute('placeholder', /leave blank/i);
      await expect(input).toHaveAttribute('placeholder', new RegExp(tail));
    }

    await page.getByTestId('llm-config-save').click();
    await expect(
      page.getByTestId('toast').filter({ hasText: 'LLM configuration saved' }),
    ).toBeVisible();
    await expect(page.getByTestId('chat-form')).toBeVisible();
    await closeChatPanel(page);

    await page.getByTestId('llm-config-button').click();
    await expect(page.locator('#llm-base-url')).toHaveValue(
      'https://llm.example.test/v1',
    );
    await expect(page.locator('#llm-api-key')).toHaveAttribute(
      'placeholder',
      new RegExp('abcd'),
    );
  });

  test('clearing the configuration re-gates the chat panel', async ({
    page,
  }) => {
    await registerAndOpenChat(page, { prefix: 'e2e_llmcfg_clear' });
    await closeChatPanel(page);
    await page.getByTestId('llm-config-button').click();
    await expect(page.getByTestId('llm-config-clear')).toBeVisible();
    await page.getByTestId('llm-config-clear').click();
    await page.getByTestId('confirm-llm-clear').click();

    await expect(
      page.getByTestId('toast').filter({ hasText: 'LLM configuration cleared' }),
    ).toBeVisible();
    await page.getByRole('button', { name: 'Open AI Agent' }).click();
    await expect(
      page.getByRole('dialog', { name: GATE_TITLE }),
    ).toBeVisible();
    await expect(page.locator('#llm-base-url')).toHaveValue('');
    await expect(page.locator('#llm-api-key')).toHaveValue('');
    await expect(page.locator('#tavily-api-key')).toHaveValue('');
    await expect(page.getByTestId('chat-form')).not.toBeVisible();
  });

  test('a routed 409 from the chat endpoint opens the gate', async ({
    page,
  }) => {
    await registerAndOpenChat(page, { prefix: 'e2e_llmcfg_409' });
    await page.route('**/api/chat', (route) =>
      route.fulfill({
        status: 409,
        contentType: 'application/json',
        body: JSON.stringify({
          error: 'LLM configuration required',
          code: 409,
          request_id: 'e2e-llm-not-configured',
          details: 'llm_not_configured',
        }),
      }),
    );

    await page.getByTestId('chat-input').fill('hello');
    await page.getByTestId('chat-send').click();

    await expect(
      page.getByRole('dialog', { name: GATE_TITLE }),
    ).toBeVisible();
    await expect(page.getByTestId('chat-form')).not.toBeVisible();
  });
});
