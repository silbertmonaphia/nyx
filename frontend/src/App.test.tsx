import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import App from './App';
import { vi } from 'vitest';
import { useMovies } from './features/movies/hooks/useMovies';

vi.mock('./features/movies/hooks/useMovies');

const mockUseMovies = useMovies as vi.Mock;

describe('App', () => {
  let addMovieMock: vi.Mock;
  let updateMovieMock: vi.Mock;
  let deleteMovieMock: vi.Mock;

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
  });

  it('renders the main title', () => {
    render(<App />);
    expect(screen.getByText('Nyx')).toBeInTheDocument();
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

  it('can add a new movie', async () => {
    mockUseMovies.mockReturnValue({
      getMovies: () => ({ data: [], isLoading: false }),
      addMovie: { mutateAsync: addMovieMock },
      updateMovie: { mutateAsync: updateMovieMock },
      deleteMovie: { mutateAsync: deleteMovieMock },
    });
    addMovieMock.mockResolvedValue({});
    
    render(<App />);
    
    await userEvent.click(screen.getByText('+ Add Movie'));
    await userEvent.type(screen.getByPlaceholderText('Title'), 'New Test Movie');
    await userEvent.click(screen.getByRole('button', { name: /save movie/i }));

    await waitFor(() => {
      expect(addMovieMock).toHaveBeenCalledWith(expect.objectContaining({ title: 'New Test Movie' }));
    });
  });

  it('can start editing a movie', async () => {
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

  it('can delete a movie', async () => {
    const movies = [{ id: 1, title: 'Movie to Delete', description: 'Desc', rating: 5 }];
    mockUseMovies.mockReturnValue({
      getMovies: () => ({ data: movies, isLoading: false }),
      addMovie: { mutateAsync: addMovieMock },
      updateMovie: { mutateAsync: updateMovieMock },
      deleteMovie: { mutateAsync: deleteMovieMock },
    });
    deleteMovieMock.mockResolvedValue({});
    window.confirm = vi.fn(() => true); // Auto-confirm deletion

    render(<App />);
    
    await waitFor(() => screen.getByText('Movie to Delete'));
    await userEvent.click(screen.getByTitle('Delete'));

    await waitFor(() => {
      expect(deleteMovieMock).toHaveBeenCalledWith(1);
    });
  });
});
