import { describe, it, expect, beforeEach, vi, type Mock } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LlmConfigModal } from './LlmConfigModal';
import { useLlmConfig } from '../hooks/useLlmConfig';
import { useUiStore } from '~/store/uiStore';
import type { LlmConfigStatus, LlmConfigUpdateInput } from '../types/llmConfig';

vi.mock('../hooks/useLlmConfig');

const mockedUseLlmConfig = useLlmConfig as unknown as ReturnType<typeof vi.fn>;

const unconfigured: LlmConfigStatus = {
  configured: false,
  llm: null,
  embedding: null,
  tavily: null,
  fallback: null,
};

const configuredStatus: LlmConfigStatus = {
  configured: true,
  llm: {
    base_url: 'https://api.deepseek.com/v1',
    model: 'deepseek-chat',
    api_key_masked: '••••abcd',
  },
  embedding: {
    base_url: 'http://localhost:8000/v1',
    model: 'Qwen/Qwen3-Embedding-0.6B',
    api_key_masked: '••••ef12',
  },
  tavily: {
    base_url: 'https://api.tavily.com/search',
    api_key_masked: '••••tvly',
  },
  fallback: null,
};

interface MutationMock {
  mutateAsync: Mock;
  isPending: boolean;
}

interface HookOverrides {
  status?: LlmConfigStatus | null;
  save?: MutationMock;
  remove?: MutationMock;
}

function mockHook(overrides: HookOverrides = {}) {
  const status =
    overrides.status !== undefined ? overrides.status : unconfigured;
  const save: MutationMock =
    overrides.save ??
    { mutateAsync: vi.fn().mockResolvedValue(status), isPending: false };
  const remove: MutationMock =
    overrides.remove ??
    { mutateAsync: vi.fn().mockResolvedValue(undefined), isPending: false };
  mockedUseLlmConfig.mockReturnValue({
    status,
    configured: status ? status.configured : null,
    isLoading: false,
    isError: false,
    refetch: vi.fn(),
    save,
    remove,
  });
  return { save, remove };
}

const input = (id: string) =>
  document.getElementById(id) as HTMLInputElement;

async function expandOptionalSections() {
  if (document.getElementById('embedding-base-url') === null) {
    await userEvent.click(screen.getByTestId('llm-embedding-toggle'));
  }
  if (document.getElementById('tavily-base-url') === null) {
    await userEvent.click(screen.getByTestId('llm-tavily-toggle'));
  }
}

async function fillRequiredFields(fillKeys = true) {
  await expandOptionalSections();
  await userEvent.type(input('llm-base-url'), 'https://api.deepseek.com/v1');
  await userEvent.type(input('llm-model'), 'deepseek-chat');
  if (fillKeys) await userEvent.type(input('llm-api-key'), 'sk-llm');
  await userEvent.type(input('embedding-base-url'), 'http://localhost:8000/v1');
  await userEvent.type(input('embedding-model'), 'Qwen/Qwen3-Embedding-0.6B');
  if (fillKeys) await userEvent.type(input('embedding-api-key'), 'sk-emb');
  await userEvent.type(input('tavily-base-url'), 'https://api.tavily.com/search');
  if (fillKeys) await userEvent.type(input('tavily-api-key'), 'tvly-key');
}

const renderModal = (props: Record<string, unknown> = {}) =>
  render(<LlmConfigModal open onOpenChange={vi.fn()} {...props} />);

describe('LlmConfigModal', () => {
  beforeEach(() => {
    mockedUseLlmConfig.mockReset();
    useUiStore.setState({ toasts: [] });
    document.body.innerHTML = '';
  });

  it('blocks submission on missing required fields and shows per-field errors', async () => {
    const { save } = mockHook({ status: unconfigured });
    renderModal();

    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).not.toHaveBeenCalled();
    });
    expect(
      screen.getByText(/Fill in at least one section/),
    ).toBeInTheDocument();
    expect(useUiStore.getState().toasts).toHaveLength(0);
  });

  it('accepts blank API keys for loopback endpoints', async () => {
    const { save } = mockHook({ status: unconfigured });
    renderModal();

    await userEvent.type(input('llm-base-url'), 'http://localhost:8000/v1');
    await userEvent.type(input('llm-model'), 'local-chat');
    await expandOptionalSections();
    await userEvent.type(input('embedding-base-url'), 'http://127.0.0.1:8001/v1');
    await userEvent.type(input('embedding-model'), 'local-embed');
    await userEvent.type(input('tavily-base-url'), 'http://localhost:8899/search');

    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).toHaveBeenCalledTimes(1);
    });
    expect(save.mutateAsync.mock.calls[0][0]).toMatchObject({
      llm: { base_url: 'http://localhost:8000/v1', api_key: '' },
      embedding: { base_url: 'http://127.0.0.1:8001/v1', api_key: '' },
      tavily: { base_url: 'http://localhost:8899/search', api_key: '' },
    });
  });

  it('saves without an embedding section and shows the red impact note', async () => {
    const { save } = mockHook({ status: unconfigured });
    renderModal();

    await userEvent.type(input('llm-base-url'), 'https://api.deepseek.com/v1');
    await userEvent.type(input('llm-model'), 'deepseek-chat');
    await userEvent.type(input('llm-api-key'), 'sk-llm');

    expect(
      screen.getAllByText(/RAG step and semantic search return no context/).length,
    ).toBeGreaterThan(0);

    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).toHaveBeenCalledTimes(1);
    });
    expect(save.mutateAsync.mock.calls[0][0]).toMatchObject({
      embedding: { base_url: '', api_key: '', model: '' },
    });
  });

  it('saves without a Tavily section and shows the red impact note', async () => {
    const { save } = mockHook({ status: unconfigured });
    renderModal();

    await userEvent.type(input('llm-base-url'), 'https://api.deepseek.com/v1');
    await userEvent.type(input('llm-model'), 'deepseek-chat');
    await userEvent.type(input('llm-api-key'), 'sk-llm');
    await expandOptionalSections();
    await userEvent.type(input('embedding-base-url'), 'https://api.deepseek.com/v1');
    await userEvent.type(input('embedding-model'), 'embed-model');
    await userEvent.type(input('embedding-api-key'), 'sk-emb');

    expect(
      screen.getAllByText(/will not plan web searches/).length,
    ).toBeGreaterThan(0);

    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).toHaveBeenCalledTimes(1);
    });
    expect(save.mutateAsync.mock.calls[0][0]).toMatchObject({
      tavily: { base_url: '', api_key: '' },
    });
  });

  it('rejects a Tavily key without a base URL', async () => {
    const { save } = mockHook({ status: unconfigured });
    renderModal();

    await fillRequiredFields();
    await userEvent.type(input('tavily-api-key'), 'tvly-key');
    await userEvent.clear(input('tavily-base-url'));

    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).not.toHaveBeenCalled();
    });
  });

  it('shows an example placeholder in every empty input when unconfigured', async () => {
    mockHook({ status: unconfigured });
    renderModal();
    await expandOptionalSections();

    expect(input('llm-base-url').placeholder).toBe('https://api.deepseek.com/v1');
    expect(input('llm-model').placeholder).toBe('deepseek-flash');
    expect(input('embedding-base-url').placeholder).toBe('https://api.siliconflow.cn/v1');
    expect(input('embedding-model').placeholder).toBe('Qwen/Qwen3-Embedding-0.6B');
    expect(input('tavily-base-url').placeholder).toBe('https://api.tavily.com/search');
  });

  it('locks the embedding model once configured and explains why', () => {
    mockHook({ status: configuredStatus });
    renderModal();

    expect(input('embedding-model').disabled).toBe(true);
    expect(
      screen.getByText(/model is locked after the first save/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/deletes your stored feed vectors/),
    ).toBeInTheDocument();
  });

  it('keeps the embedding model editable before the first save', async () => {
    mockHook({ status: unconfigured });
    renderModal();
    await expandOptionalSections();
    expect(input('embedding-model').disabled).toBe(false);
  });

  it('renders masked saved-key placeholders with blank key inputs when configured', () => {
    mockHook({ status: configuredStatus });
    renderModal();

    const keep = 'Saved ••••abcd — leave blank to keep';
    expect(input('llm-api-key').placeholder).toBe(keep);
    expect(input('llm-api-key').value).toBe('');
    expect(input('embedding-api-key').placeholder).toBe(
      'Saved ••••ef12 — leave blank to keep',
    );
    expect(input('tavily-api-key').placeholder).toBe(
      'Saved ••••tvly — leave blank to keep',
    );
    expect(input('llm-base-url').value).toBe('https://api.deepseek.com/v1');
    expect(input('llm-model').value).toBe('deepseek-chat');
  });

  it('sends blank key strings (keep-stored) plus prefilled non-key fields on an update', async () => {
    const { save } = mockHook({ status: configuredStatus });
    const onOpenChange = vi.fn();
    const onSaved = vi.fn();
    renderModal({ onOpenChange, onSaved });

    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const payload = save.mutateAsync.mock.calls[0][0] as LlmConfigUpdateInput;
    expect(payload).toEqual({
      llm: {
        base_url: 'https://api.deepseek.com/v1',
        api_key: '',
        model: 'deepseek-chat',
      },
      embedding: {
        base_url: 'http://localhost:8000/v1',
        api_key: '',
        model: 'Qwen/Qwen3-Embedding-0.6B',
      },
      tavily: {
        base_url: 'https://api.tavily.com/search',
        api_key: '',
      },
    });
    expect(onSaved).toHaveBeenCalledTimes(1);
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(
      useUiStore
        .getState()
        .toasts.some((toast) => toast.message === 'LLM configuration saved'),
    ).toBe(true);
  });

  it('shows the failed toast when the save rejects without a response', async () => {
    mockHook({
      status: unconfigured,
      save: {
        mutateAsync: vi.fn().mockRejectedValue(new Error('network down')),
        isPending: false,
      },
    });
    renderModal();

    await fillRequiredFields();
    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(
        useUiStore
          .getState()
          .toasts.some(
            (toast) =>
              toast.message ===
              'Could not update your LLM configuration. Please try again.',
          ),
      ).toBe(true);
    });
  });

  it('shows guidance copy for every section', async () => {
    mockHook({ status: unconfigured });
    renderModal();
    await expandOptionalSections();

    expect(screen.getByText('Main LLM')).toBeInTheDocument();
    expect(
      screen.getByText(/Drives chat replies and the agent's planning steps/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/platform\.deepseek\.com/),
    ).toBeInTheDocument();
    expect(screen.getAllByText(/a trailing \/v1/).length).toBe(2);
    expect(
      screen.getByText(/model must exist on the endpoint/),
    ).toBeInTheDocument();
    expect(screen.getByText(/streaming chat completions/)).toBeInTheDocument();
    expect(
      screen.getByText(/Without tool calls the AI Agent is unavailable/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'DeepSeek API documentation' }),
    ).toHaveAttribute('href', 'https://api-docs.deepseek.com/');
    expect(screen.getByText('Embedding (optional)')).toBeInTheDocument();
    expect(
      screen.getAllByText(/RAG step and semantic search return no context/).length,
    ).toBeGreaterThan(0);
    expect(
      screen.getByText(/may point at a different service than the main LLM/),
    ).toBeInTheDocument();
    expect(screen.getByText(/1024-dimension vectors/)).toBeInTheDocument();
    expect(
      screen.getAllByText(/probed on save/).length,
    ).toBeGreaterThan(0);
    expect(
      screen.getByRole('link', {
        name: 'SiliconFlow API documentation',
      }),
    ).toBeInTheDocument();
    expect(screen.getByText('Tavily search (optional)')).toBeInTheDocument();
    expect(
      screen.getAllByText(/will not plan web searches/).length,
    ).toBeGreaterThan(0);
    expect(screen.getByText(/web_search/)).toBeInTheDocument();
    expect(screen.getByText(/app\.tavily\.com/)).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Tavily search API documentation' }),
    ).toBeInTheDocument();
    expect(screen.getByText(/shaped for the deployment's search provider/)).toBeInTheDocument();
    expect(
      screen.getByText(/The main LLM is optional too/),
    ).toBeInTheDocument();
  });

  it('clears only after the confirm dialog is confirmed', async () => {
    const { remove } = mockHook({ status: configuredStatus });
    const onOpenChange = vi.fn();
    renderModal({ onOpenChange });

    expect(screen.getByTestId('llm-config-clear')).toBeInTheDocument();
    await userEvent.click(screen.getByTestId('llm-config-clear'));

    expect(
      await screen.findByText('Clear LLM configuration?'),
    ).toBeInTheDocument();
    expect(remove.mutateAsync).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole('button', { name: /^cancel$/i }));
    expect(remove.mutateAsync).not.toHaveBeenCalled();

    await userEvent.click(screen.getByTestId('llm-config-clear'));
    await userEvent.click(screen.getByTestId('confirm-llm-clear'));

    await vi.waitFor(() => {
      expect(remove.mutateAsync).toHaveBeenCalledTimes(1);
    });
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(
      useUiStore
        .getState()
        .toasts.some((toast) => toast.message === 'LLM configuration cleared'),
    ).toBe(true);
  });

  it('hides the clear affordance while no configuration exists', () => {
    mockHook({ status: unconfigured });
    renderModal({ blocking: true });

    expect(screen.queryByTestId('llm-config-clear')).not.toBeInTheDocument();
    expect(screen.getByText('LLM configuration required')).toBeInTheDocument();
  });

  it('hides the close button in blocking mode and shows it otherwise', () => {
    mockHook({ status: unconfigured });
    const { unmount } = renderModal({ blocking: true });
    expect(
      screen.queryByRole('button', { name: /close/i }),
    ).not.toBeInTheDocument();
    unmount();

    mockHook({ status: configuredStatus });
    renderModal();
    expect(
      screen.getAllByRole('button', { name: /close/i }).length,
    ).toBeGreaterThan(0);
    expect(screen.getByText('LLM configuration')).toBeInTheDocument();
  });

  it('rejects a non-http base URL with the URL error', async () => {
    const { save } = mockHook({ status: unconfigured });
    renderModal();

    await fillRequiredFields();
    await userEvent.clear(input('llm-base-url'));
    await userEvent.type(input('llm-base-url'), 'ftp://api.deepseek.com/v1');
    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).not.toHaveBeenCalled();
    });
    expect(
      screen.getAllByText('Enter a valid http(s) URL').length,
    ).toBe(1);
  });
});
