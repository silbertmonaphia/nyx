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
    } else {
      expect(await status.textContent()).toContain('Authentication failed');
      expect(await status.textContent()).toContain('401');
      await page.getByTestId('llm-probe-embedding-toggle').click();
      const disclosure = page.getByTestId('llm-probe-embedding-request');
      await expect(disclosure).toContainText('Bearer ••••');
      await expect(disclosure).not.toContainText(WRONG_KEY);
      await expect(disclosure).not.toContainText(RIGHT_KEY);
    }

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
    } else {
      expect(await status.textContent()).toContain('Connected');
      expect(await status.textContent()).toContain('1024 dims');
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
});
