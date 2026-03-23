import React, { useEffect } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { Movie, movieSchema, MovieFormData } from '../types/movie';

interface MovieFormProps {
  movie?: Movie | null;
  onSubmit: (data: MovieFormData | Movie) => void;
  onCancel: () => void;
  title: string;
}

export const MovieForm: React.FC<MovieFormProps> = ({ movie, onSubmit, onCancel, title }) => {
  const {
    register,
    handleSubmit,
    reset,
    formState: { errors },
    watch,
  } = useForm<MovieFormData>({
    resolver: zodResolver(movieSchema),
    defaultValues: {
      title: '',
      description: '',
      rating: 5.0,
    },
  });

  // Watch the rating field to display its value
  const currentRating = watch('rating');

  useEffect(() => {
    if (movie) {
      reset({
        title: movie.title,
        description: movie.description || '',
        rating: movie.rating,
      });
    } else {
      reset({
        title: '',
        description: '',
        rating: 5.0,
      });
    }
  }, [movie, reset]);

  const onFormSubmit = (data: MovieFormData) => {
    if (movie) {
      onSubmit({ ...movie, ...data });
    } else {
      onSubmit(data);
    }
  };

  return (
    <form className={`add-movie-form ${movie ? 'edit-form' : ''}`} onSubmit={handleSubmit(onFormSubmit)}>
      <h2>{title}</h2>
      
      <div className="form-field">
        <input
          {...register('title')}
          type="text"
          placeholder="Title"
          aria-invalid={!!errors.title}
        />
        {errors.title && <span className="error-message">{errors.title.message}</span>}
      </div>

      <div className="form-field">
        <textarea
          {...register('description')}
          placeholder="Description"
          aria-invalid={!!errors.description}
        />
        {errors.description && <span className="error-message">{errors.description.message}</span>}
      </div>

      <div className="rating-input">
        <label htmlFor="rating">Rating: {currentRating}</label>
        <input
          {...register('rating')}
          id="rating"
          type="range"
          min="0"
          max="10"
          step="0.1"
        />
        {errors.rating && <span className="error-message">{errors.rating.message}</span>}
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
