import React, { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ArrowLeft, Loader2, Save, Trash2 } from 'lucide-react';
import { Button } from '~/components/ui/Button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '~/components/ui/Dialog';
import { Textarea } from '~/components/ui/Textarea';
import { useUiStore } from '~/store/uiStore';
import { useKbDocument, useKbMutations } from '../hooks/useKb';

export const DocumentView: React.FC<{ id: number; onBack: () => void }> = ({
  id,
  onBack,
}) => {
  const { t } = useTranslation();
  const documentQuery = useKbDocument(id);
  const mutations = useKbMutations();
  const addToast = useUiStore((state) => state.addToast);
  const [draft, setDraft] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState(false);

  if (documentQuery.isLoading) {
    return (
      <p className="text-sm text-muted-foreground" data-testid="kb-document-loading">
        {t('kb.loading')}
      </p>
    );
  }
  if (documentQuery.isError || !documentQuery.data) {
    return (
      <div className="space-y-2">
        <p className="text-sm text-destructive" data-testid="kb-document-error" role="alert">
          {t('kb.error')}
        </p>
        <Button type="button" variant="outline" onClick={onBack}>
          {t('kb.back')}
        </Button>
      </div>
    );
  }

  const document = documentQuery.data;
  const fileBacked = document.origin === 'file';
  const content = draft ?? document.content ?? '';

  const save = async () => {
    try {
      await mutations.updateDocument.mutateAsync({ id, content });
      addToast(t('kb.saveDone'), 'success');
    } catch {
      addToast(t('kb.saveFailed'), 'error');
    }
  };

  const remove = async () => {
    try {
      await mutations.deleteDocument.mutateAsync(id);
      addToast(t('kb.deleteDone'), 'success');
      onBack();
    } catch {
      addToast(t('kb.deleteFailed'), 'error');
    } finally {
      setPendingDelete(false);
    }
  };

  return (
    <div className="w-full max-w-3xl mx-auto space-y-3" data-testid="kb-document-view">
      <div className="flex items-center gap-2">
        <Button
          type="button"
          size="icon"
          variant="ghost"
          aria-label={t('kb.back')}
          onClick={onBack}
          data-testid="kb-document-back"
        >
          <ArrowLeft className="h-4 w-4" />
        </Button>
        <h2 className="text-lg font-semibold flex-1 truncate">
          {document.title || document.rel_path}
        </h2>
        <span className="text-[10px] uppercase tracking-wide rounded bg-muted px-1.5 py-0.5">
          {t(`kb.origin.${document.origin}`)}
        </span>
        <span className="text-[10px] uppercase tracking-wide rounded bg-muted px-1.5 py-0.5">
          {t(`kb.status.${document.status}`)}
        </span>
      </div>

      <p className="text-xs text-muted-foreground" data-testid="kb-document-path">
        {document.rel_path}
      </p>

      {fileBacked ? (
        <>
          <p
            className="rounded-md border bg-muted/40 p-3 text-sm text-muted-foreground"
            data-testid="kb-document-file-note"
          >
            {t('kb.fileBackedNote')}
          </p>
          {document.headings && document.headings.length > 0 && (
            <ul className="space-y-1 text-sm" data-testid="kb-document-headings">
              {document.headings.map((path, index) => (
                <li key={index} className="text-muted-foreground">
                  {path.join(' / ')}
                </li>
              ))}
            </ul>
          )}
          <div className="flex gap-2">
            <Button type="button" variant="outline" disabled data-testid="kb-document-save">
              <Save className="h-4 w-4" />
              {t('kb.save')}
            </Button>
            <Button
              type="button"
              variant="destructive"
              disabled
              data-testid="kb-document-delete"
            >
              <Trash2 className="h-4 w-4" />
              {t('kb.delete')}
            </Button>
          </div>
        </>
      ) : (
        <>
          <Textarea
            value={content}
            onChange={(event) => setDraft(event.target.value)}
            aria-label={t('kb.contentLabel')}
            rows={16}
            data-testid="kb-document-content"
          />
          <div className="flex gap-2 justify-end">
            <Button
              type="button"
              variant="outline"
              onClick={() => void save()}
              disabled={mutations.updateDocument.isPending}
              data-testid="kb-document-save"
            >
              {mutations.updateDocument.isPending ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Save className="h-4 w-4" />
              )}
              {t('kb.save')}
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={() => setPendingDelete(true)}
              disabled={mutations.deleteDocument.isPending}
              data-testid="kb-document-delete"
            >
              <Trash2 className="h-4 w-4" />
              {t('kb.delete')}
            </Button>
          </div>
        </>
      )}

      <Dialog open={pendingDelete} onOpenChange={setPendingDelete}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t('kb.confirm.title')}</DialogTitle>
            <DialogDescription>
              {t('kb.confirm.description', {
                name: document.title || document.rel_path,
              })}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setPendingDelete(false)}>
              {t('common.cancel')}
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={() => void remove()}
              data-testid="kb-document-confirm-delete"
            >
              {t('kb.confirm.confirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
};
