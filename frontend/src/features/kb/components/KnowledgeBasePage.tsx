import React, { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Loader2, RefreshCw, Trash2, Upload } from 'lucide-react';
import { Button } from '~/components/ui/Button';
import { Input } from '~/components/ui/Input';
import { Textarea } from '~/components/ui/Textarea';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '~/components/ui/Dialog';
import { useUiStore } from '~/store/uiStore';
import { useKbDocuments, useKbMutations, useKbSources } from '../hooks/useKb';
import type { KbDocument, KbSource } from '../types/kb';
import { DocumentView } from './DocumentView';

type PendingDelete =
  | { kind: 'source'; id: number; label: string }
  | { kind: 'document'; id: number; label: string };

const newestFirst = (a: KbDocument, b: KbDocument) =>
  b.updated_at.localeCompare(a.updated_at);

const SourceCard: React.FC<{
  source: KbSource;
  selected: boolean;
  onSelect: () => void;
  onDelete: () => void;
  onToggle: () => void;
  toggling: boolean;
}> = ({ source, selected, onSelect, onDelete, onToggle, toggling }) => {
  const { t } = useTranslation();
  const identityMismatch =
    source.chunk_count > 0 &&
    source.indexed_models.length > 0 &&
    !!source.embedding_model &&
    !source.indexed_models.includes(source.embedding_model);

  return (
    <li
      className={`rounded-md border p-3 space-y-2 ${selected ? 'border-primary' : ''}`}
      data-testid="kb-source"
      data-source-id={source.id}
    >
      <div className="flex items-start gap-2">
        <button
          type="button"
          className="text-left flex-1"
          onClick={onSelect}
          data-testid="kb-source-open"
        >
          <div className="flex items-center gap-2">
            <span className="font-medium">{source.name}</span>
            <span className="text-[10px] uppercase tracking-wide rounded bg-muted px-1.5 py-0.5">
              {t(`kb.scope.${source.scope}`)}
            </span>
            <span className="text-[10px] uppercase tracking-wide rounded bg-muted px-1.5 py-0.5">
              {t(`kb.kind.${source.kind}`)}
            </span>
            {!source.enabled && (
              <span className="text-[10px] uppercase tracking-wide rounded bg-muted px-1.5 py-0.5">
                {t('kb.disabled')}
              </span>
            )}
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            {t('kb.sourceCounts', {
              documents: source.document_count,
              chunks: source.chunk_count,
              pending: source.pending_count,
            })}
          </p>
          <p className="text-xs text-muted-foreground">
            {source.last_scan_at
              ? t('kb.lastScan', {
                  time: new Intl.DateTimeFormat().format(
                    new Date(source.last_scan_at),
                  ),
                })
              : t('kb.neverScanned')}
          </p>
        </button>
        {source.scope === 'user' && (
          <>
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={onToggle}
              disabled={toggling}
              data-testid="kb-source-toggle"
            >
              {source.enabled ? t('kb.disable') : t('kb.enable')}
            </Button>
            <Button
              type="button"
              size="icon"
              variant="ghost"
              aria-label={t('kb.deleteSource')}
              onClick={onDelete}
              data-testid="kb-source-delete"
            >
              <Trash2 className="h-4 w-4" />
            </Button>
          </>
        )}
      </div>
      {identityMismatch && (
        <p
          className="text-xs text-amber-700 dark:text-amber-400"
          data-testid="kb-identity-mismatch"
        >
          {t('kb.identityMismatch')}
        </p>
      )}
    </li>
  );
};

const DocumentRow: React.FC<{
  document: KbDocument;
  onOpen: () => void;
  onDelete: () => void;
}> = ({ document, onOpen, onDelete }) => {
  const { t } = useTranslation();
  return (
    <li
      className="flex items-center gap-2 rounded-md border p-2"
      data-testid="kb-document-row"
      data-document-id={document.id}
    >
      <button
        type="button"
        className="flex-1 text-left"
        onClick={onOpen}
        data-testid="kb-document-open"
      >
        <span className="text-sm font-medium">{document.title || document.rel_path}</span>
        <p className="text-xs text-muted-foreground">{document.rel_path}</p>
      </button>
      <span className="text-[10px] uppercase tracking-wide rounded bg-muted px-1.5 py-0.5">
        {t(`kb.origin.${document.origin}`)}
      </span>
      <span
        className="text-[10px] uppercase tracking-wide rounded bg-muted px-1.5 py-0.5"
        data-testid="kb-document-status"
      >
        {t(`kb.status.${document.status}`)}
      </span>
      <Button
        type="button"
        size="icon"
        variant="ghost"
        aria-label={t('kb.deleteDocument')}
        disabled={document.origin === 'file'}
        onClick={onDelete}
        data-testid="kb-document-delete"
      >
        <Trash2 className="h-4 w-4" />
      </Button>
    </li>
  );
};

const KB_MAX_UPLOAD_BYTES = 1 << 20;

const binaryMagicSignatures: number[][] = [
  [0x25, 0x50, 0x44, 0x46],
  [0x47, 0x49, 0x46, 0x38],
  [0x50, 0x4b, 0x03, 0x04],
  [0x52, 0x61, 0x72, 0x21],
  [0x7f, 0x45, 0x4c, 0x46],
  [0x4d, 0x5a],
];

const utf8Decoder = new TextDecoder('utf-8', { fatal: true });

const hasBinaryMagic = (bytes: Uint8Array): boolean =>
  binaryMagicSignatures.some((magic) =>
    magic.every((byte, index) => bytes[index] === byte),
  );

const isTextUpload = async (file: File): Promise<boolean> => {
  try {
    const bytes = new Uint8Array(await file.arrayBuffer());
    if (hasBinaryMagic(bytes)) return false;
    if (bytes.includes(0)) return false;
    utf8Decoder.decode(bytes);
    return true;
  } catch {
    return false;
  }
};

export const KnowledgeBasePage: React.FC = () => {
  const { t } = useTranslation();
  const sourcesQuery = useKbSources();
  const [selectedSource, setSelectedSource] = useState<number | undefined>();
  const documentsQuery = useKbDocuments(
    selectedSource ? { sourceId: selectedSource } : {},
  );
  const [editingDocument, setEditingDocument] = useState<number | null>(null);
  const [pendingDelete, setPendingDelete] = useState<PendingDelete | null>(null);
  const [showNewDocument, setShowNewDocument] = useState(false);
  const [newName, setNewName] = useState('');
  const [newContent, setNewContent] = useState('');
  const fileInput = useRef<HTMLInputElement>(null);
  const addToast = useUiStore((state) => state.addToast);
  const mutations = useKbMutations();

  if (editingDocument !== null) {
    return (
      <DocumentView
        key={editingDocument}
        id={editingDocument}
        onBack={() => setEditingDocument(null)}
      />
    );
  }

  const sources = sourcesQuery.data ?? [];
  const documents = [...(documentsQuery.data ?? [])].sort(newestFirst);

  const runIndex = async () => {
    try {
      const report = await mutations.indexSources.mutateAsync();
      const totals = report.sources.reduce(
        (acc, source) => ({
          documents: acc.documents + source.documents,
          chunks: acc.chunks + source.chunks,
        }),
        { documents: 0, chunks: 0 },
      );
      addToast(
        t('kb.indexDone', {
          documents: totals.documents,
          chunks: totals.chunks,
        }),
        'success',
      );
    } catch {
      addToast(t('kb.indexFailed'), 'error');
    }
  };

  const uploadFile = async (file: File) => {
    if (file.size === 0) {
      addToast(t('kb.uploadEmpty'), 'error');
      return;
    }
    if (file.size > KB_MAX_UPLOAD_BYTES) {
      addToast(t('kb.uploadTooLarge'), 'error');
      return;
    }
    if (!(await isTextUpload(file))) {
      addToast(t('kb.uploadNotText'), 'error');
      return;
    }
    try {
      await mutations.uploadDocument.mutateAsync(file);
      addToast(t('kb.uploadDone'), 'success');
    } catch {
      addToast(t('kb.uploadFailed'), 'error');
    }
  };

  const saveNewDocument = async () => {
    if (!newContent.trim()) return;
    try {
      await mutations.createTextDocument.mutateAsync({
        name: newName.trim() || 'note.md',
        content: newContent,
      });
      setNewName('');
      setNewContent('');
      setShowNewDocument(false);
      addToast(t('kb.createDone'), 'success');
    } catch {
      addToast(t('kb.createFailed'), 'error');
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    try {
      if (pendingDelete.kind === 'source') {
        await mutations.deleteSource.mutateAsync(pendingDelete.id);
      } else {
        await mutations.deleteDocument.mutateAsync(pendingDelete.id);
      }
      addToast(t('kb.deleteDone'), 'success');
    } catch {
      addToast(t('kb.deleteFailed'), 'error');
    } finally {
      setPendingDelete(null);
    }
  };

  return (
    <div className="w-full max-w-3xl mx-auto space-y-4" data-testid="kb-page">
      <div className="flex flex-col items-start gap-2">
        <h2 className="text-lg font-semibold">{t('kb.title')}</h2>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => void runIndex()}
          disabled={mutations.indexSources.isPending}
          data-testid="kb-index"
        >
          {mutations.indexSources.isPending ? (
            <Loader2 className="h-4 w-4 animate-spin" />
          ) : (
            <RefreshCw className="h-4 w-4" />
          )}
          {t('kb.index')}
        </Button>
        <div className="flex flex-col items-start gap-1">
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => fileInput.current?.click()}
            disabled={mutations.uploadDocument.isPending}
            data-testid="kb-upload"
          >
            <Upload className="h-4 w-4" />
            {t('kb.upload')}
          </Button>
          <p className="text-xs text-muted-foreground" data-testid="kb-upload-hint">
            {t('kb.uploadHint')}
          </p>
        </div>
        <input
          ref={fileInput}
          type="file"
          className="hidden"
          accept=".md,.markdown,.txt,.csv,.json,.log,.yaml,.yml,.xml,.py,.go,.js,.ts,.jsx,.tsx,.sql,.sh,.html,.css,.java,.c,.h,.cpp,.rs,text/markdown,text/plain"
          data-testid="kb-file-input"
          onChange={(event) => {
            const file = event.target.files?.[0];
            if (file) void uploadFile(file);
            event.target.value = '';
          }}
        />
        <Button
          type="button"
          size="sm"
          onClick={() => setShowNewDocument((value) => !value)}
          data-testid="kb-new-document"
        >
          {t('kb.newDocument')}
        </Button>
      </div>

      {showNewDocument && (
        <div className="space-y-2 rounded-md border p-3" data-testid="kb-new-form">
          <Input
            value={newName}
            onChange={(event) => setNewName(event.target.value)}
            placeholder={t('kb.namePlaceholder')}
            maxLength={200}
            data-testid="kb-new-name"
          />
          <Textarea
            value={newContent}
            onChange={(event) => setNewContent(event.target.value)}
            placeholder={t('kb.contentPlaceholder')}
            rows={6}
            data-testid="kb-new-content"
          />
          <div className="flex justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              onClick={() => setShowNewDocument(false)}
            >
              {t('common.cancel')}
            </Button>
            <Button
              type="button"
              onClick={() => void saveNewDocument()}
              disabled={!newContent.trim() || mutations.createTextDocument.isPending}
              data-testid="kb-new-save"
            >
              {t('kb.create')}
            </Button>
          </div>
        </div>
      )}

      {sourcesQuery.isLoading && (
        <p className="text-sm text-muted-foreground" data-testid="kb-loading">
          {t('kb.loading')}
        </p>
      )}
      {sourcesQuery.isError && (
        <p className="text-sm text-destructive" data-testid="kb-error" role="alert">
          {t('kb.error')}
        </p>
      )}
      {!sourcesQuery.isLoading && !sourcesQuery.isError && sources.length === 0 && (
        <p className="text-sm text-muted-foreground" data-testid="kb-empty">
          {t('kb.emptySources')}
        </p>
      )}

      {sources.length > 0 && (
        <section className="space-y-2">
          <h3 className="text-sm font-medium">{t('kb.sources')}</h3>
          <ul className="space-y-2">
            {sources.map((source) => (
              <SourceCard
                key={source.id}
                source={source}
                selected={selectedSource === source.id}
                onSelect={() =>
                  setSelectedSource((current) =>
                    current === source.id ? undefined : source.id,
                  )
                }
                onDelete={() =>
                  setPendingDelete({
                    kind: 'source',
                    id: source.id,
                    label: source.name,
                  })
                }
                onToggle={() => {
                  void mutations.updateSource
                    .mutateAsync({
                      id: source.id,
                      enabled: !source.enabled,
                    })
                    .catch(() => {});
                }}
                toggling={mutations.updateSource.isPending}
              />
            ))}
          </ul>
        </section>
      )}

      <section className="space-y-2">
        <h3 className="text-sm font-medium">
          {selectedSource ? t('kb.documentsFiltered') : t('kb.documents')}
        </h3>
        {documentsQuery.isLoading && (
          <p className="text-sm text-muted-foreground" data-testid="kb-documents-loading">
            {t('kb.loading')}
          </p>
        )}
        {documentsQuery.isError && (
          <p className="text-sm text-destructive" data-testid="kb-documents-error" role="alert">
            {t('kb.error')}
          </p>
        )}
        {!documentsQuery.isLoading && !documentsQuery.isError && documents.length === 0 && (
          <p className="text-sm text-muted-foreground" data-testid="kb-documents-empty">
            {t('kb.emptyDocuments')}
          </p>
        )}
        {documents.length > 0 && (
          <ul className="space-y-2">
            {documents.map((document) => (
              <DocumentRow
                key={document.id}
                document={document}
                onOpen={() => setEditingDocument(document.id)}
                onDelete={() =>
                  setPendingDelete({
                    kind: 'document',
                    id: document.id,
                    label: document.title || document.rel_path,
                  })
                }
              />
            ))}
          </ul>
        )}
      </section>

      <Dialog
        open={pendingDelete !== null}
        onOpenChange={(open) => {
          if (!open) setPendingDelete(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t('kb.confirm.title')}</DialogTitle>
            <DialogDescription>
              {pendingDelete
                ? t('kb.confirm.description', { name: pendingDelete.label })
                : ''}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setPendingDelete(null)}
            >
              {t('common.cancel')}
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={() => void confirmDelete()}
              data-testid="kb-confirm-delete"
            >
              {t('kb.confirm.confirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
};
