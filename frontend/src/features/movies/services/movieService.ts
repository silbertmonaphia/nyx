import { Movie, NewMovie, PaginatedMovies } from '../types/movie';
import api from '~/services/api'; // Use the alias here

const DEFAULT_PAGE_SIZE = 20;

export const movieService = {
  async getMovies(
    searchTerm: string = '',
    page: number = 1,
    pageSize: number = DEFAULT_PAGE_SIZE,
  ): Promise<PaginatedMovies> {
    const params = new URLSearchParams();
    if (searchTerm) params.set('q', searchTerm);
    params.set('page', String(page));
    params.set('page_size', String(pageSize));
    const response = await api.get(`/movies?${params.toString()}`);
    return response.data;
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
