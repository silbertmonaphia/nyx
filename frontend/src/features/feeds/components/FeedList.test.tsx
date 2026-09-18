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
        totalCount={feeds.length}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
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
        totalCount={feeds.length}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
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
        totalCount={0}
        loading={true}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
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
        totalCount={0}
        loading={false}
        searchTerm="nonexistent"
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByText('Feed "nonexistent" not found')).toBeInTheDocument();
  });

  it('renders the empty state without the search qualifier when no search term', () => {
    render(
      <FeedList
        feeds={[]}
        totalCount={0}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
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
        totalCount={feeds.length}
        loading={false}
        searchTerm=""
        hasMore={true}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
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
        totalCount={feeds.length}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.queryByTestId('feed-list-sentinel')).not.toBeInTheDocument();
  });

  it('renders the edit form inline in place of the edited row', () => {
    render(
      <FeedList
        feeds={feeds}
        totalCount={feeds.length}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={feeds[0]}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    // The edited row is replaced by the form; the other row stays as a card.
    expect(screen.queryByText('Feed 1')).not.toBeInTheDocument();
    expect(screen.getByText('Feed 2')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Edit Feed' })).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Feed title')).toHaveValue('Feed 1');
  });

  it('renders a fixed-height scrollable panel for the loaded state', () => {
    render(
      <FeedList
        feeds={feeds}
        totalCount={feeds.length}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    const root = screen.getByTestId('feed-list');
    expect(root.className).toMatch(/overflow-y-auto/);
    expect(root.className).toMatch(/min-h-0/);
    expect(root.className).toMatch(/flex-1/);
  });

  it('renders a fixed-height scrollable panel for the skeleton state', () => {
    render(
      <FeedList
        feeds={[]}
        totalCount={0}
        loading={true}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    const root = screen.getByTestId('feed-list-skeleton');
    expect(root.className).toMatch(/overflow-y-auto/);
    expect(root.className).toMatch(/min-h-0/);
  });

  it('renders a fixed-height scrollable panel for the empty state', () => {
    render(
      <FeedList
        feeds={[]}
        totalCount={0}
        loading={false}
        searchTerm="nope"
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    const root = screen.getByTestId('feed-list-empty');
    expect(root.className).toMatch(/overflow-y-auto/);
    expect(root.className).toMatch(/min-h-0/);
    expect(screen.getByText('Feed "nope" not found')).toBeInTheDocument();
  });

  it('renders the loaded feed count above the list', () => {
    render(
      <FeedList
        feeds={feeds}
        totalCount={2}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('feed-count')).toHaveTextContent('2 feeds loaded');
  });

  it('renders "1 feed loaded" (singular) when only one feed is loaded', () => {
    render(
      <FeedList
        feeds={[feeds[0]]}
        totalCount={1}
        loading={false}
        searchTerm=""
        hasMore={false}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('feed-count')).toHaveTextContent('1 feed loaded');
  });

  it('renders "X of Y feeds loaded" when more pages remain', () => {
    render(
      <FeedList
        feeds={feeds}
        totalCount={42}
        loading={false}
        searchTerm=""
        hasMore={true}
        isLoadingMore={false}
        onLoadMore={vi.fn()}
        editingFeed={null}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.getByTestId('feed-count')).toHaveTextContent('2 of 42 feeds loaded');
  });
});