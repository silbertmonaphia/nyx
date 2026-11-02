export type KbScope = 'shared' | 'user';
export type KbKind = 'dir' | 'inline';
export type KbOrigin = 'file' | 'inline';
export type KbStatus = 'pending' | 'indexed' | 'failed';

export interface KbSource {
  id: number;
  name: string;
  scope: KbScope;
  kind: KbKind;
  enabled: boolean;
  last_scan_at: string | null;
  last_error?: string;
  document_count: number;
  chunk_count: number;
  pending_count: number;
  directories: string[];
  embedding_model?: string;
  indexed_models: string[];
}

export interface KbDocument {
  id: number;
  source_id: number;
  origin: KbOrigin;
  rel_path: string;
  title: string;
  doc_type: string;
  byte_size: number;
  status: KbStatus;
  error_detail?: string;
  generation: number;
  mtime: string | null;
  updated_at: string;
  content?: string | null;
  headings?: string[][];
}

export interface KbSettings {
  enabled: boolean;
  private_base: string;
  embedding_model: string;
}

export interface KbScanSkip {
  rel_path: string;
  reason: string;
}

export interface KbSourceScan {
  source_id: number;
  name: string;
  documents: number;
  chunks: number;
  skipped: KbScanSkip[];
  error?: string;
}

export interface KbIndexReport {
  sources: KbSourceScan[];
}

export interface KbDocumentFilters {
  sourceId?: number;
  origin?: KbOrigin;
  status?: KbStatus;
  limit?: number;
  offset?: number;
}
