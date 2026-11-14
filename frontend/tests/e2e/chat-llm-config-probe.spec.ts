import { expect } from '@playwright/test';
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import { test } from './fixtures/e2e-fixtures';
import { makeTestUser, registerViaUi } from './helpers/llm-config';

const GATE_TITLE = 'LLM configuration required';

const PRIVATE_DENIED = process.env.E2E_BYOK_PRIVATE_DENIED !== 'false';

const WRONG_KEY = 'sk-e2e-probe-wrong';
const RIGHT_KEY = 'sk-e2e-probe-right-abcd';

interface StubHandle {
  url: string;
  requests: { auth: string; bodies: string[] }[];
  close: () => Promise<void>;
}

async function startEmbedStub(
  respond: (auth: string) => { status: number; body: unknown },
): Promise<StubHandle> {
  const requests: { auth: string; bodies: string[] }[] = [];
  const server = http.createServer((req, res) => {
    let raw = '';
    req.on('data', (chunk) => {
      raw += chunk;
    });
    req.on('end', () => {
      const auth = req.headers.authorization ?? '';
      requests.push({ auth, bodies: [raw] });
      const out = respond(auth);
      res.writeHead(out.status, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(out.body));
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    url: `http://127.0.0.1:${port}/v1`,
    requests,
    close: () =>
      new Promise<void>((resolve, reject) =>
        server.close((err) => (err ? reject(err) : resolve())),
      ),
  };
}

function embedOK(): { status: number; body: unknown } {
  return {
    status: 200,
    body: {
      object: 'list',
      data: [
        {
          object: 'embedding',
          index: 0,
          embedding: Array.from({ length: 1024 }, () => 0.01),
        },
      ],
      model: 'e2e-probe-model',
      usage: { prompt_tokens: 1, total_tokens: 1 },
    },
  };
}

async function startStallingChatStub(): Promise<StubHandle> {
  const requests: { auth: string; bodies: string[] }[] = [];
  const server = http.createServer((req, res) => {
    let raw = '';
    req.on('data', (chunk) => {
      raw += chunk;
    });
    req.on('end', () => {
      requests.push({ auth: req.headers.authorization ?? '', bodies: [raw] });
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      res.flushHeaders();
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    url: `http://127.0.0.1:${port}/v1`,
    requests,
    close: () =>
      new Promise<void>((resolve, reject) => {
        server.closeAllConnections();
        server.close((err) => (err ? reject(err) : resolve()));
      }),
  };
}

test.describe('BYOK connectivity probe (browser-level)', () => {
  test('a wrong key renders an inline credential failure, saves nothing', async ({
    page,
  }) => {
    const stub = await startEmbedStub((auth) =>
      auth === `Bearer ${RIGHT_KEY}`
        ? embedOK()
        : {
            status: 401,
            body: { error: { message: 'invalid api key', type: 'auth' } },
          },
    );
    const user = makeTestUser('e2e_llmprobe_wrong');
    await registerViaUi(page, user);
    const gate = page.getByRole('dialog', { name: GATE_TITLE });
    await expect(gate).toBeVisible();

    const putRequests: string[] = [];
    page.on('request', (req) => {
      if (req.method() === 'PUT' && req.url().includes('/me/llm-config')) {
        putRequests.push(req.url());
      }
    });

    await page.getByTestId('llm-embedding-toggle').click();
    await page.locator('#embedding-base-url').fill(stub.url);
    await page.locator('#embedding-model').fill('e2e-probe-model');
    await page.locator('#embedding-api-key').fill(WRONG_KEY);
    await page.getByTestId('llm-probe-embedding-button').click();

    const status = page.getByTestId('llm-probe-embedding-status');
    await expect(status).toBeVisible({ timeout: 15_000 });

    if (PRIVATE_DENIED) {
      expect(await status.textContent()).toContain('Unreachable');
      await expect(page.getByTestId('llm-probe-embedding-request')).toHaveCount(0);
      expect(stub.requests).toHaveLength(0);
    } else {
      expect(await status.textContent()).toContain('Authentication failed');
      expect(await status.textContent()).toContain('401');
      await page.getByTestId('llm-probe-embedding-toggle').click();
      const disclosure = page.getByTestId('llm-probe-embedding-request');
      await expect(disclosure).toContainText('Bearer ••••');
      await expect(disclosure).not.toContainText(WRONG_KEY);
      await expect(disclosure).not.toContainText(RIGHT_KEY);
      expect(stub.requests).toHaveLength(1);
      expect(stub.requests[0].auth).toBe(`Bearer ${WRONG_KEY}`);
      const body = JSON.parse(stub.requests[0].bodies[0]) as {
        model?: string;
        input?: string[];
      };
      expect(body.model).toBe('e2e-probe-model');
      expect(body.input).toHaveLength(1);
    }

    await page.getByTestId('llm-probe-chat-button').click();
    await expect(page.getByTestId('llm-probe-chat-status')).toContainText(
      'Empty section',
      { timeout: 15_000 },
    );
    expect(
      stub.requests.length,
      'probing an empty section must not dial any upstream',
    ).toBe(PRIVATE_DENIED ? 0 : 1);

    expect(putRequests).toHaveLength(0);
    await page.keyboard.press('Escape');
    await page.getByRole('button', { name: 'Open AI Agent' }).click();
    await expect(page.getByRole('dialog', { name: GATE_TITLE })).toBeVisible();
    await stub.close();
  });

  test('a correct key renders connected with the width detail', async ({
    page,
  }) => {
    const stub = await startEmbedStub((auth) =>
      auth === `Bearer ${RIGHT_KEY}` ? embedOK() : { status: 401, body: {} },
    );
    const user = makeTestUser('e2e_llmprobe_ok');
    await registerViaUi(page, user);
    await expect(
      page.getByRole('dialog', { name: GATE_TITLE }),
    ).toBeVisible();

    await page.getByTestId('llm-embedding-toggle').click();
    await page.locator('#embedding-base-url').fill(stub.url);
    await page.locator('#embedding-model').fill('e2e-probe-model');
    await page.locator('#embedding-api-key').fill(RIGHT_KEY);
    await page.getByTestId('llm-probe-embedding-button').click();

    const status = page.getByTestId('llm-probe-embedding-status');
    await expect(status).toBeVisible({ timeout: 15_000 });

    if (PRIVATE_DENIED) {
      expect(await status.textContent()).toContain('Unreachable');
      expect(stub.requests).toHaveLength(0);
    } else {
      expect(await status.textContent()).toContain('Connected');
      expect(await status.textContent()).toContain('1024 dims');
      expect(stub.requests).toHaveLength(1);
      expect(stub.requests[0].auth).toBe(`Bearer ${RIGHT_KEY}`);
    }
    await stub.close();
  });

  test('an empty section probes as skipped and renders neutral', async ({
    page,
  }) => {
    const user = makeTestUser('e2e_llmprobe_empty');
    await registerViaUi(page, user);
    const gate = page.getByRole('dialog', { name: GATE_TITLE });
    await expect(gate).toBeVisible();

    await page.getByTestId('llm-embedding-toggle').click();
    await page.getByTestId('llm-probe-embedding-button').click();
    const status = page.getByTestId('llm-probe-embedding-status');
    await expect(status).toBeVisible({ timeout: 15_000 });
    expect(await status.textContent()).toContain('Empty section');
  });

  test.describe('clipboard granted', () => {
    test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

    test('a stalled stream after HTTP 200 renders slow_response with a copyable curl', async ({
      page,
    }) => {
      test.setTimeout(90_000);
      const statusBudget = 60_000;
      const stub = await startStallingChatStub();
      const user = makeTestUser('e2e_llmprobe_slow');
      await registerViaUi(page, user);
      const gate = page.getByRole('dialog', { name: GATE_TITLE });
      await expect(gate).toBeVisible();

      await page.locator('#llm-base-url').fill(stub.url);
      await page.locator('#llm-model').fill('e2e-probe-model');
      await page.locator('#llm-api-key').fill(RIGHT_KEY);
      await page.getByTestId('llm-probe-chat-button').click();

      const status = page.getByTestId('llm-probe-chat-status');
      await expect(status).toBeVisible({ timeout: statusBudget });

      if (PRIVATE_DENIED) {
        expect(await status.textContent()).toContain('Unreachable');
        expect(stub.requests).toHaveLength(0);
        await stub.close();
        return;
      }

      expect(await status.textContent()).toContain('Slow response');
      expect(await status.textContent()).toContain('(upstream HTTP 200)');
      expect(stub.requests).toHaveLength(1);
      expect(stub.requests[0].auth).toBe(`Bearer ${RIGHT_KEY}`);

      await page.getByTestId('llm-probe-chat-toggle').click();
      await expect(page.getByTestId('llm-probe-chat-request')).toContainText(
        'Bearer ••••',
      );
      await page.getByTestId('llm-probe-chat-copy').click();
      await expect(page.getByTestId('llm-probe-chat-copy')).toHaveText(
        /Copied/,
        { timeout: 15_000 },
      );
      const curl = await page.evaluate(() =>
        navigator.clipboard.readText(),
      );
      expect(curl).toContain(`curl -X POST '${stub.url}/chat/completions'`);
      expect(curl).toContain('Bearer <your API key>');
      expect(curl).not.toContain(RIGHT_KEY);
      await stub.close();
    });
  });
});
