import { beforeEach, describe, expect, it, vi } from 'vitest';
import api from '~/services/api';
import { kbService } from './kbService';

vi.mock('~/services/api', () => ({
  default: {
    get: vi.fn(),
    post: vi.fn(),
    put: vi.fn(),
    patch: vi.fn(),
    delete: vi.fn(),
  },
}));

const mockedApi = api as unknown as {
  get: ReturnType<typeof vi.fn>;
  post: ReturnType<typeof vi.fn>;
  put: ReturnType<typeof vi.fn>;
  patch: ReturnType<typeof vi.fn>;
  delete: ReturnType<typeof vi.fn>;
};

describe('kbService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('lists sources from the envelope', async () => {
    mockedApi.get.mockResolvedValue({ data: { sources: [{ id: 1, name: 'shared' }] } });
    const sources = await kbService.listSources();
    expect(mockedApi.get).toHaveBeenCalledWith('/kb/sources', {
      skipErrorToast: true,
      skipSentry: true,
    });
    expect(sources).toEqual([{ id: 1, name: 'shared' }]);
  });

  it('creates a private source', async () => {
    mockedApi.post.mockResolvedValue({ data: { id: 2 } });
    await kbService.createSource('mine', '/srv/kb/users/1');
    expect(mockedApi.post).toHaveBeenCalledWith('/kb/sources', {
      name: 'mine',
      root_path: '/srv/kb/users/1',
    });
  });

  it('passes document filters as query parameters', async () => {
    mockedApi.get.mockResolvedValue({ data: { documents: [] } });
    await kbService.listDocuments({ sourceId: 3, origin: 'inline', status: 'indexed', limit: 25, offset: 50 });
    expect(mockedApi.get).toHaveBeenCalledWith(
      '/kb/documents?source_id=3&origin=inline&status=indexed&limit=25&offset=50',
      { skipErrorToast: true, skipSentry: true },
    );
  });

  it('uploads multipart form data without writing a JSON body', async () => {
    mockedApi.post.mockResolvedValue({ data: { id: 4 } });
    const file = new File(['# hi'], 'note.md', { type: 'text/markdown' });
    await kbService.uploadDocument(file);
    const [url, body, config] = mockedApi.post.mock.calls[0];
    expect(url).toBe('/kb/documents');
    expect(body).toBeInstanceOf(FormData);
    expect((body as FormData).get('file')).toBeInstanceOf(File);
    expect(config).toEqual({ headers: { 'Content-Type': 'multipart/form-data' } });
  });

  it('creates, updates and deletes documents', async () => {
    mockedApi.post.mockResolvedValue({ data: { id: 5 } });
    mockedApi.put.mockResolvedValue({ data: { id: 5, content: 'new' } });
    mockedApi.delete.mockResolvedValue({ data: {} });

    await kbService.createTextDocument('typed', '# typed');
    expect(mockedApi.post).toHaveBeenCalledWith('/kb/documents', {
      name: 'typed',
      content: '# typed',
    });

    await kbService.updateDocument(5, 'new');
    expect(mockedApi.put).toHaveBeenCalledWith('/kb/documents/5', { content: 'new' });

    await kbService.deleteDocument(5);
    expect(mockedApi.delete).toHaveBeenCalledWith('/kb/documents/5');
  });

  it('triggers indexing and reads settings', async () => {
    mockedApi.post.mockResolvedValue({ data: { sources: [] } });
    mockedApi.get.mockResolvedValue({ data: { enabled: true, private_base: '/srv', embedding_model: 'm' } });

    expect(await kbService.indexSources()).toEqual({ sources: [] });
    expect(mockedApi.post).toHaveBeenCalledWith('/kb/index');
    expect(await kbService.getSettings()).toEqual({
      enabled: true,
      private_base: '/srv',
      embedding_model: 'm',
    });
    expect(mockedApi.get).toHaveBeenCalledWith('/kb/settings', {
      skipErrorToast: true,
      skipSentry: true,
    });
  });
});
