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
      if (path.endsWith('/chat/sessions')) {
        return route.fulfill({ json: sessionPage });
      }
      return route.fulfill({ status: 404, json: { detail: 'stubbed' } });
    },
  );
}

test('defaults to Chinese for a browser whose language is English', async ({
  page,
}) => {
  await stubApi(page);
  await page.goto('/');

  expect(await page.evaluate(() => navigator.language)).toBe('en-US');
  await expect(page.getByPlaceholder('搜索私人动态…')).toBeVisible();
  await expect(
    page.getByRole('button', { name: '登录 / 注册' }),
  ).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh');
  await expect(page).toHaveTitle('Nyx — 你的极简私人动态指南');
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

  await page.getByTestId('locale-en').click();
  await expect(
    page.getByRole('button', { name: 'Login / Register' }),
  ).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'en');
  expect(
    await page.evaluate(
      (key) => window.localStorage.getItem(key),
      LOCALE_STORAGE_KEY,
    ),
  ).toBe('en');

  await page.getByTestId('locale-zh').click();
  await expect(
    page.getByRole('button', { name: '登录 / 注册' }),
  ).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh');
});

test('keeps the chosen locale across a reload', async ({ page }) => {
  await stubApi(page);
  await page.goto('/');

  await page.getByTestId('locale-en').click();
  await expect(
    page.getByRole('button', { name: 'Login / Register' }),
  ).toBeVisible();

  await page.reload();

  await expect(
    page.getByRole('button', { name: 'Login / Register' }),
  ).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('lang', 'en');
  await expect(page).toHaveTitle('Nyx — Your minimalist feed guide');
});

test('renders the login, feed, sidebar, and composer surfaces in Chinese', async ({
  page,
}) => {
  await stubApi(page);
  await page.goto('/');

  await page.getByRole('button', { name: '登录 / 注册' }).click();
  await expect(page.getByRole('heading', { name: '登录或注册' })).toBeVisible();
  await expect(
    page.getByText('登录你的账号，或创建新账号。'),
  ).toBeVisible();

  await page.getByLabel('用户名').fill('zh_user');
  await page.getByLabel('密码').fill('secret123');
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await expect(page.getByText('登录成功！')).toBeVisible();
  await expect(page.getByRole('button', { name: '退出登录' })).toBeVisible();

  await expect(page.getByTestId('feed-list-empty')).toHaveText('暂无私人动态');
  await expect(page.getByTestId('toggle-sort-order')).toHaveAttribute(
    'title',
    '最新优先',
  );
  await expect(page.getByTestId('toggle-sort-order')).toHaveAttribute(
    'aria-label',
    '当前为最新优先；点击切换为最早优先',
  );
  await expect(page.getByRole('button', { name: '添加私人动态' })).toBeVisible();

  await page.getByRole('button', { name: '打开聊天' }).click();
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
});
