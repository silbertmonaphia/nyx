import React from 'react';
import { Movie } from '../types/movie';

interface MovieItemProps {
  movie: Movie;
  onEdit: (movie: Movie) => void;
  onDelete: (id: number) => void;
}

export const MovieItem: React.FC<MovieItemProps> = ({ movie, onEdit, onDelete }) => {
  return (
    <div className="movie-item">
      <div className="movie-content">
        <h3>{movie.title}</h3>
        <p>{movie.description}</p>
        <span className="rating">★ {movie.rating}</span>
      </div>
      <div className="movie-actions">
        <button 
          className="edit-icon-button"
          onClick={() => onEdit(movie)}
          title="Edit"
        >
          ✎
        </button>
        <button 
          className="delete-icon-button"
          onClick={() => onDelete(movie.id)}
          title="Delete"
        >
          ×
        </button>
      </div>
    </div>
  );
};

interface MovieListProps {
  movies: Movie[];
  loading: boolean;
  searchTerm: string;
  onEdit: (movie: Movie) => void;
  onDelete: (id: number) => void;
}

export const MovieList: React.FC<MovieListProps> = ({ movies, loading, searchTerm, onEdit, onDelete }) => {
  if (loading) {
    return <p>Loading movies...</p>;
  }

  if (movies.length === 0) {
    return <p>No movies found {searchTerm && `matching "${searchTerm}"`}</p>;
  }

  return (
    <div className="movie-list">
      {movies.map((movie) => (
        <MovieItem 
          key={movie.id} 
          movie={movie} 
          onEdit={onEdit} 
          onDelete={onDelete} 
        />
      ))}
    </div>
  );
};
