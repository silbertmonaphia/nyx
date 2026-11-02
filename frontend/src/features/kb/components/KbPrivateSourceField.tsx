import React, { useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { AxiosError } from 'axios';
import { Button } from '~/components/ui/Button';
import { Input } from '~/components/ui/Input';
import { useUiStore } from '~/store/uiStore';
import { useKbMutations, useKbSettings } from '../hooks/useKb';
import type { ApiError } from '~/api/openapi';

export const KbPrivateSourceField: React.FC = () => {
  const { t } = useTranslation();
  const settingsQuery = useKbSettings();
  const mutations = useKbMutations();
  const addToast = useUiStore((state) => state.addToast);
  const [rootPath, setRootPath] = useState<string | null>(null);
  const [error, setError] = useState('');
  const value = rootPath ?? settingsQuery.data?.private_base ?? '';

  if (settingsQuery.isError || (settingsQuery.data && !settingsQuery.data.enabled)) {
    return null;
  }

  const save = async () => {
    setError('');
    try {
      await mutations.createSource.mutateAsync({
        name: 'private',
        rootPath: value.trim(),
      });
      addToast(t('kb.privateSaved'), 'success');
    } catch (err) {
      const axiosError = err as AxiosError<ApiError>;
      setError(axiosError.response?.data?.error ?? t('kb.privateFailed'));
    }
  };

  return (
    <section className="space-y-2 rounded-md border p-3" data-testid="kb-private-source">
      <h3 className="text-sm font-medium">{t('kb.privateTitle')}</h3>
      <p className="text-xs text-muted-foreground">
        {t('kb.privateHint', {
          base: settingsQuery.data?.private_base ?? '',
        })}
      </p>
      <Input
        value={value}
        onChange={(event) => setRootPath(event.target.value)}
        placeholder={settingsQuery.data?.private_base ?? ''}
        maxLength={1024}
        data-testid="kb-private-root"
      />
      {error && (
        <p className="text-xs text-destructive" role="alert" data-testid="kb-private-error">
          {error}
        </p>
      )}
      <div className="flex justify-end">
        <Button
          type="button"
          variant="outline"
          onClick={() => void save()}
          disabled={!value.trim() || mutations.createSource.isPending}
          data-testid="kb-private-save"
        >
          {t('kb.privateSave')}
        </Button>
      </div>
    </section>
  );
};
