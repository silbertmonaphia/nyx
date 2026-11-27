import { expect, type APIRequestContext, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

const API = (process.env.E2E_API_URL ?? 'http://localhost:8080/api').replace(
  /\/$/,
  '',
);
const ENABLED = process.env.E2E_ACCESS_GATES_LIVE === '1';
const PASSWORD = 'e2e-gate-pass-1';
const ALLOWED = 'e2e_gate_allowed';
const WRITER = 'e2e_gate_writer';

async function tokenFor(
  request: APIRequestContext,
  username: string,
): Promise<string> {
  const login = await request.post(`${API}/login`, {
    data: { username, password: PASSWORD },
  });
  if (login.ok()) {
    return (await login.json()).access_token as string;
  }
  const register = await request.post(`${API}/register`, {
    data: { username, password: PASSWORD },
  });
  expect(
    register.status(),
    `register ${username}: ${await register.text()}`,
  ).toBe(201);
  return (await register.json()).access_token as string;
}

async function loginViaUi(page: Page, username: string): Promise<void> {
  await page.goto('/');
  const authDialog = page
    .getByRole('dialog')
    .filter({ hasText: 'Login or Register' });
  await expect(authDialog).toBeVisible();
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password').fill(PASSWORD);
  await page.getByRole('button', { name: 'Login', exact: true }).click();
  await expect(authDialog).toHaveCount(0);
}

test.describe('access gates on a gated stack @live', () => {
  test.skip(
    !ENABLED,
    'set E2E_ACCESS_GATES_LIVE=1 and start a side stack with REGISTRATION_ENABLED=true REGISTRATION_ALLOWLIST=e2e_gate_allowed,e2e_gate_writer AI_ALLOWLIST=e2e_gate_allowed',
  );

  test.beforeAll(async ({ request }) => {
    await tokenFor(request, ALLOWED);
    await tokenFor(request, WRITER);
  });

  test('anonymous visitors see invite-only registration', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('dialog')).toBeVisible();

    await page.getByRole('button', { name: 'Register', exact: true }).click();
    await expect(page.getByTestId('registration-invite-hint')).toHaveText(
      'Registration is invite-only.',
    );
  });

  test('an unlisted username is rejected through the form', async ({ page }) => {
    await page.goto('/');
    await page.getByRole('button', { name: 'Register', exact: true }).click();
    await page.getByLabel('Username').fill(`e2e_gate_denied_${Date.now()}`);
    await page.getByLabel('Password').fill(PASSWORD);
    await page.getByRole('button', { name: 'Register', exact: true }).click();

    await expect(page.getByRole('alert')).toHaveText(
      'Registration is not available.',
    );
  });

  test('the API enforces registration and AI allowlists', async ({ request }) => {
    const allowedToken = await tokenFor(request, ALLOWED);
    const writerToken = await tokenFor(request, WRITER);

    const allowedFeatures = await request.get(`${API}/features`, {
      headers: { Authorization: `Bearer ${allowedToken}` },
    });
    expect(allowedFeatures.status()).toBe(200);
    expect((await allowedFeatures.json()).chat_allowed).toBe(true);

    const writerFeatures = await request.get(`${API}/features`, {
      headers: { Authorization: `Bearer ${writerToken}` },
    });
    expect(writerFeatures.status()).toBe(200);
    expect((await writerFeatures.json()).chat_allowed).toBe(false);

    const deniedChat = await request.post(`${API}/chat/`, {
      headers: { Authorization: `Bearer ${writerToken}` },
      data: { message: 'hello' },
    });
    expect(deniedChat.status()).toBe(403);

    const deniedConfig = await request.get(`${API}/me/llm-config`, {
      headers: { Authorization: `Bearer ${writerToken}` },
    });
    expect(deniedConfig.status()).toBe(403);

    const publicConfig = await request.get(`${API}/config`);
    expect(publicConfig.status()).toBe(200);
    expect((await publicConfig.json()).registration_mode).toBe('invite_only');
  });

  test('a non-listed user loses every AI surface in the SPA', async ({ page }) => {
    await loginViaUi(page, WRITER);

    await expect(
      page.locator('button[aria-label="Open AI Agent"]'),
    ).toHaveCount(0);
    await expect(page.getByTestId('chat-panel')).toHaveCount(0);
    await expect(page.getByTestId('llm-config-button')).toHaveCount(0);
  });

  test('a listed user keeps the chat entry point', async ({ page }) => {
    const featuresResponse = page.waitForResponse(
      (resp) => resp.url().includes('/api/features') && resp.status() === 200,
    );
    await loginViaUi(page, ALLOWED);

    expect((await (await featuresResponse).json()).chat_allowed).toBe(true);
    await expect(
      page.locator('button[aria-label="Open AI Agent"]'),
    ).toBeVisible();
  });
});
