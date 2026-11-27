import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

const USER = {
  id: 1,
  username: 'gate_user',
  created_at: '2026-10-17T00:00:00Z',
  updated_at: '2026-10-17T00:00:00Z',
};

const stubbedLlmConfig = {
  configured: true,
  llm: {
    base_url: 'https://llm.example.test/v1',
    model: 'stub-model',
    api_key_masked: '…mock',
  },
  embedding: {
    base_url: 'https://embed.example.test/v1',
    model: 'stub-embed',
    api_key_masked: '…mock',
  },
  tavily: {
    base_url: 'https://tavily.example.test/search',
    api_key_masked: '…mock',
  },
  fallback: null,
};

async function stubRegistrationMode(page: Page, mode: string): Promise<void> {
  await page.route('**/api/config', (route) =>
    route.fulfill({ json: { registration_mode: mode } }),
  );
}

async function stubAuthenticated(
  page: Page,
  features: Record<string, unknown>,
): Promise<void> {
  await page.addInitScript((user) => {
    window.localStorage.setItem(
      'nyx-auth-storage',
      JSON.stringify({ state: { user }, version: 3 }),
    );
    window.localStorage.setItem(
      'nyx-token-store',
      JSON.stringify({ accessToken: 'stub-at', refreshToken: 'stub-rt' }),
    );
  }, USER);
  await page.route(
    (url) => url.pathname.startsWith('/api/'),
    (route) => {
      const pathname = new URL(route.request().url()).pathname;
      if (pathname.endsWith('/features')) return route.fulfill({ json: features });
      if (pathname.endsWith('/me/llm-config')) {
        return route.fulfill({ json: stubbedLlmConfig });
      }
      if (pathname.endsWith('/me')) return route.fulfill({ json: USER });
      if (pathname.endsWith('/feeds')) {
        return route.fulfill({
          json: { data: [], page: 1, page_size: 20, total: 0, has_more: false },
        });
      }
      if (pathname.endsWith('/chat/sessions')) {
        return route.fulfill({ json: { sessions: [], next_cursor: null } });
      }
      if (pathname.endsWith('/kb/settings')) {
        return route.fulfill({ json: { enabled: true, embedding_model: '' } });
      }
      return route.fulfill({ status: 404, json: { detail: 'stubbed' } });
    },
  );
}

test.describe('access gates', () => {
  test('closed registration hides the register toggle and explains why', async ({
    page,
  }) => {
    await stubRegistrationMode(page, 'closed');
    await page.goto('/');

    await expect(page.getByRole('dialog')).toBeVisible();
    await expect(page.getByTestId('registration-closed-hint')).toHaveText(
      'New registrations are currently closed.',
    );
    await expect(
      page.getByRole('button', { name: 'Register', exact: true }),
    ).toHaveCount(0);
    await expect(
      page.getByRole('button', { name: 'Login', exact: true }),
    ).toBeVisible();
  });

  test('invite-only registration keeps the form and shows a hint', async ({
    page,
  }) => {
    await stubRegistrationMode(page, 'invite_only');
    await page.goto('/');

    const registerToggle = page.getByRole('button', {
      name: 'Register',
      exact: true,
    });
    await registerToggle.click();

    await expect(page.getByTestId('registration-invite-hint')).toHaveText(
      'Registration is invite-only.',
    );
    await expect(
      page.getByRole('button', { name: 'Register', exact: true }),
    ).toBeVisible();
  });

  test('fails open when the public config probe fails', async ({ page }) => {
    await page.route('**/api/config', (route) => route.abort());
    await page.goto('/');

    await expect(
      page.getByRole('button', { name: 'Register', exact: true }),
    ).toBeVisible();
    await expect(page.getByTestId('registration-closed-hint')).toHaveCount(0);
  });

  test('a rejected registration shows the inline error', async ({ page }) => {
    await stubRegistrationMode(page, 'invite_only');
    await page.route('**/api/register', (route) =>
      route.fulfill({
        status: 403,
        json: { error: 'Registration is not available', code: 403 },
      }),
    );
    await page.goto('/');

    await page.getByRole('button', { name: 'Register', exact: true }).click();
    await page.getByLabel('Username').fill('not_invited_user');
    await page.getByLabel('Password').fill('hunter2');
    await page.getByRole('button', { name: 'Register', exact: true }).click();

    await expect(page.getByRole('alert')).toHaveText(
      'Registration is not available.',
    );
  });

  test('chat_allowed=false hides every AI surface', async ({ page }) => {
    const featuresResponse = page.waitForResponse(
      (resp) => resp.url().includes('/api/features') && resp.status() === 200,
    );
    await stubAuthenticated(page, { agent_enabled: false, chat_allowed: false });
    await page.goto('/');
    await featuresResponse;

    await expect(
      page.getByRole('button', { name: 'Logout', exact: true }),
    ).toBeVisible();
    await expect(page.getByTestId('chat-panel')).toHaveCount(0);
    await expect(
      page.locator('button[aria-label="Open AI Agent"]'),
    ).toHaveCount(0);
    await expect(page.getByTestId('llm-config-button')).toHaveCount(0);
    await expect(page.getByTestId('view-tab-knowledge')).toHaveCount(0);
    await expect(page.getByTestId('view-tab-knowledge-mobile')).toHaveCount(0);
  });

  test('a missing chat_allowed fails open', async ({ page }) => {
    const featuresResponse = page.waitForResponse(
      (resp) => resp.url().includes('/api/features') && resp.status() === 200,
    );
    await stubAuthenticated(page, { agent_enabled: false });
    await page.goto('/');
    await featuresResponse;

    await expect(page.getByTestId('chat-panel')).toBeVisible();
    await expect(
      page.locator('button[aria-label="Open AI Agent"]'),
    ).toBeVisible();
    await expect(page.getByTestId('view-tab-knowledge')).toBeVisible();
  });
});
