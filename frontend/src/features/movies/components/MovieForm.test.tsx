import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MovieForm } from './MovieForm';
import { Movie } from '../features/movies/types/movie';
import { vi } from 'vitest';

describe('MovieForm', () => {
  it('renders correctly for adding a new movie', () => {
    render(<MovieForm title="New Movie" onSubmit={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByText('New Movie')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Title')).toHaveValue('');
  });

  it('renders correctly for editing an existing movie', () => {
    const movie: Movie = { id: 1, title: 'Existing Movie', description: 'Existing Desc', rating: 7 };
    render(<MovieForm title="Edit Movie" movie={movie} onSubmit={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByText('Edit Movie')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Title')).toHaveValue('Existing Movie');
  });

  it('calls onSubmit with form data when creating a movie', async () => {
    const handleSubmit = vi.fn();
    render(<MovieForm title="New Movie" onSubmit={handleSubmit} onCancel={vi.fn()} />);
    
    await userEvent.type(screen.getByPlaceholderText('Title'), 'New Film');
    await userEvent.type(screen.getByPlaceholderText('Description'), 'A great film.');
    
    fireEvent.submit(screen.getByRole('button', { name: /save movie/i }));
    
    expect(handleSubmit).toHaveBeenCalledWith({
      title: 'New Film',
      description: 'A great film.',
      rating: 5, // Default
    });
  });

  it('calls onSubmit with updated form data when editing a movie', async () => {
    const movie: Movie = { id: 1, title: 'Old Title', description: 'Old Desc', rating: 7 };
    const handleSubmit = vi.fn();
    render(<MovieForm title="Edit Movie" movie={movie} onSubmit={handleSubmit} onCancel={vi.fn()} />);

    const titleInput = screen.getByPlaceholderText('Title');
    await userEvent.clear(titleInput);
    await userEvent.type(titleInput, 'Updated Title');
    
    fireEvent.submit(screen.getByRole('button', { name: /update movie/i }));

    expect(handleSubmit).toHaveBeenCalledWith({
      id: 1,
      title: 'Updated Title',
      description: 'Old Desc',
      rating: 7,
    });
  });

  it('calls onCancel when cancel button is clicked', async () => {
    const handleCancel = vi.fn();
    render(<MovieForm title="New Movie" onSubmit={vi.fn()} onCancel={handleCancel} />);
    await userEvent.click(screen.getByRole('button', { name: /cancel/i }));
    expect(handleCancel).toHaveBeenCalled();
  });
});
