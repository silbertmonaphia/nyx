import { describe, it, expect, beforeEach, afterEach, vi, type Mock } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LlmConfigModal } from './LlmConfigModal';
import { useLlmConfig, useLlmSlotProbe } from '../hooks/useLlmConfig';
import { useAuthStore } from '~/store/authStore';
import { useUiStore } from '~/store/uiStore';
import type { LlmConfigStatus, LlmConfigUpdateInput } from '../types/llmConfig';

vi.mock('../hooks/useLlmConfig');
vi.mock('~/features/kb/hooks/useKb', () => ({
  useKbSettings: () => ({
    data: { enabled: false, private_base: '', embedding_model: '' },
    isLoading: false,
  }),
  useKbMutations: () => ({
    createSource: { mutateAsync: vi.fn(), isPending: false },
  }),
}));

const mockedUseLlmConfig = useLlmConfig as unknown as ReturnType<typeof vi.fn>;
const mockedUseLlmSlotProbe = useLlmSlotProbe as unknown as ReturnType<typeof vi.fn>;

function slotProbeFixture(slot: string, overrides: Record<string, unknown> = {}) {
  return {
    probe: vi.fn(),
    probeAsync: vi.fn().mockResolvedValue(undefined),
    isPending: false,
    result: null,
    reset: vi.fn(),
    ...overrides,
  };
}

function mockSlotProbes(
  fixtures: Record<string, Record<string, unknown>> = {},
) {
  mockedUseLlmSlotProbe.mockImplementation((slot: string) =>
    slotProbeFixture(slot, fixtures[slot]),
  );
}

function makeUser(id: number) {
  return {
    id,
    username: `user-${id}`,
    created_at: '2026-10-14T00:00:00Z',
    updated_at: '2026-10-14T00:00:00Z',
  };
}

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
  userId?: number | null;
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
  const clearProbeResults = vi.fn();
  mockedUseLlmConfig.mockReturnValue({
    userId: overrides.userId !== undefined ? overrides.userId : 1,
    status,
    configured: status ? status.configured : null,
    isLoading: false,
    isError: false,
    refetch: vi.fn(),
    save,
    remove,
    clearProbeResults,
  });
  return { save, remove, clearProbeResults };
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
    mockSlotProbes();
    useUiStore.setState({ toasts: [] });
    useAuthStore.setState({ user: makeUser(1), isAuthenticated: true });
    document.body.innerHTML = '';
  });

  afterEach(() => {
    useAuthStore.setState({ user: null, isAuthenticated: false });
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

  it('shows the close button in blocking mode too', () => {
    mockHook({ status: unconfigured });
    renderModal({ blocking: true });
    expect(
      screen.getAllByRole('button', { name: /close/i }).length,
    ).toBeGreaterThan(0);
    expect(screen.getByText('LLM configuration required')).toBeInTheDocument();
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

  it('never carries the previous identity\'s API key into the next one', async () => {
    const { save } = mockHook({ status: unconfigured, userId: 1 });
    const view = render(
      <LlmConfigModal open onOpenChange={vi.fn()} />,
    );

    await userEvent.type(input('llm-base-url'), 'https://llm-a.example.test/v1');
    await userEvent.type(input('llm-model'), 'model-a');
    await userEvent.type(input('llm-api-key'), 'sk-user-a-secret');
    await expandOptionalSections();
    await userEvent.type(input('embedding-base-url'), 'https://embed-a.example.test/v1');
    await userEvent.type(input('embedding-model'), 'embed-model-a');
    await userEvent.type(input('embedding-api-key'), 'sk-user-a-embed');
    await userEvent.type(input('tavily-base-url'), 'https://tavily-a.example.test/search');
    await userEvent.type(input('tavily-api-key'), 'tvly-user-a');

    mockHook({ status: unconfigured, userId: 2, save });
    view.rerender(<LlmConfigModal open onOpenChange={vi.fn()} />);

    await waitFor(() => {
      expect(input('llm-api-key').value).toBe('');
    });
    expect(input('llm-base-url').value).toBe('');
    expect(input('llm-model').value).toBe('');
    expect(document.getElementById('embedding-api-key')).toBeNull();
    expect(document.getElementById('tavily-api-key')).toBeNull();

    await userEvent.type(input('llm-base-url'), 'https://llm-b.example.test/v1');
    await userEvent.type(input('llm-model'), 'model-b');
    await userEvent.type(input('llm-api-key'), 'sk-user-b-secret');
    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const payload = save.mutateAsync.mock.calls[0][0] as LlmConfigUpdateInput;
    expect(payload).toEqual({
      llm: {
        base_url: 'https://llm-b.example.test/v1',
        api_key: 'sk-user-b-secret',
        model: 'model-b',
      },
      embedding: { base_url: '', api_key: '', model: '' },
      tavily: { base_url: '', api_key: '' },
    });
  });

  it('ignores a save that resolves after the identity changed', async () => {
    let resolveSave: (status: LlmConfigStatus) => void = () => undefined;
    const pending = new Promise<LlmConfigStatus>((resolve) => {
      resolveSave = resolve;
    });
    const onSaved = vi.fn();
    const onOpenChange = vi.fn();
    mockHook({
      status: unconfigured,
      userId: 1,
      save: { mutateAsync: vi.fn(() => pending), isPending: false },
    });
    const view = render(
      <LlmConfigModal open onOpenChange={onOpenChange} onSaved={onSaved} />,
    );

    await userEvent.type(input('llm-base-url'), 'https://llm-a.example.test/v1');
    await userEvent.type(input('llm-model'), 'model-a');
    await userEvent.type(input('llm-api-key'), 'sk-user-a-secret');
    await userEvent.click(screen.getByTestId('llm-config-save'));

    useAuthStore.setState({ user: makeUser(2), isAuthenticated: true });
    mockHook({ status: unconfigured, userId: 2 });
    view.rerender(
      <LlmConfigModal open onOpenChange={onOpenChange} onSaved={onSaved} />,
    );

    resolveSave(configuredStatus);

    await waitFor(() => {
      expect(input('llm-base-url').value).toBe('');
    });
    expect(input('llm-api-key').value).toBe('');
    expect(onSaved).not.toHaveBeenCalled();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
  });

  it('does not signal a dismissal when a save closes the modal', async () => {
    const onDismissed = vi.fn();
    mockHook({ status: configuredStatus });
    renderModal({ onDismissed });

    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(
        useUiStore
          .getState()
          .toasts.some((toast) => toast.message === 'LLM configuration saved'),
      ).toBe(true);
    });
    expect(onDismissed).not.toHaveBeenCalled();
  });

  it('drops the plaintext API key from the form after a successful save', async () => {
    const savedStatus: LlmConfigStatus = {
      ...configuredStatus,
      llm: {
        base_url: 'https://llm.example.test/v1',
        model: 'deepseek-chat',
        api_key_masked: '••••cret',
      },
    };
    const { save } = mockHook({
      status: unconfigured,
      save: {
        mutateAsync: vi.fn().mockResolvedValue(savedStatus),
        isPending: false,
      },
    });
    const view = render(<LlmConfigModal open onOpenChange={vi.fn()} />);

    await userEvent.type(input('llm-base-url'), 'https://llm.example.test/v1');
    await userEvent.type(input('llm-model'), 'deepseek-chat');
    await userEvent.type(input('llm-api-key'), 'sk-user-a-secret');
    await userEvent.click(screen.getByTestId('llm-config-save'));

    await vi.waitFor(() => {
      expect(save.mutateAsync).toHaveBeenCalledTimes(1);
    });

    mockHook({ status: savedStatus, save });
    view.rerender(<LlmConfigModal open onOpenChange={vi.fn()} />);

    await waitFor(() => {
      expect(input('llm-api-key').value).toBe('');
    });
    expect(input('llm-base-url').value).toBe('https://llm.example.test/v1');
  });
});

describe('LlmConfigModal slot probe', () => {
  beforeEach(() => {
    mockedUseLlmConfig.mockReset();
    mockSlotProbes();
    useUiStore.setState({ toasts: [] });
    useAuthStore.setState({ user: makeUser(1), isAuthenticated: true });
    document.body.innerHTML = '';
  });

  afterEach(() => {
    useAuthStore.setState({ user: null, isAuthenticated: false });
  });

  it('probes only the clicked section with its current form values', async () => {
    mockHook({ status: unconfigured });
    const probeAsync = vi.fn().mockResolvedValue(undefined);
    mockSlotProbes({ chat: { probeAsync } });
    renderModal();

    await userEvent.type(input('llm-base-url'), 'https://api.deepseek.com/v1');
    await userEvent.type(input('llm-api-key'), 'sk-llm-plain');
    await userEvent.type(input('llm-model'), 'deepseek-chat');
    await userEvent.click(screen.getByTestId('llm-probe-chat-button'));

    await vi.waitFor(() => {
      expect(probeAsync).toHaveBeenCalledTimes(1);
    });
    expect(probeAsync).toHaveBeenCalledWith({
      llm: {
        base_url: 'https://api.deepseek.com/v1',
        api_key: 'sk-llm-plain',
        model: 'deepseek-chat',
      },
    });
  });

  it('renders a credential failure with the upstream status and masked disclosure', async () => {
    mockHook({ status: unconfigured });
    mockSlotProbes({
      embedding: {
        result: {
          probed: true,
          status: 'auth_failed',
          http_status: 401,
          request: {
            method: 'POST',
            url: 'https://api.siliconflow.cn/v1/embeddings',
            headers: { Authorization: 'Bearer ••••9876', 'Content-Type': 'application/json' },
            body: { model: 'Qwen/Qwen3-Embedding-0.6B', input: ['nyx connectivity probe'] },
          },
        },
      },
    });
    renderModal();

    await userEvent.click(screen.getByTestId('llm-embedding-toggle'));
    const status = screen.getByTestId('llm-probe-embedding-status');
    expect(status).toHaveTextContent(/Authentication failed/);
    expect(status).toHaveTextContent('401');

    await userEvent.click(screen.getByTestId('llm-probe-embedding-toggle'));
    const disclosure = screen.getByTestId('llm-probe-embedding-request');
    expect(disclosure).toHaveTextContent('Bearer ••••9876');
    expect(disclosure).not.toHaveTextContent('sk-validate-planted-key-9876');
  });

  it('keeps other sections usable while one section probes', async () => {
    mockHook({ status: unconfigured });
    mockSlotProbes({
      embedding: { isPending: true },
    });
    renderModal();

    await userEvent.click(screen.getByTestId('llm-embedding-toggle'));
    const embeddingButton = screen.getByTestId('llm-probe-embedding-button');
    const chatButton = screen.getByTestId('llm-probe-chat-button');
    expect(embeddingButton).toBeDisabled();
    expect(embeddingButton).toHaveTextContent('Probing');
    expect(chatButton).toBeEnabled();
  });

  it('renders a skipped section as neutral, not failure', async () => {
    mockHook({ status: unconfigured });
    mockSlotProbes({
      tavily: { result: { probed: false } },
    });
    renderModal();

    await userEvent.click(screen.getByTestId('llm-tavily-toggle'));
    expect(screen.getByTestId('llm-probe-tavily-status')).toHaveTextContent(
      /Empty section/,
    );
    expect(screen.getByTestId('llm-probe-tavily-status').className).not.toContain(
      'destructive',
    );
  });

  it('copies a replayable curl with a key placeholder', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    mockHook({ status: unconfigured });
    mockSlotProbes({
      chat: {
        result: {
          probed: true,
          status: 'ok',
          request: {
            method: 'POST',
            url: 'https://api.deepseek.com/v1/chat/completions',
            headers: { Authorization: 'Bearer ••••abcd', 'Content-Type': 'application/json' },
            body: { model: 'deepseek-chat', messages: [] },
          },
        },
      },
    });
    renderModal();

    await userEvent.click(screen.getByTestId('llm-probe-chat-toggle'));
    await userEvent.click(screen.getByTestId('llm-probe-chat-copy'));

    await vi.waitFor(() => {
      expect(writeText).toHaveBeenCalledTimes(1);
    });
    const curl = writeText.mock.calls[0][0] as string;
    expect(curl).toContain("curl -X POST 'https://api.deepseek.com/v1/chat/completions'");
    expect(curl).toContain('Authorization: Bearer <your API key>');
    expect(curl).not.toContain('••••abcd');
    expect(screen.getByTestId('llm-probe-chat-copy')).toHaveTextContent('Copied');
  });

  const probeRequestFixture = {
    probed: true,
    status: 'ok',
    request: {
      method: 'POST',
      url: 'https://api.deepseek.com/v1/chat/completions',
      headers: { Authorization: 'Bearer ••••abcd' },
      body: { model: 'deepseek-chat', messages: [] },
    },
  };

  it('falls back to execCommand when the clipboard API is unavailable', async () => {
    Object.assign(navigator, { clipboard: undefined });
    const execCopy = vi.fn(() => true);
    document.execCommand = execCopy as unknown as typeof document.execCommand;
    mockHook({ status: unconfigured });
    mockSlotProbes({ chat: { result: probeRequestFixture } });
    renderModal();

    await userEvent.click(screen.getByTestId('llm-probe-chat-toggle'));
    await userEvent.click(screen.getByTestId('llm-probe-chat-copy'));

    await vi.waitFor(() => {
      expect(execCopy).toHaveBeenCalled();
    });
    expect(screen.getByTestId('llm-probe-chat-copy')).toHaveTextContent('Copied');
  });

  it('shows copy failure feedback when every copy path fails', async () => {
    const writeText = vi.fn().mockRejectedValue(new Error('denied'));
    Object.assign(navigator, { clipboard: { writeText } });
    const execCopy = vi.fn(() => false);
    document.execCommand = execCopy as unknown as typeof document.execCommand;
    mockHook({ status: unconfigured });
    mockSlotProbes({ chat: { result: probeRequestFixture } });
    renderModal();

    await userEvent.click(screen.getByTestId('llm-probe-chat-toggle'));
    await userEvent.click(screen.getByTestId('llm-probe-chat-copy'));

    await vi.waitFor(() => {
      expect(screen.getByTestId('llm-probe-chat-copy')).toHaveTextContent('Copy failed');
    });
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(execCopy).toHaveBeenCalledTimes(1);
  });
});

describe('LlmConfigModal probe result lifecycle', () => {
  beforeEach(() => {
    mockedUseLlmConfig.mockReset();
    mockSlotProbes();
    useUiStore.setState({ toasts: [] });
    useAuthStore.setState({ user: makeUser(1), isAuthenticated: true });
    document.body.innerHTML = '';
  });

  afterEach(() => {
    useAuthStore.setState({ user: null, isAuthenticated: false });
  });

  const statusLabels: Record<string, RegExp> = {
    ok: /Connected/,
    auth_failed: /Authentication failed/,
    rate_limited: /Rate limited/,
    invalid_model: /Model not found/,
    invalid_config: /Incomplete configuration/,
    width_mismatch: /Width mismatch/,
    unreachable: /Unreachable/,
    timeout: /Timed out/,
    slow_response: /Slow response/,
  };

  for (const [status, label] of Object.entries(statusLabels)) {
    it(`renders the ${status} status class`, async () => {
      mockHook({ status: unconfigured });
      mockSlotProbes({ chat: { result: { probed: true, status } } });
      renderModal();
      const el = screen.getByTestId('llm-probe-chat-status');
      expect(el).toHaveTextContent(label);
      const destructive = el.className.includes('destructive');
      if (status === 'ok') expect(destructive).toBe(false);
      if (
        status === 'rate_limited' ||
        status === 'unreachable' ||
        status === 'timeout' ||
        status === 'slow_response'
      ) {
        expect(el.className).toContain('amber');
      } else if (status !== 'ok') {
        expect(destructive).toBe(true);
      }
    });
  }

  it('clears probe results when the modal closes', async () => {
    const { clearProbeResults } = mockHook({ status: unconfigured });
    const { rerender } = render(<LlmConfigModal open onOpenChange={vi.fn()} />);
    expect(clearProbeResults).not.toHaveBeenCalled();
    rerender(<LlmConfigModal open={false} onOpenChange={vi.fn()} />);
    await vi.waitFor(() => {
      expect(clearProbeResults).toHaveBeenCalled();
    });
  });
});
