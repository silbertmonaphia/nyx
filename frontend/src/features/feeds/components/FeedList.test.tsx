import { render, screen } from '@testing-library/react';
import { FeedList } from './FeedList';
import { Feed } from '../types/feed';
import { vi } from 'vitest';

// `IntersectionObserver` is polyfilled in test/setup.js. The stub keeps a
// reference to the most-recently constructed instance and exposes a
// `trigger()` helper so tests can simulate intersection callbacks without
// needing real layout.
const IntersectionObserverStub = globalThis.IntersectionObserver as unknown as {
  last: {
    trigger: (isIntersecting: boolean, target?: Element | null) => void;
    root: Element | null;
    rootMargin: string;
  } | null;
  reset: () => void;
};

describe('FeedList', () => {
  const feeds: Feed[] = [
    { id: 1, user_id: 1, title: 'Feed 1', description: 'Desc 1', rating: 8, created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
    { id: 2, user_id: 1, title: 'Feed 2', description: 'Desc 2', rating: 9, created_at: '2024-01-01T00:00:00Z', updated_at: '2024-01-01T00:00:00Z' },
  ];
  // Helper: every existing render call needs a `currentUserId`. Default
  // to "you own these rows" so the existing assertions stay meaningful;
  // the dedicated ownership tests below use a different id to exercise
  // the hide-when-not-owner branch.
  const renderList = (props: Partial<React.ComponentProps<typeof FeedList>> = {}) =>
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
        currentUserId={1}
        onUpdate={vi.fn()}
        onCancelEdit={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
        {...props}
      />,
    );

  beforeEach(() => {
    IntersectionObserverStub.reset();
  });

  it('renders a list of feeds', () => {
    renderList();
    expect(screen.getByTestId('feed-list')).toBeInTheDocument();
    expect(screen.getByText('Feed 1')).toBeInTheDocument();
    expect(screen.getByText('Feed 2')).toBeInTheDocument();
  });

  it('renders created and updated timestamps for each feed', () => {
    renderList();
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
    renderList({ feeds: [], totalCount: 0, loading: true });
    expect(screen.getByTestId('feed-list-skeleton')).toBeInTheDocument();
  });

  it('renders no feeds found message', () => {
    renderList({ feeds: [], totalCount: 0, searchTerm: 'nonexistent' });
    expect(screen.getByText('Feed "nonexistent" not found')).toBeInTheDocument();
  });

  it('renders the empty state without the search qualifier when no search term', () => {
    renderList({ feeds: [], totalCount: 0 });
    expect(screen.getByText('No feeds found')).toBeInTheDocument();
  });

  it('renders the infinite-scroll sentinel when there is more data', () => {
    renderList({ hasMore: true, totalCount: 42 });
    expect(screen.getByTestId('feed-list-sentinel')).toBeInTheDocument();
  });

  it('does not render the sentinel when there is no more data', () => {
    renderList();
    expect(screen.queryByTestId('feed-list-sentinel')).not.toBeInTheDocument();
  });

  it('renders the edit form inline in place of the edited row', () => {
    renderList({ editingFeed: feeds[0] });
    // The edited row is replaced by the form; the other row stays as a card.
    expect(screen.queryByText('Feed 1')).not.toBeInTheDocument();
    expect(screen.getByText('Feed 2')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Edit Feed' })).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Feed title')).toHaveValue('Feed 1');
  });

  it('renders a fixed-height scrollable panel for the loaded state', () => {
    renderList();
    const root = screen.getByTestId('feed-list');
    expect(root.className).toMatch(/overflow-y-auto/);
    expect(root.className).toMatch(/min-h-0/);
    expect(root.className).toMatch(/flex-1/);
  });

  it('renders a fixed-height scrollable panel for the skeleton state', () => {
    renderList({ feeds: [], totalCount: 0, loading: true });
    const root = screen.getByTestId('feed-list-skeleton');
    expect(root.className).toMatch(/overflow-y-auto/);
    expect(root.className).toMatch(/min-h-0/);
  });

  it('renders a fixed-height scrollable panel for the empty state', () => {
    renderList({ feeds: [], totalCount: 0, searchTerm: 'nope' });
    const root = screen.getByTestId('feed-list-empty');
    expect(root.className).toMatch(/overflow-y-auto/);
    expect(root.className).toMatch(/min-h-0/);
    expect(screen.getByText('Feed "nope" not found')).toBeInTheDocument();
  });

  it('renders the loaded feed count above the list', () => {
    renderList({ totalCount: 2 });
    expect(screen.getByTestId('feed-count')).toHaveTextContent('2 feeds loaded');
  });

  it('places the feed count outside the scrollable panel (chat-style layout)', () => {
    renderList();
    const count = screen.getByTestId('feed-count');
    const list = screen.getByTestId('feed-list');
    // The count must not be nested inside the scroll container —
    // column-reverse would otherwise push it to the visual bottom.
    expect(count.parentElement).not.toBe(list);
  });

  it('renders the list with a reversed column flex so the newest sits at the visual bottom', () => {
    renderList();
    const root = screen.getByTestId('feed-list');
    expect(root.className).toMatch(/flex-col-reverse/);
  });

  it('renders "1 feed loaded" (singular) when only one feed is loaded', () => {
    renderList({ feeds: [feeds[0]], totalCount: 1 });
    expect(screen.getByTestId('feed-count')).toHaveTextContent('1 feed loaded');
  });

  it('renders "X of Y feeds loaded" when more pages remain', () => {
    renderList({ totalCount: 42, hasMore: true });
    expect(screen.getByTestId('feed-count')).toHaveTextContent('2 of 42 feeds loaded');
  });

  it('renders edit + delete buttons when the current user owns the feed', () => {
    renderList({ currentUserId: 1 });
    // Two rows × one edit button + two rows × one delete button.
    expect(screen.getAllByTitle('Edit')).toHaveLength(2);
    expect(screen.getAllByTitle('Delete')).toHaveLength(2);
  });

  it('hides edit + delete buttons when the current user is not the feed owner', () => {
    // Same fixtures (user_id: 1) but the viewer is user 2 — the
    // backend would 404 a write; the UI shouldn't show them.
    renderList({ currentUserId: 2 });
    expect(screen.queryByTitle('Edit')).not.toBeInTheDocument();
    expect(screen.queryByTitle('Delete')).not.toBeInTheDocument();
  });

  it('hides edit + delete buttons when no user is authenticated', () => {
    renderList({ currentUserId: undefined });
    expect(screen.queryByTitle('Edit')).not.toBeInTheDocument();
    expect(screen.queryByTitle('Delete')).not.toBeInTheDocument();
  });

  describe('infinite-scroll observer', () => {
    it('pins the IntersectionObserver root to the scroll container, not the document viewport', () => {
      // Regression: the sentinel lives inside an `overflow-auto` +
      // `column-reverse` panel. Watching the document viewport is
      // unreliable here because the browser's bounding-rect math for the
      // LAST flex child of a column-reverse container doesn't track the
      // inner scroll predictably, so `isIntersecting` never flips.
      // Pinning `root` to the scroll container makes the intersection
      // test deterministic.
      renderList({ hasMore: true, totalCount: 42 });

      const observer = IntersectionObserverStub.last;
      const panel = screen.getByTestId('feed-list');
      expect(observer).not.toBeNull();
      expect(observer?.root).toBe(panel);
      expect(observer?.rootMargin).toBe('200px');
    });

    it('calls onLoadMore when the sentinel reports intersecting', () => {
      const onLoadMore = vi.fn();
      renderList({ hasMore: true, totalCount: 42, onLoadMore });

      expect(onLoadMore).not.toHaveBeenCalled();
      IntersectionObserverStub.last?.trigger(true);
      expect(onLoadMore).toHaveBeenCalledTimes(1);
    });

    it('does not call onLoadMore when the sentinel reports not intersecting', () => {
      const onLoadMore = vi.fn();
      renderList({ hasMore: true, totalCount: 42, onLoadMore });

      IntersectionObserverStub.last?.trigger(false);
      expect(onLoadMore).not.toHaveBeenCalled();
    });

    it('does not attach an observer when there are no more pages', () => {
      renderList();
      expect(IntersectionObserverStub.last).toBeNull();
    });
  });
});