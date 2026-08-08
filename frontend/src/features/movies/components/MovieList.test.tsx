import { render, screen } from '@testing-library/react';
import { MovieList } from './MovieList';
import { Movie } from '../types/movie';
import { vi } from 'vitest';

describe('MovieList', () => {
  const movies: Movie[] = [
    { id: 1, title: 'Movie 1', description: 'Desc 1', rating: 8, created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
    { id: 2, title: 'Movie 2', description: 'Desc 2', rating: 9, created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
  ];

  it('renders a list of movies', () => {
    render(
      <MovieList
        movies={movies}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('movie-list')).toBeInTheDocument();
    expect(screen.getByText('Movie 1')).toBeInTheDocument();
    expect(screen.getByText('Movie 2')).toBeInTheDocument();
  });

  it('renders skeleton placeholders while loading', () => {
    render(
      <MovieList
        movies={[]}
        loading={true}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('movie-list-skeleton')).toBeInTheDocument();
  });

  it('renders no movies found message', () => {
    render(
      <MovieList
        movies={[]}
        loading={false}
        searchTerm="nonexistent"
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByText('No movies found matching "nonexistent"')).toBeInTheDocument();
  });

  it('renders the empty state without the search qualifier when no search term', () => {
    render(
      <MovieList
        movies={[]}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByText('No movies found')).toBeInTheDocument();
  });

  it('renders the infinite-scroll sentinel when there is more data', () => {
    render(
      <MovieList
        movies={movies}
        loading={false}
        searchTerm=""
        hasMore={true}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('movie-list-sentinel')).toBeInTheDocument();
  });

  it('does not render the sentinel when there is no more data', () => {
    render(
      <MovieList
        movies={movies}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.queryByTestId('movie-list-sentinel')).not.toBeInTheDocument();
  });
});