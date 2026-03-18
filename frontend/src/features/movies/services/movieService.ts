import { Movie, NewMovie } from '../types/movie';
import api from '~/services/api'; // Use the alias here

export const movieService = {
  async getMovies(searchTerm: string = ''): Promise<Movie[]> {
    const response = await api.get(`/movies${searchTerm ? `?q=${encodeURIComponent(searchTerm)}` : ''}`);
    return response.data || [];
  },

  async addMovie(movie: NewMovie): Promise<Movie> {
    const response = await api.post('/movies', movie);
    return response.data;
  },

  async updateMovie(id: number, movie: Partial<Movie>): Promise<Movie> {
    const response = await api.put(`/movies/${id}`, movie);
    return response.data;
  },

  async deleteMovie(id: number): Promise<void> {
    await api.delete(`/movies/${id}`);
  },
};
