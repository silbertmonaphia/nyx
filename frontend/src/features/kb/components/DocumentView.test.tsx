import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { DocumentView } from './DocumentView';
import { kbService } from '../services/kbService';
import { useAuthStore } from '~/store/authStore';
import { TestProviders } from '~/test/test-utils';
import type { KbDocument } from '../types/kb';

vi.mock('../services/kbService');

const mockedService = kbService as unknown as {
  [K in keyof typeof kbService]: ReturnType<typeof vi.fn>;
};

const inlineDocument: KbDocument = {
  id: 5,
  source_id: 1,
  origin: 'inline',
  rel_path: 'notes.md',
  title: 'Notes',
  doc_type: 'markdown',
  byte_size: 10,
  status: 'indexed',
  generation: 1,
  mtime: null,
  updated_at: '2026-11-01T00:00:00Z',
  content: '# Notes\n\nbody',
  headings: [['Notes']],
};

const fileDocument: KbDocument = {
  ...inlineDocument,
  id: 6,
  origin: 'file',
  rel_path: 'B-Cook/diet.md',
  content: null,
  headings: [['饮食'], ['饮食', '碳水']],
};

const renderView = (id: number, onBack = vi.fn()) =>
  render(
    <TestProviders>
      <DocumentView id={id} onBack={onBack} />
    </TestProviders>,
  );

describe('DocumentView', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useAuthStore.setState({
      user: {
        id: 1,
        username: 'alice',
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
      },
      isAuthenticated: true,
    });
  });

  it('saves an inline document in place', async () => {
    mockedService.getDocument.mockResolvedValue(inlineDocument);
    mockedService.updateDocument.mockResolvedValue(inlineDocument);
    renderView(5);

    const editor = await screen.findByTestId('kb-document-content');
    expect(screen.getByLabelText('Document content')).toBe(editor);
    await userEvent.clear(editor);
    await userEvent.type(editor, '# Notes\n\nnew body');
    await userEvent.click(screen.getByTestId('kb-document-save'));
    await waitFor(() =>
      expect(mockedService.updateDocument).toHaveBeenCalledWith(
        5,
        '# Notes\n\nnew body',
      ),
    );
  });

  it('deletes an inline document and returns to the list', async () => {
    mockedService.getDocument.mockResolvedValue(inlineDocument);
    mockedService.deleteDocument.mockResolvedValue(undefined);
    const onBack = vi.fn();
    renderView(5, onBack);

    await userEvent.click(await screen.findByTestId('kb-document-delete'));
    expect(mockedService.deleteDocument).not.toHaveBeenCalled();
    await userEvent.click(screen.getByTestId('kb-document-confirm-delete'));
    await waitFor(() => expect(mockedService.deleteDocument).toHaveBeenCalledWith(5));
    expect(onBack).toHaveBeenCalled();
  });

  it('disables edit and delete for a file-backed document', async () => {
    mockedService.getDocument.mockResolvedValue(fileDocument);
    renderView(6);

    expect(await screen.findByTestId('kb-document-file-note')).toBeInTheDocument();
    expect(screen.queryByTestId('kb-document-content')).not.toBeInTheDocument();
    expect(screen.getByTestId('kb-document-save')).toBeDisabled();
    expect(screen.getByTestId('kb-document-delete')).toBeDisabled();
    expect(screen.getByTestId('kb-document-headings')).toHaveTextContent('饮食 / 碳水');
  });
});
