import { render, screen } from '@testing-library/react';
import { MovieList } from './MovieList';
import { Movie } from '../features/movies/types/movie';
import { vi } from 'vitest';

describe('MovieList', () => {
  const movies: Movie[] = [
    { id: 1, title: 'Movie 1', description: 'Desc 1', rating: 8 },
    { id: 2, title: 'Movie 2', description: 'Desc 2', rating: 9 },
  ];

  it('renders a list of movies', () => {
    render(
      <MovieList
        movies={movies}
        loading={false}
        searchTerm=""
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />
    );
    expect(screen.getByText('Movie 1')).toBeInTheDocument();
    expect(screen.getByText('Movie 2')).toBeInTheDocument();
  });

  it('renders loading state', () => {
    render(
      <MovieList
        movies={[]}
        loading={true}
        searchTerm=""
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />
    );
    expect(screen.getByText('Loading movies...')).toBeInTheDocument();
  });

  it('renders no movies found message', () => {
    render(
      <MovieList
        movies={[]}
        loading={false}
        searchTerm="nonexistent"
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />
    );
    expect(screen.getByText('No movies found matching "nonexistent"')).toBeInTheDocument();
  });
});
