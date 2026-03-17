import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { movieService } from '../services/movieService';
import { Movie, NewMovie } from '../types/movie';

export const useMovies = () => {
  const queryClient = useQueryClient();

  const getMovies = (searchTerm: string) => 
    useQuery({
      queryKey: ['movies', searchTerm],
      queryFn: () => movieService.getMovies(searchTerm),
    });

  const addMovie = useMutation({
    mutationFn: (movie: NewMovie) => movieService.addMovie(movie),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['movies'] });
    },
  });

  const updateMovie = useMutation({
    mutationFn: (movie: Movie) => movieService.updateMovie(movie.id, movie),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['movies'] });
    },
  });

  const deleteMovie = useMutation({
    mutationFn: (id: number) => movieService.deleteMovie(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['movies'] });
    },
  });

  return { getMovies, addMovie, updateMovie, deleteMovie };
};
