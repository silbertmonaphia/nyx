import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import {
  closeChatPanel,
  makeTestUser,
  registerViaUi,
} from './helpers/llm-config';

async function dismissBlockingDialog(page: Page): Promise<void> {
  const dialog = page.getByRole('dialog');
  for (let attempt = 0; attempt < 3; attempt++) {
    if ((await dialog.count()) === 0) return;
    await page.keyboard.press('Escape');
    await page.waitForTimeout(300);
  }
  await expect(dialog).toHaveCount(0);
}

async function registerAndOpenKb(page: Page): Promise<void> {
  await registerViaUi(page, makeTestUser('e2e_kb_upload'));
  await dismissBlockingDialog(page);
  await closeChatPanel(page);
  await page.getByTestId('view-tab-knowledge').click();
  await expect(page.getByTestId('kb-page')).toBeVisible();
}

test.describe('KB upload constraints (agent-free)', () => {
  test('shows the upload constraint hint', async ({ page }) => {
    await registerAndOpenKb(page);
    const hint = page.getByTestId('kb-upload-hint');
    await expect(hint).toBeVisible();
    await expect(hint).toContainText('UTF-8');
    await expect(hint).toContainText('1 MiB');
  });

  test('rejects binary content without a network upload', async ({ page }) => {
    await registerAndOpenKb(page);
    const uploadRequest = page
      .waitForRequest(
        (request) =>
          request.method() === 'POST' &&
          new URL(request.url()).pathname.endsWith('/kb/documents'),
        { timeout: 3000 },
      )
      .then(() => 'sent')
      .catch(() => 'no-request');
    await page.getByTestId('kb-file-input').setInputFiles({
      name: 'binary.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from([0xff, 0xfe, 0x00, 0x01]),
    });
    await expect(
      page.getByTestId('toast').filter({ hasText: 'UTF-8' }),
    ).toBeVisible();
    expect(await uploadRequest).toBe('no-request');
  });

  test('rejects oversized files and accepts a valid text file', async ({ page }) => {
    await registerAndOpenKb(page);
    await page.getByTestId('kb-file-input').setInputFiles({
      name: 'big.md',
      mimeType: 'text/markdown',
      buffer: Buffer.alloc((1 << 20) + 1, 0x61),
    });
    await expect(
      page.getByTestId('toast').filter({ hasText: 'too large' }),
    ).toBeVisible();

    await page.getByTestId('kb-file-input').setInputFiles({
      name: 'note.md',
      mimeType: 'text/markdown',
      buffer: Buffer.from('# side stack upload\n\nhello'),
    });
    await expect(
      page.getByTestId('toast').filter({ hasText: 'uploaded' }),
    ).toBeVisible();
    await expect(
      page.getByTestId('kb-document-row').filter({ hasText: 'note.md' }).first(),
    ).toBeVisible();
  });
});
