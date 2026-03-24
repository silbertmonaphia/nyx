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

describe('App', () => {
  let addMovieMock: any;
  let updateMovieMock: any;
  let deleteMovieMock: any;

  beforeEach(() => {
    addMovieMock = vi.fn();
    updateMovieMock = vi.fn();
    deleteMovieMock = vi.fn();

    mockUseMovies.mockReturnValue({
      getMovies: () => ({ data: [], isLoading: true }),
      addMovie: { mutateAsync: addMovieMock },
      updateMovie: { mutateAsync: updateMovieMock },
      deleteMovie: { mutateAsync: deleteMovieMock },
    });

    // Default: Unauthenticated
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: false,
      user: null,
      logout: vi.fn(),
    });
  });

  it('renders the main title', () => {
    render(<App />);
    // There are multiple "Nyx" elements now (Logo and Title), checking if at least one exists
    expect(screen.getAllByText('Nyx').length).toBeGreaterThan(0);
  });

  it('displays loading state initially', () => {
    render(<App />);
    expect(screen.getByText('Loading movies...')).toBeInTheDocument();
  });

  it('fetches and displays movies', async () => {
    const movies = [
      { id: 1, title: 'Test Movie 1', description: 'Desc 1', rating: 8 },
      { id: 2, title: 'Test Movie 2', description: 'Desc 2', rating: 9 },
    ];
    mockUseMovies.mockReturnValue({
      getMovies: () => ({ data: movies, isLoading: false }),
      addMovie: { mutateAsync: addMovieMock },
      updateMovie: { mutateAsync: updateMovieMock },
      deleteMovie: { mutateAsync: deleteMovieMock },
    });

    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('Test Movie 1')).toBeInTheDocument();
      expect(screen.getByText('Test Movie 2')).toBeInTheDocument();
    });
  });

  it('shows "No movies found" message when there are no movies', async () => {
    mockUseMovies.mockReturnValue({
      getMovies: () => ({ data: [], isLoading: false }),
      addMovie: { mutateAsync: addMovieMock },
      updateMovie: { mutateAsync: updateMovieMock },
      deleteMovie: { mutateAsync: deleteMovieMock },
    });

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

    mockUseMovies.mockReturnValue({
      getMovies: () => ({ data: [], isLoading: false }),
      addMovie: { mutateAsync: addMovieMock },
      updateMovie: { mutateAsync: updateMovieMock },
      deleteMovie: { mutateAsync: deleteMovieMock },
    });
    addMovieMock.mockResolvedValue({ data: {} });
    
    render(<App />);
    
    await userEvent.click(screen.getByText('+ Add Movie'));
    await userEvent.type(screen.getByPlaceholderText('Title'), 'New Test Movie');
    await userEvent.click(screen.getByRole('button', { name: /save movie/i }));

    await waitFor(() => {
      expect(addMovieMock).toHaveBeenCalledWith(expect.objectContaining({ title: 'New Test Movie' }));
    });
  });

  it('can start editing a movie (when authenticated)', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const movies = [{ id: 1, title: 'Movie to Edit', description: 'Desc', rating: 5 }];
    mockUseMovies.mockReturnValue({
      getMovies: () => ({ data: movies, isLoading: false }),
      addMovie: { mutateAsync: addMovieMock },
      updateMovie: { mutateAsync: updateMovieMock },
      deleteMovie: { mutateAsync: deleteMovieMock },
    });

    render(<App />);

    await waitFor(() => screen.getByText('Movie to Edit'));
    await userEvent.click(screen.getByTitle('Edit'));

    expect(screen.getByRole('heading', { name: 'Edit Movie' })).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Title')).toHaveValue('Movie to Edit');
  });

  it('can delete a movie (when authenticated)', async () => {
    mockUseAuthStore.mockReturnValue({
      isAuthenticated: true,
      user: { id: 1, username: 'testuser' },
      logout: vi.fn(),
    });

    const movies = [{ id: 1, title: 'Movie to Delete', description: 'Desc', rating: 5 }];
    mockUseMovies.mockReturnValue({
      getMovies: () => ({ data: movies, isLoading: false }),
      addMovie: { mutateAsync: addMovieMock },
      updateMovie: { mutateAsync: updateMovieMock },
      deleteMovie: { mutateAsync: deleteMovieMock },
    });
    deleteMovieMock.mockResolvedValue({ data: {} });
    window.confirm = vi.fn(() => true); // Auto-confirm deletion

    render(<App />);
    
    await waitFor(() => screen.getByText('Movie to Delete'));
    await userEvent.click(screen.getByTitle('Delete'));

    await waitFor(() => {
      expect(deleteMovieMock).toHaveBeenCalledWith(1);
    });
  });
});
