import React from 'react';
import { Movie } from '../types/movie';
import { Card, CardHeader, CardTitle, CardContent } from '~/components/ui/Card';
import { Button } from '~/components/ui/Button';
import { Pencil, Trash2 } from 'lucide-react';

interface MovieItemProps {
  movie: Movie;
  onEdit: (movie: Movie) => void;
  onDelete: (id: number) => void;
}

export const MovieItem: React.FC<MovieItemProps> = ({ movie, onEdit, onDelete }) => {
  return (
    <Card className="hover:shadow-md transition-shadow">
      <CardHeader className="flex flex-row items-start justify-between space-y-0 pb-2">
        <div className="space-y-1">
          <CardTitle className="text-xl">{movie.title}</CardTitle>
          <div className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-semibold bg-primary/10 text-primary">
            ★ {movie.rating}
          </div>
        </div>
        <div className="flex gap-1">
          <Button 
            variant="ghost" 
            size="icon" 
            onClick={() => onEdit(movie)}
            title="Edit"
            className="h-8 w-8 text-muted-foreground hover:text-primary"
          >
            <Pencil className="h-4 w-4" />
          </Button>
          <Button 
            variant="ghost" 
            size="icon" 
            onClick={() => onDelete(movie.id)}
            title="Delete"
            className="h-8 w-8 text-muted-foreground hover:text-destructive"
          >
            <Trash2 className="h-4 w-4" />
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        <p className="text-sm text-muted-foreground leading-relaxed">
          {movie.description}
        </p>
      </CardContent>
    </Card>
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
    return <div className="py-10 text-center text-muted-foreground">Loading movies...</div>;
  }

  if (movies.length === 0) {
    return (
      <div className="py-10 text-center text-muted-foreground bg-secondary/20 rounded-lg border border-dashed border-border">
        No movies found {searchTerm && `matching "${searchTerm}"`}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4 w-full max-w-[600px] my-8 text-left">
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
