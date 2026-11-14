import { expect, type Page, type APIRequestContext } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { llmConfigSeedBody, makeTestUser } from './helpers/llm-config';

const GO_PORT = process.env.E2E_GO_PORT ?? '8180';
const API = `http://127.0.0.1:${GO_PORT}/api`;
const MAX_ACCESS_TTL_SECONDS = 120;

function jwtExpSeconds(token: string): number | null {
  const parts = token.split('.');
  if (parts.length !== 3) return null;
  try {
    const payload = JSON.parse(
      Buffer.from(parts[1], 'base64url').toString('utf8'),
    ) as { exp?: unknown };
    return typeof payload.exp === 'number' ? payload.exp : null;
  } catch {
    return null;
  }
}

async function registerViaApi(
  request: APIRequestContext,
): Promise<{ username: string; password: string; accessToken: string }> {
  const user = makeTestUser('e2e_run_token');
  const reg = await request.post(`${API}/register`, { data: user });
  expect(
    reg.status(),
    `register failed: ${reg.status()} ${await reg.text()}`,
  ).toBe(201);
  const { access_token } = (await reg.json()) as { access_token: string };
  return { ...user, accessToken: access_token };
}

async function seedWithRetry(
  request: APIRequestContext,
  accessToken: string,
): Promise<void> {
  const retryable = new Set([429, 500, 502, 503, 504]);
  let lastStatus = 0;
  let lastBody = '';
  for (let attempt = 0; attempt < 5; attempt++) {
    const res = await request.put(`${API}/me/llm-config`, {
      headers: { Authorization: `Bearer ${accessToken}` },
      data: llmConfigSeedBody(),
    });
    if (res.ok()) return;
    lastStatus = res.status();
    lastBody = await res.text();
    if (!retryable.has(lastStatus)) break;
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
  throw new Error(`llm config seed failed: ${lastStatus} ${lastBody}`);
}

async function loginThroughUi(
  page: Page,
  user: { username: string; password: string },
): Promise<void> {
  await page.goto('/');
  await page.getByLabel(/Username/i).fill(user.username);
  await page.getByLabel(/Password/i).fill(user.password);
  await page.getByRole('button', { name: /^Login$/i }).click();
  await expect(page.getByText(/Successfully logged in!/i)).toBeVisible();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

test.describe(
  'Per-run token survives caller access-token expiry (live stack)',
  { tag: '@live' },
  () => {
    test('create_feed lands after the run-start bearer has expired', async ({
      page,
      request,
    }) => {
      test.setTimeout(420_000);

      const user = await registerViaApi(request);
      const accessExp = jwtExpSeconds(user.accessToken);
      const accessTtl =
        accessExp === null ? Number.NaN : accessExp - Date.now() / 1000;
      test.skip(
        !Number.isFinite(accessTtl) ||
          accessTtl <= 0 ||
          accessTtl > MAX_ACCESS_TTL_SECONDS,
        `Requires a short user-access-token TTL: start the side stack with ` +
          `E2E_JWT_ACCESS_TTL=90s (measured JWT_ACCESS_TTL=${Math.round(accessTtl)}s; expected 1-${MAX_ACCESS_TTL_SECONDS}s).`,
      );

      await seedWithRetry(request, user.accessToken);
      await loginThroughUi(page, user);

      let sentAuth: string | undefined;
      page.on('request', (req) => {
        const path = new URL(req.url()).pathname;
        if (
          req.method() === 'POST' &&
          (path === '/api/chat' || path === '/api/chat/')
        ) {
          const header = req.headers()['authorization'];
          if (header) sentAuth = header;
        }
      });

      const feedName = `Run token smoke ${Date.now()}`;
      const prompt = `Use the create_feed tool to add a feed with title "${feedName}" and url https://example.com/feed.xml.`;
      const input = page.getByTestId('chat-input');
      const send = page.getByTestId('chat-send');
      await expect(async () => {
        await input.fill(prompt);
        await expect(input).toHaveValue(prompt);
        await expect(send).toBeEnabled({ timeout: 1_000 });
      }).toPass({ timeout: 30_000 });
      await send.click({ timeout: 30_000 });

      const card = page.getByTestId('chat-confirm-card');
      await expect(card).toBeVisible({ timeout: 120_000 });
      await expect(page.getByTestId('chat-confirm-tool')).toHaveText(
        'create_feed',
      );
      await expect(page.getByTestId('chat-confirm-args')).toContainText(
        feedName,
      );

      if (!sentAuth) {
        throw new Error('POST /api/chat carried no Authorization header');
      }
      const runStartToken = sentAuth.replace(/^Bearer /i, '');
      const runStartExp = jwtExpSeconds(runStartToken);
      expect(
        runStartExp,
        'the run-start bearer must carry an exp claim',
      ).not.toBeNull();
      const runStartExpMs = runStartExp! * 1000;

      const waitMs = runStartExpMs + 5_000 - Date.now();
      if (waitMs > 0) await page.waitForTimeout(waitMs);

      expect(
        Date.now(),
        'the run-start bearer must be expired before the approval click',
      ).toBeGreaterThan(runStartExpMs);
      await expect(card).toBeVisible();
      await expect(card).toHaveAttribute('data-decision', 'pending');
      await expect(page.getByTestId('chat-error')).toHaveCount(0);

      await page.getByTestId('chat-confirm-approve').click();
      await expect(card).toHaveAttribute('data-decision', 'approved', {
        timeout: 30_000,
      });
      await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
        timeout: 120_000,
      });
      await expect(page.getByTestId('chat-error')).toHaveCount(0);

      await page.keyboard.press('Escape');
      await expect(page.getByTestId('chat-form')).not.toBeVisible();
      await page.reload();
      await expect(page.getByTestId('feed-list')).toContainText(feedName, {
        timeout: 15_000,
      });
    });
  },
);
