import { z } from 'zod';

// Define the validation schema for a movie
export const movieSchema = z.object({
  title: z.string().min(1, 'Title is required').max(100, 'Title is too long'),
  description: z.string().max(1000, 'Description is too long').optional().default(''),
  rating: z.coerce.number().min(0, 'Rating must be at least 0').max(10, 'Rating cannot exceed 10'),
});

// Derive TypeScript types from the schema
export type MovieFormData = z.infer<typeof movieSchema>;

export interface Movie extends MovieFormData {
  id: number;
  created_at?: string;
  updated_at?: string;
}

export type NewMovie = MovieFormData;
