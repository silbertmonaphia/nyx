import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { KbPrivateSourceField } from './KbPrivateSourceField';
import { kbService } from '../services/kbService';
import { useAuthStore } from '~/store/authStore';
import { TestProviders } from '~/test/test-utils';

vi.mock('../services/kbService');

const mockedService = kbService as unknown as {
  [K in keyof typeof kbService]: ReturnType<typeof vi.fn>;
};

describe('KbPrivateSourceField', () => {
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
    mockedService.getSettings.mockResolvedValue({
      enabled: true,
      private_base: '/srv/kb/users/1',
      embedding_model: 'model-a',
    });
  });

  it('prefills the operator base and saves a private directory', async () => {
    mockedService.createSource.mockResolvedValue({ id: 1 });
    render(
      <TestProviders>
        <KbPrivateSourceField />
      </TestProviders>,
    );

    const input = await screen.findByTestId('kb-private-root');
    await waitFor(() => expect(input).toHaveValue('/srv/kb/users/1'));
    await userEvent.clear(input);
    await userEvent.type(input, '/srv/kb/users/1/notes');
    await userEvent.click(screen.getByTestId('kb-private-save'));

    await waitFor(() =>
      expect(mockedService.createSource).toHaveBeenCalledWith(
        'private',
        '/srv/kb/users/1/notes',
      ),
    );
  });

  it('surfaces the server refusal verbatim', async () => {
    mockedService.createSource.mockRejectedValue({
      response: { data: { error: 'Invalid knowledge source directory' } },
    });
    render(
      <TestProviders>
        <KbPrivateSourceField />
      </TestProviders>,
    );

    await screen.findByTestId('kb-private-root');
    await userEvent.click(screen.getByTestId('kb-private-save'));
    expect(await screen.findByTestId('kb-private-error')).toHaveTextContent(
      'Invalid knowledge source directory',
    );
  });
});
