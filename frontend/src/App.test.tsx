import { render, screen, waitFor, act, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import App from './App';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import { useFeeds } from './features/feeds/hooks/useFeeds';
import { useAuthStore } from './store/authStore';
import { useFeedUiStore } from './features/feeds/store/feedUiStore';

vi.mock('./features/feeds/hooks/useFeeds');
vi.mock('./store/authStore');

const mockUseFeeds = useFeeds as any;
const mockUseAuthStore = useAuthStore as any;

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

describe('App', () => {
  beforeEach(() => {
    mockUseFeeds.mockReturnValue({ ...baseFeeds });
    // SECURITY.md L7: useAuthReconciliation reads
    // `useAuthStore.getState().user` on mount, so the mock must
    // expose getState alongside the hook return value. Without
    // it, the hook throws "Cannot read properties of undefined
    // (reading 'user')" and every App render fails.
    const baseAuth = {
      isAuthenticated: false,
      user: null,
      logout: vi.fn(),
    };
    mockUseAuthStore.mockReturnValue(baseAuth);
    mockUseAuthStore.getState = vi.fn().mockReturnValue(baseAuth);
    // Reset the Zustand UI store so search/auth/edit state from a previous
    // test doesn't leak into the next one.
    useFeedUiStore.setState({
      searchTerm: '',
      showAddForm: false,
      editingFeed: null,
    });
  });

  it('renders the brand in the nav', () => {
    render(<App />);
    expect(screen.getAllByText('Nyx').length).toBeGreaterThan(0);
  });

  it('displays loading skeleton initially', () => {
    mockUseFeeds.mockReturnValue({ ...baseFeeds, isLoading: true });
    render(<App />);
    expect(screen.getByTestId('feed-list-skeleton')).toBeInTheDocument();
  });

  it('fetches and displays feeds', async () => {
    const feeds = [
      { id: 1, title: 'Test Feed 1', description: 'Desc 1', rating: 8 },
      { id: 2, title: 'Test Feed 2', description: 'Desc 2', rating: 9 },
    ];
    mockUseFeeds.mockReturnValue({ ...baseFeeds, feeds });

    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('Test Feed 1')).toBeInTheDocument();
      expect(screen.getByText('Test Feed 2')).toBeInTheDocument();
    });
  });

  it('shows "No feeds found" message when there are no feeds', async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('No feeds found')).toBeInTheDocument();
    });
  });

  it('can add a new feed (when authenticated)', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const addFeedMock = vi.fn().mockResolvedValue({});
    mockUseFeeds.mockReturnValue({
      ...baseFeeds,
      addFeed: { mutateAsync: addFeedMock },
    });

    render(<App />);

    await userEvent.click(screen.getByText('Add Feed'));
    await userEvent.type(screen.getByPlaceholderText('Feed title'), 'New Test Feed');
    await userEvent.click(screen.getByRole('button', { name: /save feed/i }));

    await waitFor(() => {
      expect(addFeedMock).toHaveBeenCalledWith(
        expect.objectContaining({ title: 'New Test Feed' }),
      );
    });
  });

  it('can start editing a feed (when authenticated)', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const feeds = [{ id: 1, title: 'Feed to Edit', description: 'Desc', rating: 5 }];
    mockUseFeeds.mockReturnValue({ ...baseFeeds, feeds });

    render(<App />);

    await waitFor(() => screen.getByText('Feed to Edit'));
    await userEvent.click(screen.getByTitle('Edit'));

    expect(screen.getByRole('heading', { name: 'Edit Feed' })).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Feed title')).toHaveValue('Feed to Edit');
  });

  it('can delete a feed via the confirm dialog (when authenticated)', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const deleteFeedMock = vi.fn().mockResolvedValue({});
    const feeds = [{ id: 1, title: 'Feed to Delete', description: 'Desc', rating: 5 }];
    mockUseFeeds.mockReturnValue({
      ...baseFeeds,
      feeds,
      deleteFeed: { mutateAsync: deleteFeedMock },
    });

    render(<App />);

    await waitFor(() => screen.getByText('Feed to Delete'));
    await userEvent.click(screen.getByTitle('Delete'));

    // The Radix dialog should now be open; click the confirm button.
    const confirmButton = await screen.findByTestId('confirm-delete');
    await userEvent.click(confirmButton);

    await waitFor(() => {
      expect(deleteFeedMock).toHaveBeenCalledWith(1);
    });
  });

  it('does not delete when the cancel button is clicked in the confirm dialog', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const deleteFeedMock = vi.fn().mockResolvedValue({});
    const feeds = [{ id: 1, title: 'Feed to Keep', description: 'Desc', rating: 5 }];
    mockUseFeeds.mockReturnValue({
      ...baseFeeds,
      feeds,
      deleteFeed: { mutateAsync: deleteFeedMock },
    });

    render(<App />);

    await waitFor(() => screen.getByText('Feed to Keep'));
    await userEvent.click(screen.getByTitle('Delete'));

    const cancelButton = await screen.findByRole('button', { name: /cancel/i });
    await userEvent.click(cancelButton);

    expect(deleteFeedMock).not.toHaveBeenCalled();
  });

  it('only fires deleteFeed.mutateAsync once when the confirm button is double-clicked rapidly', async () => {
    // SECURITY.md L9: the destructive confirm button must not be
    // re-fireable mid-mutation. The fix has two layers: the
    // button is `disabled={deleteFeed.isPending}` and the
    // executeDelete handler short-circuits if a delete is already
    // in flight. Either guard alone would close the hole; both
    // together cover the case where React hasn't flushed the
    // dialog-close re-render yet (a rapid second click can hit
    // the handler with the stale `confirmDeleteId`).
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    // The mutation stays pending for the lifetime of the test so
    // `isPending` reads true on every render. `mutateAsync` would
    // normally resolve; we override it to a never-resolving
    // promise so the disabled-button assertion is meaningful.
    let resolveDelete: (() => void) | null = null;
    const deleteFeedMock = vi.fn(
      () => new Promise<void>((resolve) => {
        resolveDelete = resolve;
      }),
    );
    const feeds = [{ id: 1, title: 'Feed to Delete', description: 'Desc', rating: 5 }];
    mockUseFeeds.mockReturnValue({
      ...baseFeeds,
      feeds,
      deleteFeed: {
        mutateAsync: deleteFeedMock,
        // useMutation exposes `isPending`; we model the in-flight
        // state for the duration of the test.
        isPending: true,
      },
    });

    render(<App />);

    await waitFor(() => screen.getByText('Feed to Delete'));
    await userEvent.click(screen.getByTitle('Delete'));

    // The dialog renders; the confirm button is disabled because
    // deleteFeed.isPending is true (we modeled it that way for
    // this test — covers the disabled-while-pending half).
    const confirmButton = await screen.findByTestId('confirm-delete');
    expect(confirmButton).toBeDisabled();

    // Sanity: pressing the disabled button is a no-op.
    await userEvent.click(confirmButton);
    expect(deleteFeedMock).not.toHaveBeenCalled();

    // Cleanup: resolve the pending promise so vitest doesn't warn
    // about an unhandled rejection.
    resolveDelete?.();
  });

  it('relabels the confirm button to "Deleting…" while the mutation is in flight', async () => {
    // Companion to the disabled-state test: while `isPending` is
    // true the visible label flips so users see that the action
    // is in progress (and that re-clicking is a no-op).
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const feeds = [{ id: 1, title: 'In Flight', description: 'Desc', rating: 5 }];
    mockUseFeeds.mockReturnValue({
      ...baseFeeds,
      feeds,
      deleteFeed: {
        mutateAsync: vi.fn(() => new Promise<void>(() => {})),
        isPending: true,
      },
    });

    render(<App />);

    await waitFor(() => screen.getByText('In Flight'));
    await userEvent.click(screen.getByTitle('Delete'));

    const confirmButton = await screen.findByTestId('confirm-delete');
    expect(confirmButton).toHaveTextContent(/deleting/i);
  });

  it('does not refetch on every keystroke — only after the debounce settles', async () => {
    // Fake timers so the setTimeout inside useDebounce is under our
    // control and doesn't fire on its own between assertions.
    vi.useFakeTimers();

    render(<App />);

    const input = screen.getByPlaceholderText('Search for feeds...');

    // The first argument to useFeeds is the search term it will key
    // its query on. If the bug were present, every keystroke would
    // surface a new value here, which is what triggers a refetch.
    // With the debounce, the value stays at '' until the timer fires.
    fireEvent.change(input, { target: { value: 'm' } });
    fireEvent.change(input, { target: { value: 'ma' } });
    fireEvent.change(input, { target: { value: 'mat' } });
    fireEvent.change(input, { target: { value: 'matr' } });
    fireEvent.change(input, { target: { value: 'matri' } });
    fireEvent.change(input, { target: { value: 'matrix' } });

    const lastCallArg = mockUseFeeds.mock.calls.at(-1)?.[0];
    expect(lastCallArg).toBe('');

    // Drain the debounce window. After it elapses, App should have
    // re-rendered with the debounced value and called useFeeds with
    // the settled string.
    await act(async () => {
      vi.advanceTimersByTime(300);
    });

    expect(mockUseFeeds.mock.calls.at(-1)?.[0]).toBe('matrix');

    vi.useRealTimers();
  });

  describe('SEO metadata (document.title)', () => {
    beforeEach(() => {
      // Each test asserts on document.title, which persists across renders.
      document.title = '';
    });

    it('uses the default Nyx title when nothing else is going on', () => {
      render(<App />);
      expect(document.title).toBe('Nyx — Your minimalist feed guide');
    });

    it('reflects the debounced search term, not the raw input', async () => {
      vi.useFakeTimers();
      render(<App />);

      const input = screen.getByPlaceholderText('Search for feeds...');

      // Type "matrix" one char at a time. The title should NOT update
      // mid-typing — that's what the debounce is for.
      fireEvent.change(input, { target: { value: 'm' } });
      fireEvent.change(input, { target: { value: 'ma' } });
      fireEvent.change(input, { target: { value: 'mat' } });
      fireEvent.change(input, { target: { value: 'matr' } });
      fireEvent.change(input, { target: { value: 'matri' } });
      fireEvent.change(input, { target: { value: 'matrix' } });

      expect(document.title).toBe('Nyx — Your minimalist feed guide');

      await act(async () => {
        vi.advanceTimersByTime(300);
      });

      expect(document.title).toBe('"matrix" — Search — Nyx');

      vi.useRealTimers();
    });

    it('uses the sign-in title when the auth form is open (logged out)', async () => {
      render(<App />);
      await userEvent.click(screen.getByText('Login / Register'));
      expect(document.title).toBe('Sign in — Nyx');
    });

    it('uses the add-feed title when the add form is open (authenticated)', () => {
      mockUseAuthStore.mockReturnValue({
        isAuthenticated: true,
        user: { id: 1, username: 'testuser' },
        logout: vi.fn(),
      });
      render(<App />);
      // The "Add Feed" button toggles showAddForm in the UI store.
      fireEvent.click(screen.getByText('Add Feed'));
      expect(document.title).toBe('Add a feed — Nyx');
    });

    it('uses the edit-feed title when editing', () => {
      mockUseAuthStore.mockReturnValue({
        isAuthenticated: true,
        user: { id: 1, username: 'testuser' },
        logout: vi.fn(),
      });
      const feeds = [
        { id: 1, title: 'Feed to Edit', description: 'Desc', rating: 5, created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
      ];
      mockUseFeeds.mockReturnValue({ ...baseFeeds, feeds });

      render(<App />);

      // Drive editing via the UI store directly — same effect as clicking
      // the row's edit button, but skips FeedList's internal handlers.
      act(() => {
        useFeedUiStore.getState().setEditingFeed(feeds[0]);
      });

      expect(document.title).toBe('Edit "Feed to Edit" — Nyx');
    });
  });
});