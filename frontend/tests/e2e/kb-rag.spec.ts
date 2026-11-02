import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat as registerAndOpenChatShared } from './helpers/llm-config';

async function register(page: Page): Promise<void> {
  await registerAndOpenChatShared(page, { prefix: 'e2e_kb' });
}

async function openKnowledgeBase(page: Page): Promise<void> {
  await page.keyboard.press('Escape');
  await page.getByTestId('view-tab-knowledge').click();
  await expect(page.getByTestId('kb-page')).toBeVisible();
}

async function openChat(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Open AI Agent' }).click();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

async function sendMessage(page: Page, text: string): Promise<void> {
  await page.getByTestId('chat-input').fill(text);
  await page.getByTestId('chat-send').click();
}

async function waitForStreamEnd(page: Page): Promise<void> {
  await expect(page.getByTestId('chat-message-assistant').last()).not.toHaveText(
    /^[\s…]*$/u,
    { timeout: 180_000 },
  );
  await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
    timeout: 180_000,
  });
  await expect(page.getByTestId('chat-error')).toHaveCount(0);
}

async function uploadFixture(page: Page, name: string, body: string): Promise<void> {
  await page.getByTestId('kb-file-input').setInputFiles({
    name,
    mimeType: 'text/markdown',
    buffer: Buffer.from(body),
  });
  await expect(page.getByTestId('kb-document-row').filter({ hasText: name }).first()).toBeVisible({
    timeout: 30_000,
  });
}

test.describe('Knowledge base retrieval (live)', { tag: '@live' }, () => {
  test.beforeEach(async ({ page }) => {
    await register(page);
  });

  test('a chat question cites an uploaded knowledge document', async ({ page }) => {
    test.setTimeout(300_000);
    const marker = `KB-E2E-MARKER-${Date.now()}`;
    const content = `# 知识库校验\n\n## 周末记录\n\n本次记录的标签是 ${marker}，周末活动是去海边捡贝壳。\n`;
    await openKnowledgeBase(page);
    await uploadFixture(page, `kb-fixture-${Date.now()}.md`, content);

    await openChat(page);
    await sendMessage(
      page,
      `我的知识库里这篇记录写了什么周末活动？回答时必须包含记录标签，并引用文档的相对路径。`,
    );
    await waitForStreamEnd(page);

    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).toContainText(marker, { timeout: 30_000 });
    await expect(assistant).toContainText('.md', { timeout: 30_000 });
    const kbStep = page.locator(
      '[data-testid="chat-agent-step"][data-step-kind="kb"]',
    );
    await expect(kbStep.last()).toHaveAttribute('data-step-status', 'ok', {
      timeout: 10_000,
    });
  });

  test('an inline document is retrievable after an edit, and gone after deletion', async ({
    page,
  }) => {
    test.setTimeout(420_000);
    const markerV1 = `KB-EDIT-V1-${Date.now()}`;
    const markerV2 = `KB-EDIT-V2-${Date.now()}`;

    await openKnowledgeBase(page);
    await page.getByTestId('kb-new-document').click();
    await page.getByTestId('kb-new-name').fill('kb-edit.md');
    await page.getByTestId('kb-new-content').fill(`# 编辑验证\n\n记录标签：${markerV1}\n`);
    await page.getByTestId('kb-new-save').click();
    await expect(
      page.getByTestId('kb-document-row').filter({ hasText: 'kb-edit.md' }),
    ).toBeVisible({ timeout: 30_000 });

    const row = page
      .getByTestId('kb-document-row')
      .filter({ hasText: 'kb-edit.md' });
    await row.getByTestId('kb-document-open').click();
    const editor = page.getByTestId('kb-document-content');
    await editor.fill(`# 编辑验证\n\n记录标签：${markerV2}\n`);
    await page.getByTestId('kb-document-save').click();
    await expect(editor).toHaveValue(new RegExp(markerV2), { timeout: 30_000 });
    await page.getByTestId('kb-document-back').click();

    await openChat(page);
    await sendMessage(page, '我的知识库里“编辑验证”这篇文档的记录标签是什么？');
    await waitForStreamEnd(page);
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText(markerV2, { timeout: 30_000 });

    await openKnowledgeBase(page);
    await page
      .getByTestId('kb-document-row')
      .filter({ hasText: 'kb-edit.md' })
      .getByTestId('kb-document-open')
      .click();
    await page.getByTestId('kb-document-delete').click();
    await page.getByTestId('kb-document-confirm-delete').click();
    await expect(page.getByTestId('kb-document-view')).toHaveCount(0, {
      timeout: 30_000,
    });
    await expect(page.getByText('kb-edit.md')).toHaveCount(0);

    const kbStep = page.locator(
      '[data-testid="chat-agent-step"][data-step-kind="kb"]',
    );
    await openChat(page);
    await page.getByTestId('chat-new-session').click();
    await expect(page.getByTestId('chat-message-assistant')).toHaveCount(0);
    await expect(kbStep).toHaveCount(0);
    await sendMessage(page, '我的知识库里“编辑验证”这篇文档的记录标签是什么？');
    await waitForStreamEnd(page);
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).not.toContainText(markerV2, { timeout: 30_000 });
    await expect(kbStep).not.toHaveCount(0, { timeout: 10_000 });
    await expect(kbStep.last()).toHaveAttribute('data-step-status', 'ok', {
      timeout: 10_000,
    });
  });
});
