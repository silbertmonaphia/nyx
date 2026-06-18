import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import App from './App';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import { useMovies } from './features/movies/hooks/useMovies';
import { useAuthStore } from './store/authStore';

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
});