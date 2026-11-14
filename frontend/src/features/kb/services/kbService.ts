import api from '~/services/api';
import type {
  KbDocument,
  KbDocumentFilters,
  KbIndexReport,
  KbSettings,
  KbSource,
} from '../types/kb';

const KB_SILENT_OPTIONS = { skipErrorToast: true, skipSentry: true };

export const kbService = {
  async listSources(): Promise<KbSource[]> {
    const response = await api.get('/kb/sources', KB_SILENT_OPTIONS);
    return response.data.sources ?? [];
  },

  async createSource(name: string, rootPath: string): Promise<KbSource> {
    const response = await api.post('/kb/sources', {
      name,
      root_path: rootPath,
    });
    return response.data;
  },

  async updateSource(id: number, enabled: boolean): Promise<KbSource> {
    const response = await api.patch(`/kb/sources/${id}`, { enabled });
    return response.data;
  },

  async deleteSource(id: number): Promise<void> {
    await api.delete(`/kb/sources/${id}`);
  },

  async listDocuments(filters: KbDocumentFilters = {}): Promise<KbDocument[]> {
    const params = new URLSearchParams();
    if (filters.sourceId) params.set('source_id', String(filters.sourceId));
    if (filters.origin) params.set('origin', filters.origin);
    if (filters.status) params.set('status', filters.status);
    if (filters.limit) params.set('limit', String(filters.limit));
    if (filters.offset) params.set('offset', String(filters.offset));
    const query = params.toString();
    const response = await api.get(`/kb/documents${query ? `?${query}` : ''}`, KB_SILENT_OPTIONS);
    return response.data.documents ?? [];
  },

  async getDocument(id: number): Promise<KbDocument> {
    const response = await api.get(`/kb/documents/${id}`, KB_SILENT_OPTIONS);
    return response.data;
  },

  async createTextDocument(name: string, content: string): Promise<KbDocument> {
    const response = await api.post(
      '/kb/documents',
      { name, content },
      KB_SILENT_OPTIONS,
    );
    return response.data;
  },

  async uploadDocument(file: File): Promise<KbDocument> {
    const form = new FormData();
    form.append('file', file, file.name);
    const response = await api.post('/kb/documents', form, {
      headers: { 'Content-Type': 'multipart/form-data' },
      ...KB_SILENT_OPTIONS,
    });
    return response.data;
  },

  async updateDocument(id: number, content: string): Promise<KbDocument> {
    const response = await api.put(`/kb/documents/${id}`, { content });
    return response.data;
  },

  async deleteDocument(id: number): Promise<void> {
    await api.delete(`/kb/documents/${id}`);
  },

  async indexSources(): Promise<KbIndexReport> {
    const response = await api.post('/kb/index');
    return response.data;
  },

  async getSettings(): Promise<KbSettings> {
    const response = await api.get('/kb/settings', KB_SILENT_OPTIONS);
    return response.data;
  },
};
