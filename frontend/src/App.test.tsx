import { render, screen, waitFor, act, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import App from './App';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import { useMovies } from './features/movies/hooks/useMovies';
import { useAuthStore } from './store/authStore';
import { useMovieUiStore } from './features/movies/store/movieUiStore';

vi.mock('./features/movies/hooks/useMovies');
vi.mock('./store/authStore');

const mockUseMovies = useMovies as any;
const mockUseAuthStore = useAuthStore as any;

const baseMovies = {
  movies: [],
  totalCount: 0,
  isLoading: false,
  isError: false,
  hasMore: false,
  isLoadingMore: false,
  loadMore: vi.fn(),
  addMovie: { mutateAsync: vi.fn() },
  updateMovie: { mutateAsync: vi.fn() },
  deleteMovie: { mutateAsync: vi.fn() },
};

describe('App', () => {
  beforeEach(() => {
    mockUseMovies.mockReturnValue({ ...baseMovies });
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: false,
      user: null,
      logout: vi.fn(),
    });
    // Reset the Zustand UI store so search/auth/edit state from a previous
    // test doesn't leak into the next one.
    useMovieUiStore.setState({
      searchTerm: '',
      showAddForm: false,
      editingMovie: null,
    });
  });

  it('renders the main title', () => {
    render(<App />);
    expect(screen.getAllByText('Nyx').length).toBeGreaterThan(0);
  });

  it('displays loading skeleton initially', () => {
    mockUseMovies.mockReturnValue({ ...baseMovies, isLoading: true });
    render(<App />);
    expect(screen.getByTestId('movie-list-skeleton')).toBeInTheDocument();
  });

  it('fetches and displays movies', async () => {
    const movies = [
      { id: 1, title: 'Test Movie 1', description: 'Desc 1', rating: 8 },
      { id: 2, title: 'Test Movie 2', description: 'Desc 2', rating: 9 },
    ];
    mockUseMovies.mockReturnValue({ ...baseMovies, movies });

    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('Test Movie 1')).toBeInTheDocument();
      expect(screen.getByText('Test Movie 2')).toBeInTheDocument();
    });
  });

  it('shows "No movies found" message when there are no movies', async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('No movies found')).toBeInTheDocument();
    });
  });

  it('can add a new movie (when authenticated)', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const addMovieMock = vi.fn().mockResolvedValue({});
    mockUseMovies.mockReturnValue({
      ...baseMovies,
      addMovie: { mutateAsync: addMovieMock },
    });

    render(<App />);

    await userEvent.click(screen.getByText('Add Movie'));
    await userEvent.type(screen.getByPlaceholderText('Movie title'), 'New Test Movie');
    await userEvent.click(screen.getByRole('button', { name: /save movie/i }));

    await waitFor(() => {
      expect(addMovieMock).toHaveBeenCalledWith(
        expect.objectContaining({ title: 'New Test Movie' }),
      );
    });
  });

  it('can start editing a movie (when authenticated)', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const movies = [{ id: 1, title: 'Movie to Edit', description: 'Desc', rating: 5 }];
    mockUseMovies.mockReturnValue({ ...baseMovies, movies });

    render(<App />);

    await waitFor(() => screen.getByText('Movie to Edit'));
    await userEvent.click(screen.getByTitle('Edit'));

    expect(screen.getByRole('heading', { name: 'Edit Movie' })).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Movie title')).toHaveValue('Movie to Edit');
  });

  it('can delete a movie via the confirm dialog (when authenticated)', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const deleteMovieMock = vi.fn().mockResolvedValue({});
    const movies = [{ id: 1, title: 'Movie to Delete', description: 'Desc', rating: 5 }];
    mockUseMovies.mockReturnValue({
      ...baseMovies,
      movies,
      deleteMovie: { mutateAsync: deleteMovieMock },
    });

    render(<App />);

    await waitFor(() => screen.getByText('Movie to Delete'));
    await userEvent.click(screen.getByTitle('Delete'));

    // The Radix dialog should now be open; click the confirm button.
    const confirmButton = await screen.findByTestId('confirm-delete');
    await userEvent.click(confirmButton);

    await waitFor(() => {
      expect(deleteMovieMock).toHaveBeenCalledWith(1);
    });
  });

  it('does not delete when the cancel button is clicked in the confirm dialog', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const deleteMovieMock = vi.fn().mockResolvedValue({});
    const movies = [{ id: 1, title: 'Movie to Keep', description: 'Desc', rating: 5 }];
    mockUseMovies.mockReturnValue({
      ...baseMovies,
      movies,
      deleteMovie: { mutateAsync: deleteMovieMock },
    });

    render(<App />);

    await waitFor(() => screen.getByText('Movie to Keep'));
    await userEvent.click(screen.getByTitle('Delete'));

    const cancelButton = await screen.findByRole('button', { name: /cancel/i });
    await userEvent.click(cancelButton);

    expect(deleteMovieMock).not.toHaveBeenCalled();
  });

  it('does not refetch on every keystroke — only after the debounce settles', async () => {
    // Fake timers so the setTimeout inside useDebounce is under our
    // control and doesn't fire on its own between assertions.
    vi.useFakeTimers();

    render(<App />);

    const input = screen.getByPlaceholderText('Search for movies...');

    // The first argument to useMovies is the search term it will key
    // its query on. If the bug were present, every keystroke would
    // surface a new value here, which is what triggers a refetch.
    // With the debounce, the value stays at '' until the timer fires.
    fireEvent.change(input, { target: { value: 'm' } });
    fireEvent.change(input, { target: { value: 'ma' } });
    fireEvent.change(input, { target: { value: 'mat' } });
    fireEvent.change(input, { target: { value: 'matr' } });
    fireEvent.change(input, { target: { value: 'matri' } });
    fireEvent.change(input, { target: { value: 'matrix' } });

    const lastCallArg = mockUseMovies.mock.calls.at(-1)?.[0];
    expect(lastCallArg).toBe('');

    // Drain the debounce window. After it elapses, App should have
    // re-rendered with the debounced value and called useMovies with
    // the settled string.
    await act(async () => {
      vi.advanceTimersByTime(300);
    });

    expect(mockUseMovies.mock.calls.at(-1)?.[0]).toBe('matrix');

    vi.useRealTimers();
  });

  describe('SEO metadata (document.title)', () => {
    beforeEach(() => {
      // Each test asserts on document.title, which persists across renders.
      document.title = '';
    });

    it('uses the default Nyx title when nothing else is going on', () => {
      render(<App />);
      expect(document.title).toBe('Nyx — Your minimalist movie guide');
    });

    it('reflects the debounced search term, not the raw input', async () => {
      vi.useFakeTimers();
      render(<App />);

      const input = screen.getByPlaceholderText('Search for movies...');

      // Type "matrix" one char at a time. The title should NOT update
      // mid-typing — that's what the debounce is for.
      fireEvent.change(input, { target: { value: 'm' } });
      fireEvent.change(input, { target: { value: 'ma' } });
      fireEvent.change(input, { target: { value: 'mat' } });
      fireEvent.change(input, { target: { value: 'matr' } });
      fireEvent.change(input, { target: { value: 'matri' } });
      fireEvent.change(input, { target: { value: 'matrix' } });

      expect(document.title).toBe('Nyx — Your minimalist movie guide');

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

    it('uses the add-movie title when the add form is open (authenticated)', () => {
      mockUseAuthStore.mockReturnValue({
        isAuthenticated: true,
        user: { id: 1, username: 'testuser' },
        logout: vi.fn(),
      });
      render(<App />);
      // The "Add Movie" button toggles showAddForm in the UI store.
      fireEvent.click(screen.getByText('Add Movie'));
      expect(document.title).toBe('Add a movie — Nyx');
    });

    it('uses the edit-movie title when editing', () => {
      mockUseAuthStore.mockReturnValue({
        isAuthenticated: true,
        user: { id: 1, username: 'testuser' },
        logout: vi.fn(),
      });
      const movies = [
        { id: 1, title: 'Movie to Edit', description: 'Desc', rating: 5 },
      ];
      mockUseMovies.mockReturnValue({ ...baseMovies, movies });

      render(<App />);

      // Drive editing via the UI store directly — same effect as clicking
      // the row's edit button, but skips MovieList's internal handlers.
      act(() => {
        useMovieUiStore.getState().setEditingMovie(movies[0]);
      });

      expect(document.title).toBe('Edit "Movie to Edit" — Nyx');
    });
  });
});