import React, { useState, useEffect } from 'react';
import { Movie, NewMovie } from '../types/movie';

interface MovieFormProps {
  movie?: Movie | null;
  onSubmit: (movie: NewMovie | Movie) => void;
  onCancel: () => void;
  title: string;
}

export const MovieForm: React.FC<MovieFormProps> = ({ movie, onSubmit, onCancel, title }) => {
  const [formData, setFormData] = useState<NewMovie>({
    title: '',
    description: '',
    rating: 5.0,
  });

  useEffect(() => {
    if (movie) {
      setFormData({
        title: movie.title,
        description: movie.description,
        rating: movie.rating,
      });
    } else {
      setFormData({
        title: '',
        description: '',
        rating: 5.0,
      });
    }
  }, [movie]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (movie) {
      onSubmit({ ...movie, ...formData, rating: Number(formData.rating) });
    } else {
      onSubmit({ ...formData, rating: Number(formData.rating) });
    }
  };

  return (
    <form className={`add-movie-form ${movie ? 'edit-form' : ''}`} onSubmit={handleSubmit}>
      <h2>{title}</h2>
      <input
        type="text"
        placeholder="Title"
        value={formData.title}
        onChange={(e) => setFormData({ ...formData, title: e.target.value })}
        required
      />
      <textarea
        placeholder="Description"
        value={formData.description}
        onChange={(e) => setFormData({ ...formData, description: e.target.value })}
      />
      <div className="rating-input">
        <label htmlFor="rating">Rating: {formData.rating}</label>
        <input
          id="rating"
          type="range"
          min="0"
          max="10"
          step="0.1"
          value={formData.rating}
          onChange={(e) => setFormData({ ...formData, rating: Number(e.target.value) })}
        />
      </div>
      <div className="form-actions">
        <button type="submit" className="submit-button">
          {movie ? 'Update Movie' : 'Save Movie'}
        </button>
        <button type="button" className="cancel-button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
};
