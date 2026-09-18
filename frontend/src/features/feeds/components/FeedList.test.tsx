import { render, screen } from '@testing-library/react';
import { FeedList } from './FeedList';
import { Feed } from '../types/feed';
import { vi } from 'vitest';

describe('FeedList', () => {
  const feeds: Feed[] = [
    { id: 1, title: 'Feed 1', description: 'Desc 1', rating: 8, created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
    { id: 2, title: 'Feed 2', description: 'Desc 2', rating: 9, created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
  ];

  it('renders a list of feeds', () => {
    render(
      <FeedList
        feeds={feeds}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('feed-list')).toBeInTheDocument();
    expect(screen.getByText('Feed 1')).toBeInTheDocument();
    expect(screen.getByText('Feed 2')).toBeInTheDocument();
  });

  it('renders created and updated timestamps for each feed', () => {
    render(
      <FeedList
        feeds={feeds}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    const createdLabels = screen.getAllByText(/^Created /);
    const updatedLabels = screen.getAllByText(/^Updated /);
    expect(createdLabels).toHaveLength(2);
    expect(updatedLabels).toHaveLength(2);
    // <time> elements expose dateTime so screen readers see ISO.
    expect(createdLabels[0].tagName.toLowerCase()).toBe('time');
    expect(createdLabels[0].getAttribute('datetime')).toBe('2024-01-01T00:00:00Z');
    expect(updatedLabels[0].getAttribute('datetime')).toBe('2024-01-01T00:00:00Z');
  });

  it('renders skeleton placeholders while loading', () => {
    render(
      <FeedList
        feeds={[]}
        loading={true}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('feed-list-skeleton')).toBeInTheDocument();
  });

  it('renders no feeds found message', () => {
    render(
      <FeedList
        feeds={[]}
        loading={false}
        searchTerm="nonexistent"
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByText('No feeds found matching "nonexistent"')).toBeInTheDocument();
  });

  it('renders the empty state without the search qualifier when no search term', () => {
    render(
      <FeedList
        feeds={[]}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByText('No feeds found')).toBeInTheDocument();
  });

  it('renders the infinite-scroll sentinel when there is more data', () => {
    render(
      <FeedList
        feeds={feeds}
        loading={false}
        searchTerm=""
        hasMore={true}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('feed-list-sentinel')).toBeInTheDocument();
  });

  it('does not render the sentinel when there is no more data', () => {
    render(
      <FeedList
        feeds={feeds}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.queryByTestId('feed-list-sentinel')).not.toBeInTheDocument();
  });
});