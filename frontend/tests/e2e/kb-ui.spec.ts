import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import {
  closeChatPanel,
  makeTestUser,
  registerViaUi,
} from './helpers/llm-config';

const source = {
  id: 1,
  name: 'inline',
  scope: 'user',
  kind: 'inline',
  enabled: true,
  last_scan_at: null,
  document_count: 1,
  chunk_count: 1,
  pending_count: 0,
  directories: [],
  embedding_model: 'mock-embedding',
  indexed_models: ['mock-embedding'],
};

const document = {
  id: 10,
  source_id: 1,
  origin: 'inline',
  rel_path: 'notes.md',
  title: 'Notes',
  doc_type: 'markdown',
  byte_size: 10,
  status: 'indexed',
  generation: 1,
  mtime: null,
  updated_at: '2026-01-01T00:00:00Z',
  content: '# Notes\n\nbody',
  headings: [['Notes']],
};

interface KbMockOptions {
  sources?: unknown[];
  documents?: unknown[];
  sourcesStatus?: number;
  settingsStatus?: number;
}

async function mockKb(page: Page, options: KbMockOptions = {}): Promise<void> {
  const sources = options.sources ?? [source];
  const documents = options.documents ?? [document];
  const sourcesStatus = options.sourcesStatus ?? 200;
  const settingsStatus = options.settingsStatus ?? 200;
  await page.route('**/api/kb/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    if (path.endsWith('/kb/settings')) {
      await route.fulfill({
        status: settingsStatus,
        contentType: 'application/json',
        body: JSON.stringify({
          enabled: true,
          private_base: '/srv/kb/users/1',
          embedding_model: 'mock-embedding',
        }),
      });
      return;
    }
    if (path.endsWith('/kb/sources')) {
      await route.fulfill({
        status: sourcesStatus,
        contentType: 'application/json',
        body: JSON.stringify(sourcesStatus === 200 ? { sources } : { error: 'down' }),
      });
      return;
    }
    if (path.endsWith('/kb/documents')) {
      if (request.method() === 'POST') {
        await route.fulfill({
          status: 201,
          contentType: 'application/json',
          body: JSON.stringify(document),
        });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ documents }),
      });
      return;
    }
    if (path.endsWith('/kb/documents/10')) {
      if (request.method() === 'PUT') {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify(document),
        });
        return;
      }
      if (request.method() === 'DELETE') {
        await route.fulfill({ status: 204, body: '' });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(document),
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ sources: [], documents: [] }),
    });
  });
}

async function openKb(page: Page): Promise<void> {
  await closeChatPanel(page);
  await page.getByTestId('view-tab-knowledge').click();
  await expect(page.getByTestId('kb-page')).toBeVisible();
}

test.describe('Knowledge base UI (agent-free)', () => {
  test.beforeEach(async ({ page }) => {
    await registerViaUi(page, makeTestUser('e2e_kb_ui'));
  });

  test('shows the empty knowledge base', async ({ page }) => {
    await mockKb(page, { sources: [], documents: [] });
    await openKb(page);
    await expect(page.getByTestId('kb-empty')).toBeVisible();
    await expect(page.getByTestId('kb-documents-empty')).toBeVisible();
  });

  test('shows the sources error without a global error toast', async ({ page }) => {
    await mockKb(page, { sourcesStatus: 500 });
    await openKb(page);
    await expect(page.getByTestId('kb-error')).toBeVisible();
    await expect(
      page.getByTestId('toast').filter({ hasText: /Server error|500/ }),
    ).toHaveCount(0);
  });

  test('creates, edits and deletes an inline document', async ({ page }) => {
    await mockKb(page);
    await openKb(page);

    await page.getByTestId('kb-new-document').click();
    await page.getByTestId('kb-new-name').fill('ui-note.md');
    await page.getByTestId('kb-new-content').fill('# UI\n\nfirst body');
    await page.getByTestId('kb-new-save').click();
    await expect(
      page.getByTestId('toast').filter({ hasText: 'Document created' }),
    ).toBeVisible();

    await page.getByTestId('kb-document-open').click();
    const editor = page.getByTestId('kb-document-content');
    await expect(editor).toHaveValue(/# Notes/);
    await editor.fill('# Notes\n\nsecond body');
    await page.getByTestId('kb-document-save').click();
    await expect(
      page.getByTestId('toast').filter({ hasText: /saved/i }),
    ).toBeVisible();

    await page.getByTestId('kb-document-delete').click();
    await page.getByTestId('kb-document-confirm-delete').click();
    await expect(page.getByTestId('kb-document-view')).toHaveCount(0);
  });
});
