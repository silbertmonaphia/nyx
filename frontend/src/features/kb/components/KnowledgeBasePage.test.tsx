import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { KnowledgeBasePage } from './KnowledgeBasePage';
import { kbService } from '../services/kbService';
import { useAuthStore } from '~/store/authStore';
import { useUiStore } from '~/store/uiStore';
import { TestProviders } from '~/test/test-utils';
import type { KbDocument, KbSource } from '../types/kb';

vi.mock('../services/kbService');

const mockedService = kbService as unknown as {
  [K in keyof typeof kbService]: ReturnType<typeof vi.fn>;
};

const source: KbSource = {
  id: 1,
  name: 'posts',
  kind: 'dir',
  enabled: true,
  last_scan_at: '2026-11-01T00:00:00Z',
  document_count: 2,
  chunk_count: 5,
  pending_count: 0,
  directories: ['B-Cook'],
  embedding_model: 'model-a',
  indexed_models: ['model-a'],
};

const document: KbDocument = {
  id: 10,
  source_id: 1,
  origin: 'inline',
  rel_path: 'notes.md',
  title: 'Notes',
  doc_type: 'markdown',
  byte_size: 20,
  status: 'indexed',
  generation: 1,
  mtime: null,
  updated_at: '2026-11-01T00:00:00Z',
};

const setupAuth = () => {
  useAuthStore.setState({
    user: {
      id: 1,
      username: 'alice',
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    },
    isAuthenticated: true,
  });
};

const renderPage = () =>
  render(
    <TestProviders>
      <KnowledgeBasePage />
    </TestProviders>,
  );

describe('KnowledgeBasePage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useUiStore.setState({ toasts: [] });
    setupAuth();
    mockedService.listSources.mockResolvedValue([]);
    mockedService.listDocuments.mockResolvedValue([]);
    mockedService.getSettings.mockResolvedValue({
      enabled: true,
      embedding_model: 'model-a',
    });
  });

  it('renders the empty state', async () => {
    renderPage();
    expect(await screen.findByTestId('kb-empty')).toBeInTheDocument();
    expect(screen.getByTestId('kb-documents-empty')).toBeInTheDocument();
  });

  it('renders a loading state', () => {
    mockedService.listSources.mockReturnValue(new Promise(() => {}));
    mockedService.listDocuments.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(screen.getByTestId('kb-loading')).toBeInTheDocument();
  });

  it('renders an error state', async () => {
    mockedService.listSources.mockRejectedValue(new Error('nope'));
    renderPage();
    expect(await screen.findByTestId('kb-error')).toBeInTheDocument();
  });

  it('renders sources and documents with counts and status', async () => {
    mockedService.listSources.mockResolvedValue([source]);
    mockedService.listDocuments.mockResolvedValue([document]);
    renderPage();

    expect(await screen.findByText('posts')).toBeInTheDocument();
    expect(screen.getByText(/2 documents · 5 chunks/)).toBeInTheDocument();
    expect(await screen.findByText('Notes')).toBeInTheDocument();
    expect(screen.getByTestId('kb-document-status')).toHaveTextContent('Indexed');
  });

  it('shows how many documents are loaded of the total', async () => {
    mockedService.listSources.mockResolvedValue([
      { ...source, document_count: 3 },
    ]);
    mockedService.listDocuments.mockResolvedValue([
      document,
      { ...document, id: 11, rel_path: 'more.md', title: 'More' },
    ]);
    renderPage();

    expect(await screen.findByTestId('kb-document-count')).toHaveTextContent(
      '2 of 3 documents loaded',
    );
  });

  it('shows the plain document count when everything is loaded', async () => {
    mockedService.listSources.mockResolvedValue([source]);
    mockedService.listDocuments.mockResolvedValue([
      document,
      { ...document, id: 11, rel_path: 'more.md', title: 'More' },
    ]);
    renderPage();

    expect(await screen.findByTestId('kb-document-count')).toHaveTextContent(
      '2 documents',
    );
  });

  it('uses the selected source total when filtering', async () => {
    mockedService.listSources.mockResolvedValue([
      source,
      { ...source, id: 2, name: 'other', document_count: 5 },
    ]);
    mockedService.listDocuments.mockResolvedValue([document]);
    renderPage();

    await screen.findByText('other');
    await userEvent.click(screen.getAllByTestId('kb-source-open')[1]);
    expect(await screen.findByTestId('kb-document-count')).toHaveTextContent(
      '1 of 5 documents loaded',
    );
  });

  it('labels a disabled source and its documents as excluded from search', async () => {
    mockedService.listSources.mockResolvedValue([{ ...source, enabled: false }]);
    mockedService.listDocuments.mockResolvedValue([document]);
    renderPage();

    expect(await screen.findByTestId('kb-source-disabled-hint')).toHaveTextContent(
      "won't search this source",
    );
    expect(screen.getByTestId('kb-source-toggle')).toHaveTextContent(
      'Include in search',
    );
    expect(screen.getByTestId('kb-document-source-disabled')).toHaveTextContent(
      `${source.name} · excluded from search`,
    );
  });

  it('explains an embedding identity mismatch', async () => {
    mockedService.listSources.mockResolvedValue([
      { ...source, indexed_models: ['model-b'] },
    ]);
    renderPage();
    expect(await screen.findByTestId('kb-identity-mismatch')).toBeInTheDocument();
  });

  it('confirms before deleting a source', async () => {
    mockedService.listSources.mockResolvedValue([source]);
    mockedService.deleteSource.mockResolvedValue(undefined);
    renderPage();

    await userEvent.click(await screen.findByTestId('kb-source-delete'));
    expect(mockedService.deleteSource).not.toHaveBeenCalled();
    await userEvent.click(screen.getByTestId('kb-confirm-delete'));
    await waitFor(() =>
      expect(mockedService.deleteSource).toHaveBeenCalledWith(1),
    );
  });

  it('confirms before deleting an inline document', async () => {
    mockedService.listSources.mockResolvedValue([source]);
    mockedService.listDocuments.mockResolvedValue([document]);
    mockedService.deleteDocument.mockResolvedValue(undefined);
    renderPage();

    await screen.findByText('Notes');
    await userEvent.click(screen.getByTestId('kb-document-delete'));
    await userEvent.click(screen.getByTestId('kb-confirm-delete'));
    await waitFor(() =>
      expect(mockedService.deleteDocument).toHaveBeenCalledWith(10),
    );
  });

  it('uploads a file', async () => {
    mockedService.uploadDocument.mockResolvedValue(document);
    renderPage();

    const file = new File(['# hi'], 'note.md', { type: 'text/markdown' });
    await userEvent.upload(screen.getByTestId('kb-file-input'), file);
    await waitFor(() =>
      expect(mockedService.uploadDocument).toHaveBeenCalledWith(file),
    );
  });

  it('offers enable/disable and delete for a directory source', async () => {
    mockedService.listSources.mockResolvedValue([source]);
    renderPage();
    expect(await screen.findByTestId('kb-source-toggle')).toBeTruthy();
    expect(screen.getByTestId('kb-source-delete')).toBeTruthy();
  });

  it('rejects an oversized upload before calling the API', async () => {
    renderPage();
    const file = new File([new Uint8Array((1 << 20) + 1)], 'big.md', {
      type: 'text/markdown',
    });
    await userEvent.upload(screen.getByTestId('kb-file-input'), file);
    expect(mockedService.uploadDocument).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(
        useUiStore.getState().toasts.some((toast) =>
          toast.message.includes('too large'),
        ),
      ).toBe(true),
    );
  });

  it('rejects a binary upload before calling the API', async () => {
    renderPage();
    const file = new File([new Uint8Array([0xff, 0xfe, 0x00, 0x01])], 'blob.md', {
      type: 'text/markdown',
    });
    await userEvent.upload(screen.getByTestId('kb-file-input'), file);
    expect(mockedService.uploadDocument).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(
        useUiStore.getState().toasts.some((toast) =>
          toast.message.includes('UTF-8'),
        ),
      ).toBe(true),
    );
  });

  it('rejects invalid UTF-8 without a NUL byte before calling the API', async () => {
    renderPage();
    const file = new File([new Uint8Array([0xc3, 0x28, 0x61])], 'broken.md', {
      type: 'text/markdown',
    });
    await userEvent.upload(screen.getByTestId('kb-file-input'), file);
    expect(mockedService.uploadDocument).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(
        useUiStore.getState().toasts.some((toast) =>
          toast.message.includes('UTF-8'),
        ),
      ).toBe(true),
    );
  });

  it('rejects ASCII-magic binaries such as a renamed PDF before calling the API', async () => {
    renderPage();
    const file = new File(['%PDF-1.7 fake pdf body'], 'renamed.pdf.md', {
      type: 'text/markdown',
    });
    await userEvent.upload(screen.getByTestId('kb-file-input'), file);
    expect(mockedService.uploadDocument).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(
        useUiStore.getState().toasts.some((toast) =>
          toast.message.includes('UTF-8'),
        ),
      ).toBe(true),
    );
  });

  it('rejects an empty file before calling the API', async () => {
    renderPage();
    const file = new File([], 'empty.md', { type: 'text/markdown' });
    await userEvent.upload(screen.getByTestId('kb-file-input'), file);
    expect(mockedService.uploadDocument).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(
        useUiStore.getState().toasts.some((toast) =>
          toast.message.includes('empty'),
        ),
      ).toBe(true),
    );
  });

  it('shows the upload constraints', () => {
    renderPage();
    expect(screen.getByTestId('kb-upload-hint')).toHaveTextContent('1 MiB');
  });

  it('renders the toolbar action icons at the same size', () => {
    renderPage();
    const uploadIcon = screen.getByTestId('kb-upload').querySelector('svg');
    const newDocumentIcon = screen
      .getByTestId('kb-new-document')
      .querySelector('svg');
    expect(uploadIcon).toHaveClass('lucide-upload', 'h-4', 'w-4');
    expect(newDocumentIcon).toHaveClass('lucide-file-plus', 'h-4', 'w-4');
  });

  it('creates a typed document', async () => {
    mockedService.createTextDocument.mockResolvedValue(document);
    renderPage();

    await userEvent.click(screen.getByTestId('kb-new-document'));
    await userEvent.type(screen.getByTestId('kb-new-name'), 'typed');
    await userEvent.type(screen.getByTestId('kb-new-content'), '# typed body');
    await userEvent.click(screen.getByTestId('kb-new-save'));
    await waitFor(() =>
      expect(mockedService.createTextDocument).toHaveBeenCalledWith(
        'typed',
        '# typed body',
      ),
    );
  });
});
