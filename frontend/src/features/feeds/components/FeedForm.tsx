import React, { useEffect } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { Feed, feedSchema, FeedFormData } from '../types/feed';
import { Button } from '~/components/ui/Button';
import { Input } from '~/components/ui/Input';
import { Textarea } from '~/components/ui/Textarea';
import { Label } from '~/components/ui/Label';
import { Card, CardHeader, CardTitle, CardContent, CardFooter } from '~/components/ui/Card';

interface FeedFormProps {
  feed?: Feed | null;
  onSubmit: (data: FeedFormData | Feed) => void;
  onCancel: () => void;
  title: string;
}

export const FeedForm: React.FC<FeedFormProps> = ({ feed, onSubmit, onCancel, title }) => {
  const {
    register,
    handleSubmit,
    reset,
    formState: { errors },
  } = useForm<FeedFormData>({
    resolver: zodResolver(feedSchema),
    defaultValues: {
      title: '',
      description: '',
    },
  });

  useEffect(() => {
    if (feed) {
      reset({
        title: feed.title,
        description: feed.description || '',
      });
    } else {
      reset({
        title: '',
        description: '',
      });
    }
  }, [feed, reset]);

  const onFormSubmit = (data: FeedFormData) => {
    if (feed) {
      onSubmit({ ...feed, ...data });
    } else {
      onSubmit(data);
    }
  };

  return (
    <Card className={`w-full max-w-[600px] my-4 ${feed ? 'border-primary shadow-md' : ''}`}>
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
              placeholder="Feed title"
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
        </CardContent>
        <CardFooter className="flex gap-2">
          <Button type="submit" className="flex-1">
            {feed ? 'Update Feed' : 'Save Feed'}
          </Button>
          <Button type="button" variant="outline" onClick={onCancel} className="flex-1">
            Cancel
          </Button>
        </CardFooter>
      </form>
    </Card>
  );
};