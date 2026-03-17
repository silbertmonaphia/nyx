import { Movie, NewMovie } from '../types/movie';

const API_BASE_URL = 'http://localhost:8080/api';

export const movieService = {
  async getMovies(searchTerm: string = ''): Promise<Movie[]> {
    const url = `${API_BASE_URL}/movies${searchTerm ? `?q=${encodeURIComponent(searchTerm)}` : ''}`;
    const response = await fetch(url);
    if (!response.ok) {
      throw new Error(`Error fetching movies: ${response.statusText}`);
    }
    const data = await response.json();
    return data || [];
  },

  async addMovie(movie: NewMovie): Promise<Movie> {
    const response = await fetch(`${API_BASE_URL}/movies`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(movie),
    });
    if (!response.ok) {
      throw new Error(`Error adding movie: ${response.statusText}`);
    }
    return response.json();
  },

  async updateMovie(id: number, movie: Partial<Movie>): Promise<Movie> {
    const response = await fetch(`${API_BASE_URL}/movies/${id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(movie),
    });
    if (!response.ok) {
      throw new Error(`Error updating movie: ${response.statusText}`);
    }
    return response.json();
  },

  async deleteMovie(id: number): Promise<void> {
    const response = await fetch(`${API_BASE_URL}/movies/${id}`, {
      method: 'DELETE',
    });
    if (!response.ok) {
      throw new Error(`Error deleting movie: ${response.statusText}`);
    }
  },
};
