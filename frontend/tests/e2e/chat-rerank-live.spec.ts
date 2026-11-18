import { createServer, type Server } from 'node:http';
import { AddressInfo } from 'node:net';
import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import {
  API_URL,
  readAccessToken,
  registerAndOpenChat,
  seedLlmConfig,
} from './helpers/llm-config';

interface RerankStub {
  url: string;
  requests: number;
  close: () => Promise<void>;
}

async function startRerankStub(marker: string): Promise<RerankStub> {
  const state = { requests: 0 };
  const server: Server = createServer((req, res) => {
    if (req.method !== 'POST' || req.url !== '/rerank') {
      res.writeHead(404).end();
      return;
    }
    let raw = '';
    req.on('data', (chunk) => {
      raw += chunk;
    });
    req.on('end', () => {
      state.requests++;
      let body: { model?: string; documents?: string[] } = {};
      try {
        body = JSON.parse(raw) as { model?: string; documents?: string[] };
      } catch {
        res.writeHead(400).end();
        return;
      }
      const documents = body.documents ?? [];
      const results = documents
        .map((text, index) => ({
          index,
          document: { text },
          relevance_score: text.includes(marker) ? 1 : 0,
        }))
        .sort((a, b) => b.relevance_score - a.relevance_score);
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(
        JSON.stringify({
          id: 'rerank-stub',
          model: body.model ?? 'stub-reranker',
          usage: { prompt_tokens: 1, total_tokens: 1 },
          results,
        }),
      );
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = (server.address() as AddressInfo).port;
  let closed = false;
  return {
    url: `http://127.0.0.1:${port}`,
    get requests() {
      return state.requests;
    },
    close: () =>
      new Promise<void>((resolve) => {
        if (closed) {
          resolve();
          return;
        }
        closed = true;
        server.close(() => resolve());
      }),
  } as RerankStub;
}

async function sendMessage(page: Page, text: string): Promise<void> {
  await page.getByTestId('chat-input').fill(text);
  await page.getByTestId('chat-send').click();
}

async function waitForAnswer(page: Page): Promise<string> {
  await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
    timeout: 180_000,
  });
  const assistant = page.getByTestId('chat-message-assistant').last();
  await expect(assistant).not.toHaveText(/^\s*$/, { timeout: 30_000 });
  return (await assistant.innerText()).trim();
}

test.describe('Chat / RAG rerank (browser-level)', { tag: '@live' }, () => {
  test('a BYOK rerank group promotes a passage the dense leg leaves out', async ({
    page,
  }) => {
    test.setTimeout(300_000);

    const marker = `ZQ-${Date.now()}-VERIFY`;
    const stub = await startRerankStub(marker);
    try {
      await registerAndOpenChat(page, { prefix: 'e2e_rerank' });
      const token = await readAccessToken(page);
      const auth = { Authorization: `Bearer ${token}` };

      for (let i = 1; i <= 6; i++) {
        const response = await page.request.post(`${API_URL}/feeds`, {
          headers: auth,
          data: {
            title: `校验口令占位笔记 ${i}`,
            description: `这是一篇关于校验口令的记录，但这里并没有设置任何真正的口令。占位编号 ${i}。`,
          },
        });
        expect(response.ok(), `seed distractor feed ${i}`).toBeTruthy();
      }

      const markerFeed = await page.request.post(`${API_URL}/feeds`, {
        headers: auth,
        data: {
          title: '晚餐与食材备忘',
          description:
            '今天晚饭准备做番茄牛腩，需要提前解冻牛肉，搭配米饭和一份清炒时蔬。' +
            '购物清单包括番茄、牛腩、洋葱、姜和少量香料。' +
            '做菜之前先把食材按顺序整理好，避免中途遗漏。' +
            `本文记录的校验口令是${marker}。` +
            '整体流程以小火慢炖为主，收汁后撒上葱花即可。',
        },
      });
      expect(markerFeed.ok(), 'seed marker feed').toBeTruthy();

      await page.waitForTimeout(3000);

      const feedsToggle = page.getByTestId('chat-feeds-toggle');
      if ((await feedsToggle.count()) > 0) {
        await feedsToggle.check();
      }

      const question = '我的笔记里提到的校验口令是什么？只回答那个口令本身。';
      await sendMessage(page, question);
      const withoutRerank = await waitForAnswer(page);
      expect(withoutRerank).not.toContain(marker);

      await seedLlmConfig(page, {
        rerank: {
          base_url: stub.url,
          api_key: '',
          model: 'stub-reranker-v1',
        },
      });

      await sendMessage(page, question);
      const withRerank = await waitForAnswer(page);
      expect(withRerank).toContain(marker);
      expect(stub.requests).toBeGreaterThan(0);
      await expect(page.getByTestId('chat-error')).toHaveCount(0);

      await stub.close();
      await sendMessage(page, question);
      const fallback = await waitForAnswer(page);
      expect(fallback).not.toContain(marker);
      await expect(page.getByTestId('chat-error')).toHaveCount(0);
    } finally {
      await stub.close();
    }
  });
});
