import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { FeedForm } from './FeedForm';
import { Feed } from '../types/feed';
import { vi } from 'vitest';

describe('FeedForm', () => {
  it('renders correctly for adding a new feed entry', () => {
    render(<FeedForm title="New Feed" onSubmit={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByText('New Feed')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Feed title')).toHaveValue('');
  });

  it('renders correctly for editing an existing feed entry', () => {
    const feed: Feed = { id: 1, user_id: 1, title: 'Existing Feed', description: 'Existing Desc', rating: 7, created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' };
    render(<FeedForm title="Edit Feed" feed={feed} onSubmit={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByText('Edit Feed')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Feed title')).toHaveValue('Existing Feed');
  });

  it('calls onSubmit with form data when creating a feed entry', async () => {
    const handleSubmit = vi.fn();
    render(<FeedForm title="New Feed" onSubmit={handleSubmit} onCancel={vi.fn()} />);

    await userEvent.type(screen.getByPlaceholderText('Feed title'), 'New Film');
    await userEvent.type(screen.getByPlaceholderText('Description'), 'A great film.');

    fireEvent.submit(screen.getByRole('button', { name: /save feed/i }));

    await waitFor(() => {
      expect(handleSubmit).toHaveBeenCalledWith({
        title: 'New Film',
        description: 'A great film.',
      });
    });
  });

  it('shows validation error when title is empty', async () => {
    render(<FeedForm title="New Feed" onSubmit={vi.fn()} onCancel={vi.fn()} />);

    fireEvent.submit(screen.getByRole('button', { name: /save feed/i }));

    expect(await screen.findByText('Title is required')).toBeInTheDocument();
  });

  it('calls onCancel when cancel button is clicked', async () => {
    const handleCancel = vi.fn();
    render(<FeedForm title="New Feed" onSubmit={vi.fn()} onCancel={handleCancel} />);
    await userEvent.click(screen.getByRole('button', { name: /cancel/i }));
    expect(handleCancel).toHaveBeenCalled();
  });
});