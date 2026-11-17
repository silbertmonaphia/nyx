import { expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat } from './helpers/llm-config';

const REPO_ROOT = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../..',
);
const STACK_SUFFIX = process.env.E2E_STACK_SUFFIX ?? '';
const AGENT_PIDFILE = `/tmp/nyx-e2e-agent${STACK_SUFFIX}.pid`;
const AGENT_METRICS = `http://127.0.0.1:${process.env.E2E_AGENT_METRICS_PORT ?? '9101'}/metrics`;

function killAgent(): void {
  if (!existsSync(AGENT_PIDFILE)) {
    throw new Error(`agent pidfile missing: ${AGENT_PIDFILE}`);
  }
  const pid = Number(readFileSync(AGENT_PIDFILE, 'utf8').trim());
  if (!Number.isInteger(pid) || pid <= 0) {
    throw new Error(`invalid agent pid in ${AGENT_PIDFILE}`);
  }
  const cmdlinePath = `/proc/${pid}/cmdline`;
  if (!existsSync(cmdlinePath)) {
    throw new Error(`refusing to kill pid ${pid}: /proc entry is gone`);
  }
  const cmdline = readFileSync(cmdlinePath, 'utf8');
  if (!cmdline.includes('agent.main')) {
    const executable = cmdline.split('\0')[0] ?? '';
    throw new Error(
      `refusing to kill pid ${pid}: not the side agent (${executable})`,
    );
  }
  process.kill(pid, 'SIGKILL');
}

function restartAgent(): Promise<void> {
  return new Promise((resolve, reject) => {
    const child = spawn(
      'bash',
      [path.join(REPO_ROOT, 'scripts/e2e-side-stack.sh'), 'restart-agent'],
      { env: process.env, stdio: 'ignore' },
    );
    child.on('error', reject);
    child.on('exit', (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(`restart-agent exited with code ${code}`));
    });
  });
}

function metricValue(text: string, tool: string): number {
  for (const line of text.split('\n')) {
    if (!line.startsWith('agent_tool_calls_total{')) continue;
    if (!line.includes(`tool="${tool}"`)) continue;
    if (!line.includes('ok="true"')) continue;
    const value = Number(line.split(' ').pop());
    if (!Number.isNaN(value)) return value;
  }
  return 0;
}

test.describe('Per-step checkpoint resume (live stack)', { tag: '@live' }, () => {
  test('a mid-run agent crash re-runs only the in-flight step', async ({
    page,
    request,
  }) => {
    test.setTimeout(240_000);

    await registerAndOpenChat(page, { prefix: 'e2e_step_resume' });

    let releaseRecovery: () => void = () => {};
    const agentHealthy = new Promise<void>((resolve) => {
      releaseRecovery = resolve;
    });
    let chatPosts = 0;
    await page.route('**/api/chat', async (route) => {
      if (route.request().method() !== 'POST') {
        await route.fallback();
        return;
      }
      chatPosts += 1;
      if (chatPosts > 1) {
        await agentHealthy;
      }
      await route.fallback();
    });

    await page
      .getByTestId('chat-input')
      .fill(
        'Use the list_feeds tool to list my feeds, then summarize what you found in at least three sentences.',
      );
    await page.getByTestId('chat-send').click();

    const toolRow = page
      .getByTestId('chat-agent-step')
      .filter({ hasText: 'list_feeds' })
      .first();
    await expect(toolRow).toBeVisible({ timeout: 120_000 });
    await expect(toolRow).toHaveAttribute('data-step-status', 'ok', {
      timeout: 120_000,
    });

    await page.waitForTimeout(500);
    killAgent();
    await restartAgent();
    releaseRecovery();

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 120_000,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);
    await expect(
      page.getByTestId('chat-agent-step').first(),
    ).toBeVisible({ timeout: 30_000 });

    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).not.toHaveText(/^\s*$/, { timeout: 60_000 });

    expect(chatPosts).toBeGreaterThan(1);

    const metrics = await request.get(AGENT_METRICS);
    expect(metrics.ok()).toBeTruthy();
    expect(metricValue(await metrics.text(), 'list_feeds')).toBe(0);
  });
});
