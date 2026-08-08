import { z } from 'zod';
import type { Movie as ApiMovie, MoviesPage as ApiMoviesPage } from '~/api/openapi';

// Define the validation schema for a movie
export const movieSchema = z.object({
  title: z.string().min(1, 'Title is required').max(100, 'Title is too long'),
  description: z.string().max(1000, 'Description is too long').optional().default(''),
  rating: z.coerce.number().min(0, 'Rating must be at least 0').max(10, 'Rating cannot exceed 10'),
});

// Derive TypeScript types from the schema
export type MovieFormData = z.infer<typeof movieSchema>;

export type NewMovie = MovieFormData;

// Wire types — sourced from the generated OpenAPI schema so the frontend
// stays in lockstep with the backend. `Movie` is huma's response shape
// (id + timestamps are server-controlled); `MoviesPage` is the pagination
// envelope returned by GET /api/movies.
export type Movie = ApiMovie;
export type PaginatedMovies = ApiMoviesPage;
