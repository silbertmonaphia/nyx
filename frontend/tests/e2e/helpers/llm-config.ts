import { expect, type Page } from '@playwright/test';

export const API_URL = process.env.E2E_API_URL ?? 'http://localhost:8080/api';

const TOKEN_STORAGE_KEY = 'nyx-token-store';
const DEFAULT_API_KEY = 'e2e-placeholder-key-not-secret';

export interface TestUser {
  username: string;
  password: string;
}

export interface LlmEndpointSeed {
  base_url: string;
  api_key: string;
  model: string;
}

export interface LlmTavilySeed {
  base_url: string;
  api_key: string;
}

export interface LlmConfigSeedOverrides {
  llm?: Partial<LlmEndpointSeed>;
  embedding?: Partial<LlmEndpointSeed>;
  tavily?: Partial<LlmTavilySeed>;
  fallback?: LlmEndpointSeed | null;
}

function env(name: string, fallback: string): string {
  const value = process.env[name];
  return value !== undefined && value !== '' ? value : fallback;
}

export function makeTestUser(prefix: string): TestUser {
  return {
    username: `${prefix}_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

export async function readAccessToken(page: Page): Promise<string> {
  const token = await page.evaluate((key) => {
    const read = (storage: Storage): string | null => {
      const raw = storage.getItem(key);
      if (!raw) return null;
      try {
        const parsed = JSON.parse(raw) as { accessToken?: unknown };
        return typeof parsed.accessToken === 'string' &&
          parsed.accessToken !== ''
          ? parsed.accessToken
          : null;
      } catch {
        return null;
      }
    };
    return read(window.localStorage) ?? read(window.sessionStorage);
  }, TOKEN_STORAGE_KEY);
  if (!token) {
    throw new Error(
      `No access token under "${TOKEN_STORAGE_KEY}" in localStorage or sessionStorage`,
    );
  }
  return token;
}

export async function closeChatPanel(page: Page): Promise<void> {
  const chatForm = page.getByTestId('chat-form');
  for (let attempt = 0; attempt < 3; attempt++) {
    if ((await chatForm.count()) === 0) return;
    await page.keyboard.press('Escape');
    await page.waitForTimeout(300);
    if ((await chatForm.count()) > 0) {
      await page.mouse.click(20, 400);
      await page.waitForTimeout(300);
    }
  }
  await expect(chatForm).toHaveCount(0);
}

export function llmConfigSeedBody(overrides: LlmConfigSeedOverrides = {}) {
  return {
    llm: {
      base_url: env('E2E_LLM_BASE_URL', 'http://localhost:8000/v1'),
      api_key: env('E2E_LLM_API_KEY', DEFAULT_API_KEY),
      model: env('E2E_LLM_MODEL', 'Qwen/Qwen3-4B-Instruct-2507-FP8'),
      ...overrides.llm,
    },
    embedding: {
      base_url: env('E2E_EMBEDDING_BASE_URL', 'http://localhost:8080/v1'),
      api_key: DEFAULT_API_KEY,
      model: env('E2E_EMBEDDING_MODEL', 'Qwen/Qwen3-Embedding-0.6B'),
      ...overrides.embedding,
    },
    tavily: {
      base_url: env('E2E_TAVILY_BASE_URL', 'https://api.tavily.com/search'),
      api_key: env('E2E_TAVILY_API_KEY', DEFAULT_API_KEY),
      ...overrides.tavily,
    },
    fallback: overrides.fallback ?? null,
  };
}

const RETRYABLE_STATUSES = new Set([429, 500, 502, 503, 504]);
const RETRY_ATTEMPTS = 5;
const RETRY_DELAY_MS = 1500;

export async function seedLlmConfig(
  page: Page,
  overrides: LlmConfigSeedOverrides = {},
): Promise<void> {
  const token = await readAccessToken(page);
  let response = await page.request.put(`${API_URL}/me/llm-config`, {
    headers: { Authorization: `Bearer ${token}` },
    data: llmConfigSeedBody(overrides),
  });
  for (
    let attempt = 0;
    attempt < RETRY_ATTEMPTS && RETRYABLE_STATUSES.has(response.status());
    attempt++
  ) {
    await page.waitForTimeout(RETRY_DELAY_MS);
    response = await page.request.put(`${API_URL}/me/llm-config`, {
      headers: { Authorization: `Bearer ${token}` },
      data: llmConfigSeedBody(overrides),
    });
  }
  const body = await response.text();
  expect(
    response.ok(),
    `PUT ${API_URL}/me/llm-config failed (${response.status()}): ${body}`,
  ).toBeTruthy();
}

export async function clearLlmConfig(page: Page): Promise<void> {
  const token = await readAccessToken(page);
  let response = await page.request.delete(`${API_URL}/me/llm-config`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  for (
    let attempt = 0;
    attempt < RETRY_ATTEMPTS && RETRYABLE_STATUSES.has(response.status());
    attempt++
  ) {
    await page.waitForTimeout(RETRY_DELAY_MS);
    response = await page.request.delete(`${API_URL}/me/llm-config`, {
      headers: { Authorization: `Bearer ${token}` },
    });
  }
  const body = await response.text();
  expect(
    response.ok(),
    `DELETE ${API_URL}/me/llm-config failed (${response.status()}): ${body}`,
  ).toBeTruthy();
}

export async function registerViaUi(page: Page, user: TestUser): Promise<void> {
  await page.goto('/');
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(user.username);
  await page.getByLabel(/Password/i).fill(user.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
}

export async function registerAndOpenChat(
  page: Page,
  opts: { prefix?: string } = {},
): Promise<TestUser> {
  const user = makeTestUser(opts.prefix ?? 'e2e_chat');
  await registerViaUi(page, user);
  await seedLlmConfig(page);
  await page.reload();
  await expect(page.getByTestId('chat-form')).toBeVisible();
  return user;
}
