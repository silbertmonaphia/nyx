import { expect, test, type Page } from '@playwright/test';

import { LOCALE_STORAGE_KEY } from '../../src/i18n/locale-storage';

test.use({ locale: 'en-US' });

const emptyFeedPage = {
  data: [],
  page: 1,
  page_size: 20,
  total: 0,
  has_more: false,
};

const loginPayload = {
  access_token: 'e2e-access-token',
  refresh_token: 'e2e-refresh-token',
  token_type: 'Bearer',
  expires_at: '2026-10-16T00:00:00Z',
  user: {
    id: 1,
    username: 'zh_user',
    created_at: '2026-10-14T00:00:00Z',
    updated_at: '2026-10-14T00:00:00Z',
  },
};

const sessionPage = {
  sessions: [
    {
      id: 'zh-session',
      user_id: 1,
      title: 'Trip planning',
      started_at: '2026-10-14T00:00:00Z',
      last_active_at: '2026-10-14T00:00:00Z',
      ended_at: null,
      message_count: 2,
    },
  ],
  next_cursor: null,
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

async function stubApi(page: Page) {
  await page.route(
    (url) => url.pathname.startsWith('/api/'),
    (route) => {
      const path = new URL(route.request().url()).pathname;
      if (path.endsWith('/feeds')) {
        return route.fulfill({ json: emptyFeedPage });
      }
      if (path.endsWith('/login')) {
        return route.fulfill({ json: loginPayload });
      }
      if (path.endsWith('/me/llm-config')) {
        return route.fulfill({ json: stubbedLlmConfig });
      }
      if (path.endsWith('/me')) {
        return route.fulfill({ json: loginPayload.user });
      }
      if (path.endsWith('/chat/sessions')) {
        return route.fulfill({ json: sessionPage });
      }
      return route.fulfill({ status: 404, json: { detail: 'stubbed' } });
    },
  );
}

async function closeChatIfOpen(page: Page): Promise<void> {
  if ((await page.getByTestId('chat-form').count()) > 0) {
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).toHaveCount(0);
  }
}

async function loginStubbed(page: Page): Promise<void> {
  await page.getByLabel('用户名').fill('zh_user');
  await page.getByLabel('密码').fill('secret123');
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await expect(page.getByText('登录成功！')).toBeVisible();
  await closeChatIfOpen(page);
}

test('defaults to Chinese for a browser whose language is English', async ({
  page,
}) => {
  await stubApi(page);
  await page.goto('/');

  expect(await page.evaluate(() => navigator.language)).toBe('en-US');
  await expect(page.getByPlaceholder('搜索知识碎片…')).toBeVisible();
  await expect(
    page.getByRole('heading', { name: '登录或注册' }),
  ).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh');
  await expect(page).toHaveTitle('登录 — Nyx');
  expect(
    await page.evaluate(
      (key) => window.localStorage.getItem(key),
      LOCALE_STORAGE_KEY,
    ),
  ).toBeNull();
});

test('applies a locale switch immediately', async ({ page }) => {
  await stubApi(page);
  await page.goto('/');
  await loginStubbed(page);

  await page.getByTestId('locale-en').click();
  await expect(page.getByPlaceholder('Search for feeds...')).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'en');
  expect(
    await page.evaluate(
      (key) => window.localStorage.getItem(key),
      LOCALE_STORAGE_KEY,
    ),
  ).toBe('en');

  await page.getByTestId('locale-zh').click();
  await expect(page.getByPlaceholder('搜索知识碎片…')).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh');
});

test('keeps the chosen locale across a reload', async ({ page }) => {
  await stubApi(page);
  await page.goto('/');
  await loginStubbed(page);

  await page.getByTestId('locale-en').click();
  await expect(page.getByPlaceholder('Search for feeds...')).toBeVisible();

  await page.reload();

  await expect(page.getByTestId('chat-form')).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'en');
  await expect(page).toHaveTitle('Chat — Nyx');
});

test('renders the login, feed, sidebar, and composer surfaces in Chinese', async ({
  page,
}) => {
  await stubApi(page);
  await page.goto('/');

  await expect(page.getByRole('heading', { name: '登录或注册' })).toBeVisible();
  await expect(
    page.getByText('登录你的账号，或创建新账号。'),
  ).toBeVisible();

  await page.getByLabel('用户名').fill('zh_user');
  await page.getByLabel('密码').fill('secret123');
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await expect(page.getByText('登录成功！')).toBeVisible();

  await expect(page.getByRole('heading', { name: '聊天' })).toBeVisible();
  await expect(page.getByPlaceholder('输入消息…')).toBeVisible();
  await expect(page.getByTestId('chat-send')).toHaveText('发送');
  await expect(page.getByTestId('chat-session-title')).toHaveText(
    'Trip planning',
  );
  await expect(page.getByTestId('chat-session-delete')).toHaveAttribute(
    'aria-label',
    '删除 Trip planning',
  );

  await closeChatIfOpen(page);
  await expect(page.getByRole('button', { name: '退出登录' })).toBeVisible();
  await expect(page.getByTestId('feed-list-empty')).toHaveText('暂无知识碎片');
  await expect(page.getByTestId('toggle-sort-order')).toHaveAttribute(
    'title',
    '最新优先',
  );
  await expect(page.getByTestId('toggle-sort-order')).toHaveAttribute(
    'aria-label',
    '当前为最新优先；点击切换为最早优先',
  );
  await expect(page.getByRole('button', { name: '添加知识碎片' })).toBeVisible();
});
