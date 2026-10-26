import { render, screen, waitFor, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import App from './App';
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { useFeeds } from './features/feeds/hooks/useFeeds';
import { useAuthStore } from './store/authStore';
import { useFeedUiStore } from './features/feeds/store/feedUiStore';
import { useLlmConfig } from './features/chat/hooks/useLlmConfig';
import { useChatStore } from './features/chat/store/chatStore';
import type { LlmConfigStatus } from './features/chat/types/llmConfig';
import { i18n } from '~/i18n';

vi.mock('./features/feeds/hooks/useFeeds');
vi.mock('./store/authStore');
vi.mock('./features/chat/hooks/useLlmConfig');
vi.mock('./features/chat/components/ChatPanel', () => ({
  ChatPanel: ({
    open,
    onOpenChange,
  }: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
  }) =>
    open ? (
      <div data-testid="chat-panel-mock">
        <button type="button" onClick={() => onOpenChange(false)}>
          close-chat-mock
        </button>
      </div>
    ) : null,
}));

const mockUseFeeds = useFeeds as any;
const mockUseAuthStore = useAuthStore as any;
const mockUseLlmConfig = useLlmConfig as any;

const baseFeeds = {
  feeds: [],
  totalCount: 0,
  isLoading: false,
  isError: false,
  hasMore: false,
  isLoadingMore: false,
  loadMore: vi.fn(),
  addFeed: { mutateAsync: vi.fn() },
  updateFeed: { mutateAsync: vi.fn() },
  deleteFeed: { mutateAsync: vi.fn() },
};

const unconfiguredStatus: LlmConfigStatus = {
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

interface HookOverrides {
  status?: LlmConfigStatus | null;
  isLoading?: boolean;
  isUnavailable?: boolean;
  userId?: number | null;
  save?: { mutateAsync: ReturnType<typeof vi.fn>; isPending: boolean };
  remove?: { mutateAsync: ReturnType<typeof vi.fn>; isPending: boolean };
}

function mockHook(overrides: HookOverrides = {}) {
  const status = overrides.status !== undefined ? overrides.status : null;
  const save =
    overrides.save ??
    ({
      mutateAsync: vi.fn().mockResolvedValue(status ?? configuredStatus),
      isPending: false,
    } satisfies HookOverrides['save']);
  const remove =
    overrides.remove ??
    ({
      mutateAsync: vi.fn().mockResolvedValue(undefined),
      isPending: false,
    } satisfies HookOverrides['remove']);
  mockUseLlmConfig.mockReturnValue({
    userId: overrides.userId !== undefined ? overrides.userId : 1,
    status,
    configured: status ? status.configured : null,
    agentReady: status ? status.llm !== null : null,
    isLoading: overrides.isLoading ?? false,
    isError: false,
    isUnavailable: overrides.isUnavailable ?? false,
    refetch: vi.fn(),
    save,
    remove,
  });
}

function authenticate() {
  const state = {
    isAuthenticated: true,
    user: { id: 1, username: 'testuser' },
    logout: vi.fn(),
  };
  mockUseAuthStore.mockReturnValue(state);
  mockUseAuthStore.getState = vi.fn().mockReturnValue(state);
  return state;
}

async function fillRequiredFields() {
  const input = (id: string) => document.getElementById(id) as HTMLInputElement;
  await userEvent.type(input('llm-base-url'), 'https://api.deepseek.com/v1');
  await userEvent.type(input('llm-model'), 'deepseek-chat');
  await userEvent.type(input('llm-api-key'), 'sk-llm');
  await userEvent.click(screen.getByTestId('llm-embedding-toggle'));
  await userEvent.click(screen.getByTestId('llm-tavily-toggle'));
  await userEvent.type(input('embedding-base-url'), 'http://localhost:8000/v1');
  await userEvent.type(input('embedding-model'), 'Qwen/Qwen3-Embedding-0.6B');
  await userEvent.type(input('embedding-api-key'), 'sk-emb');
  await userEvent.type(input('tavily-base-url'), 'https://api.tavily.com/search');
  await userEvent.type(input('tavily-api-key'), 'tvly-key');
}

describe('App LLM config gating', () => {
  beforeEach(async () => {
    await i18n.changeLanguage('en');
    mockUseFeeds.mockReturnValue({ ...baseFeeds });
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: false,
      user: null,
      logout: vi.fn(),
    });
    mockUseAuthStore.getState = vi.fn().mockReturnValue({
      isAuthenticated: false,
      user: null,
      logout: vi.fn(),
    });
    mockUseLlmConfig.mockReset();
    mockHook();
    useFeedUiStore.setState({
      searchTerm: '',
      showAddForm: false,
      editingFeed: null,
      sortOrder: 'desc',
    });
    useChatStore.setState({ llmGateOpen: false });
  });

  afterEach(() => {
    vi.unstubAllEnvs();
  });

  it('hides the gear button while logged out', () => {
    render(<App />);
    expect(screen.queryByTestId('llm-config-button')).not.toBeInTheDocument();
  });

  it('opens the blocking gate when the panel is open and no config exists', async () => {
    authenticate();
    mockHook({ status: unconfiguredStatus });
    render(<App />);

    expect(
      await screen.findByText('LLM configuration required'),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Fill in the main-LLM fields/),
    ).toBeInTheDocument();
    expect(screen.getByText('Close')).toBeInTheDocument();
    expect(screen.queryByTestId('llm-config-clear')).not.toBeInTheDocument();
  });

  it('closes the gate from the close button and dismisses the chat panel', async () => {
    authenticate();
    mockHook({ status: unconfiguredStatus });
    const view = render(<App />);

    await screen.findByText('LLM configuration required');
    await userEvent.click(screen.getByText('Close').closest('button')!);

    await waitFor(() => {
      expect(
        screen.queryByText('LLM configuration required'),
      ).not.toBeInTheDocument();
    });
    expect(screen.queryByTestId('chat-panel-mock')).not.toBeInTheDocument();

    await userEvent.click(
      screen.getByRole('button', { name: 'Open AI Agent' }),
    );
    view.rerender(<App />);
    expect(
      await screen.findByText('LLM configuration required'),
    ).toBeInTheDocument();
    expect(screen.queryByTestId('chat-panel-mock')).not.toBeInTheDocument();
  });

  it('does not gate while the status query is loading', () => {
    authenticate();
    mockHook({ status: unconfiguredStatus, isLoading: true });
    render(<App />);

    expect(
      screen.queryByText('LLM configuration required'),
    ).not.toBeInTheDocument();
  });

  it('shows no gate and renders the gear when configured', async () => {
    authenticate();
    mockHook({ status: configuredStatus });
    render(<App />);

    expect(
      screen.queryByText('LLM configuration required'),
    ).not.toBeInTheDocument();
    const gear = await screen.findByTestId('llm-config-button');
    expect(gear).toHaveAttribute('aria-label', 'LLM settings');
  });

  it('opens the dismissible modal from the gear button and closes it', async () => {
    authenticate();
    mockHook({ status: configuredStatus });
    render(<App />);

    await userEvent.click(await screen.findByTestId('llm-config-button'));

    expect(await screen.findByText('LLM configuration')).toBeInTheDocument();
    expect(screen.queryByTestId('llm-config-clear')).toBeInTheDocument();

    await userEvent.click(screen.getByText('Close').closest('button')!);

    await waitFor(() => {
      expect(screen.queryByText('LLM configuration')).not.toBeInTheDocument();
    });
  });

  it('hides the gear and the gate when the config endpoint is unavailable', async () => {
    authenticate();
    mockHook({ status: null, isUnavailable: true });
    render(<App />);

    expect(screen.queryByTestId('llm-config-button')).not.toBeInTheDocument();
    expect(
      screen.queryByText('LLM configuration required'),
    ).not.toBeInTheDocument();
    expect(await screen.findByTestId('chat-panel-mock')).toBeInTheDocument();
  });

  it('hides the gear and the gate when the LLM feature is disabled', async () => {
    vi.stubEnv('VITE_LLM_FEATURE_ENABLED', 'false');
    authenticate();
    mockHook({ status: unconfiguredStatus });
    render(<App />);

    expect(screen.queryByTestId('llm-config-button')).not.toBeInTheDocument();
    expect(
      screen.queryByText('LLM configuration required'),
    ).not.toBeInTheDocument();
    expect(await screen.findByTestId('chat-panel-mock')).toBeInTheDocument();
  });

  it('renders the blocking modal from the 409 flag; saving clears it and re-opens the panel', async () => {
    authenticate();
    mockHook({
      status: unconfiguredStatus,
      save: {
        mutateAsync: vi.fn().mockImplementation(async () => {
          mockHook({ status: configuredStatus });
          return configuredStatus;
        }),
        isPending: false,
      },
    });
    act(() => {
      useChatStore.getState().setLlmGateOpen(true);
    });
    const view = render(<App />);

    expect(
      await screen.findByText('LLM configuration required'),
    ).toBeInTheDocument();
    expect(screen.getByText('Close')).toBeInTheDocument();

    await fillRequiredFields();
    await userEvent.click(screen.getByTestId('llm-config-save'));
    view.rerender(<App />);

    await waitFor(() => {
      expect(useChatStore.getState().llmGateOpen).toBe(false);
      expect(
        screen.queryByText('LLM configuration required'),
      ).not.toBeInTheDocument();
    });
    expect(await screen.findByTestId('chat-panel-mock')).toBeInTheDocument();
  });

  it('re-gates from the status when the config is cleared while the panel is open', async () => {
    authenticate();
    mockHook({
      status: configuredStatus,
      remove: {
        mutateAsync: vi.fn().mockImplementation(async () => {
          mockHook({ status: unconfiguredStatus });
          return undefined;
        }),
        isPending: false,
      },
    });
    render(<App />);

    await userEvent.click(await screen.findByTestId('llm-config-button'));
    await userEvent.click(await screen.findByTestId('llm-config-clear'));
    await userEvent.click(await screen.findByTestId('confirm-llm-clear'));

    await waitFor(() => {
      expect(
        screen.getByText('LLM configuration required'),
      ).toBeInTheDocument();
    });
    expect(screen.getByText('Close')).toBeInTheDocument();
  });

  it('closing the settings modal keeps the chat panel open', async () => {
    authenticate();
    mockHook({ status: configuredStatus });
    render(<App />);

    expect(await screen.findByTestId('chat-panel-mock')).toBeInTheDocument();
    await userEvent.click(await screen.findByTestId('llm-config-button'));
    expect(await screen.findByText('LLM configuration')).toBeInTheDocument();

    await userEvent.click(screen.getByText('Close').closest('button')!);

    await waitFor(() => {
      expect(screen.queryByText('LLM configuration')).not.toBeInTheDocument();
    });
    expect(screen.getByTestId('chat-panel-mock')).toBeInTheDocument();
  });

  it('closes the modal and drops the previous user\'s values when the identity changes', async () => {
    authenticate();
    mockHook({ status: configuredStatus, userId: 1 });
    const view = render(<App />);

    await userEvent.click(await screen.findByTestId('llm-config-button'));
    expect(await screen.findByText('LLM configuration')).toBeInTheDocument();
    const baseUrl = document.getElementById('llm-base-url') as HTMLInputElement;
    const apiKey = document.getElementById('llm-api-key') as HTMLInputElement;
    await userEvent.type(baseUrl, 'https://llm-a.example.test/v1');
    await userEvent.type(apiKey, 'sk-user-a-secret');

    const secondUser = {
      isAuthenticated: true,
      user: { id: 2, username: 'otheruser' },
      logout: vi.fn(),
    };
    mockUseAuthStore.mockReturnValue(secondUser);
    mockUseAuthStore.getState = vi.fn().mockReturnValue(secondUser);
    mockHook({ status: unconfiguredStatus, userId: 2 });
    view.rerender(<App />);

    await waitFor(() => {
      expect(screen.queryByText('LLM configuration')).not.toBeInTheDocument();
    });
    await waitFor(() => {
      expect(
        (document.getElementById('llm-base-url') as HTMLInputElement).value,
      ).toBe('');
    });
    expect(
      (document.getElementById('llm-api-key') as HTMLInputElement).value,
    ).toBe('');
    expect(screen.getByText('LLM configuration required')).toBeInTheDocument();
  });
});
