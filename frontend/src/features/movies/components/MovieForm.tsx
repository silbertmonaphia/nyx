import React, { useEffect } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { Movie, movieSchema, MovieFormData } from '../types/movie';
import { Button } from '~/components/ui/Button';
import { Input } from '~/components/ui/Input';
import { Textarea } from '~/components/ui/Textarea';
import { Label } from '~/components/ui/Label';
import { Card, CardHeader, CardTitle, CardContent, CardFooter } from '~/components/ui/Card';

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
    const formattedData = {
      ...data,
      rating: typeof data.rating === 'string' ? parseFloat(data.rating) : data.rating
    };
    if (movie) {
      onSubmit({ ...movie, ...formattedData });
    } else {
      onSubmit(formattedData);
    }
  };

  return (
    <Card className={`w-full max-w-[600px] my-4 ${movie ? 'border-primary shadow-md' : ''}`}>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <form onSubmit={handleSubmit(onFormSubmit)}>
        <CardContent className="space-y-4">
          <div className="space-y-2 text-left">
            <Label htmlFor="title">Title</Label>
            <Input
              id="title"
              {...register('title')}
              placeholder="Movie title"
              aria-invalid={!!errors.title}
              className={errors.title ? "border-destructive focus-visible:ring-destructive" : ""}
            />
            {errors.title && <p className="text-xs font-medium text-destructive">{errors.title.message}</p>}
          </div>

          <div className="space-y-2 text-left">
            <Label htmlFor="description">Description</Label>
            <Textarea
              id="description"
              {...register('description')}
              placeholder="Description"
              aria-invalid={!!errors.description}
              className={errors.description ? "border-destructive focus-visible:ring-destructive" : ""}
            />
            {errors.description && <p className="text-xs font-medium text-destructive">{errors.description.message}</p>}
          </div>

          <div className="space-y-2 text-left">
            <div className="flex justify-between items-center">
              <Label htmlFor="rating">Rating</Label>
              <span className="text-sm font-bold text-primary">{currentRating}</span>
            </div>
            <input
              {...register('rating')}
              id="rating"
              type="range"
              min="0"
              max="10"
              step="0.1"
              className="w-full h-2 bg-secondary rounded-lg appearance-none cursor-pointer accent-primary"
            />
            {errors.rating && <p className="text-xs font-medium text-destructive">{errors.rating.message}</p>}
          </div>
        </CardContent>
        <CardFooter className="flex gap-2">
          <Button type="submit" className="flex-1">
            {movie ? 'Update Movie' : 'Save Movie'}
          </Button>
          <Button type="button" variant="outline" onClick={onCancel} className="flex-1">
            Cancel
          </Button>
        </CardFooter>
      </form>
    </Card>
  );
};
