import React, { useEffect, useLayoutEffect, useRef } from 'react';
import { Feed, NewFeed } from '../types/feed';
import { Card, CardHeader, CardTitle, CardContent } from '~/components/ui/Card';
import { Button } from '~/components/ui/Button';
import { Pencil, Trash2 } from 'lucide-react';
import { formatRelativeTime } from '~/utils/date';
import { FeedForm } from './FeedForm';

interface FeedItemProps {
  feed: Feed;
  // The id of the currently authenticated user, if any. Edit /
  // delete icons are hidden whenever it doesn't match the feed's
  // owner — the backend will 404 a cross-owner write anyway, so
  // surfacing the buttons only invites a confusing failure toast.
  currentUserId: number | undefined;
  onEdit: (feed: Feed) => void;
  onDelete: (id: number) => void;
}

export const FeedItem: React.FC<FeedItemProps> = ({
  feed,
  currentUserId,
  onEdit,
  onDelete,
}) => {
  const isOwner = feed.user_id === currentUserId;
  return (
    <Card className="hover:shadow-md transition-shadow">
      <CardHeader className="flex flex-row items-start justify-between space-y-0 pb-2">
        <div className="space-y-1">
          <CardTitle className="text-xl">{feed.title}</CardTitle>
          <div className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-semibold bg-primary/10 text-primary">
            ★ {feed.rating}
          </div>
        </div>
        <div className="flex flex-col items-end gap-2">
          {(feed.created_at || feed.updated_at) && (
            <div
              className="flex flex-col items-end text-xs text-muted-foreground leading-tight"
              data-testid="feed-timestamps"
            >
              {feed.created_at && (
                <time
                  dateTime={feed.created_at}
                  title={new Date(feed.created_at).toLocaleString()}
                >
                  Created {formatRelativeTime(feed.created_at)}
                </time>
              )}
              {feed.updated_at && (
                <time
                  dateTime={feed.updated_at}
                  title={new Date(feed.updated_at).toLocaleString()}
                >
                  Updated {formatRelativeTime(feed.updated_at)}
                </time>
              )}
            </div>
          )}
          {isOwner && (
            <div className="flex gap-1">
              <Button
                variant="ghost"
                size="icon"
                onClick={() => onEdit(feed)}
                title="Edit"
                className="h-8 w-8 text-muted-foreground hover:text-primary"
              >
                <Pencil className="h-4 w-4" />
              </Button>
              <Button
                variant="ghost"
                size="icon"
                onClick={() => onDelete(feed.id)}
                title="Delete"
                className="h-8 w-8 text-muted-foreground hover:text-destructive"
              >
                <Trash2 className="h-4 w-4" />
              </Button>
            </div>
          )}
        </div>
      </CardHeader>
      <CardContent>
        <p className="text-sm text-muted-foreground leading-relaxed">
          {feed.description}
        </p>
      </CardContent>
    </Card>
  );
};

const SkeletonCard: React.FC = () => (
  <Card className="animate-pulse">
    <CardHeader className="flex flex-row items-start justify-between space-y-0 pb-2">
      <div className="space-y-2 flex-1">
        <div className="h-5 w-2/3 rounded bg-muted" />
        <div className="h-4 w-16 rounded-full bg-muted" />
      </div>
    </CardHeader>
    <CardContent>
      <div className="space-y-2">
        <div className="h-3 w-full rounded bg-muted" />
        <div className="h-3 w-4/5 rounded bg-muted" />
      </div>
    </CardContent>
  </Card>
);

const FeedListSkeleton: React.FC = () => (
  <>
    {Array.from({ length: 3 }).map((_, i) => (
      <SkeletonCard key={i} />
    ))}
  </>
);

interface FeedListProps {
  feeds: Feed[];
  totalCount: number;
  loading: boolean;
  searchTerm: string;
  hasMore: boolean;
  isLoadingMore: boolean;
  onLoadMore: () => void;
  editingFeed: Feed | null;
  // Threaded down so each row can decide whether to surface its
  // edit/delete icons. `undefined` means logged-out — still hide the
  // buttons (auth check happens in App.tsx too).
  currentUserId: number | undefined;
  onUpdate: (data: NewFeed | Feed) => void;
  onCancelEdit: () => void;
  onEdit: (feed: Feed) => void;
  onDelete: (id: number) => void;
}

const FeedCount: React.FC<{ loaded: number; total: number }> = ({ loaded, total }) => {
  // Render the loaded count and, when pagination is in play (loaded < total),
  // append "of N" so the user knows there is more on the next page.
  const moreOnTheWay = total > loaded;
  return (
    <div
      data-testid="feed-count"
      className="text-xs text-muted-foreground self-start w-full max-w-[600px] mb-2"
      aria-live="polite"
    >
      {moreOnTheWay
        ? `${loaded} of ${total} feeds loaded`
        : `${loaded} ${loaded === 1 ? 'feed' : 'feeds'} loaded`}
    </div>
  );
};

export const FeedList: React.FC<FeedListProps> = ({
  feeds,
  totalCount,
  loading,
  searchTerm,
  hasMore,
  isLoadingMore,
  onLoadMore,
  editingFeed,
  currentUserId,
  onUpdate,
  onCancelEdit,
  onEdit,
  onDelete,
}) => {
  const sentinelRef = useRef<HTMLDivElement | null>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const didInitialScroll = useRef(false);
  const firstFeedIdRef = useRef<number | null>(null);

  useEffect(() => {
    if (!hasMore || isLoadingMore) return;
    const node = sentinelRef.current;
    const root = scrollRef.current;
    if (!node || !root) return;

    // Watch the scroll container itself, not the document viewport. The
    // sentinel is the LAST flex child of a `column-reverse` panel and the
    // browser's bounding-rect math for it doesn't track the inner scroll
    // reliably — pinning the observer's root to the panel makes the
    // intersection test deterministic regardless of where the panel sits
    // in the viewport or which edge the sentinel approaches.
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) {
          onLoadMore();
        }
      },
      { root, rootMargin: '200px' },
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, [hasMore, isLoadingMore, onLoadMore]);

  // Chat-style scroll: the newest feed sits at the visual bottom on first
  // render, and we re-stick to the bottom whenever the head of the list
  // changes (a new optimistic add landed at index 0). Infinite-scroll
  // history fetches append to the tail — same length grows, same head id —
  // so this effect won't yank the user away from history they're reading.
  useLayoutEffect(() => {
    const node = scrollRef.current;
    if (!node || loading) return;

    if (!didInitialScroll.current) {
      didInitialScroll.current = true;
      if (feeds.length > 0) {
        firstFeedIdRef.current = feeds[0].id;
        node.scrollTop = node.scrollHeight;
      }
      return;
    }

    const currentFirstId = feeds[0]?.id;
    if (currentFirstId !== undefined && currentFirstId !== firstFeedIdRef.current) {
      firstFeedIdRef.current = currentFirstId;
      node.scrollTop = node.scrollHeight;
    }
  }, [feeds, loading]);

  if (loading) {
    return (
      <div
        data-testid="feed-list-skeleton"
        className="no-scrollbar flex flex-col gap-4 w-full max-w-[600px] mb-8 text-left flex-1 min-h-0 overflow-y-auto"
      >
        <FeedListSkeleton />
      </div>
    );
  }

  if (feeds.length === 0) {
    return (
      <div
        data-testid="feed-list-empty"
        className="no-scrollbar w-full max-w-[600px] flex-1 min-h-0 overflow-y-auto flex flex-col items-center justify-center"
      >
        <div className="w-full py-10 text-center text-muted-foreground rounded-lg border border-dashed border-border">
          {searchTerm ? `Feed "${searchTerm}" not found` : 'No feeds found'}
        </div>
      </div>
    );
  }

  return (
    <>
      <FeedCount loaded={feeds.length} total={totalCount} />
      <div
        ref={scrollRef}
        data-testid="feed-list"
        className="no-scrollbar flex flex-col-reverse gap-4 w-full max-w-[600px] mb-8 text-left flex-1 min-h-0 overflow-y-auto"
      >
        {feeds.map((feed) =>
          editingFeed?.id === feed.id ? (
            <FeedForm
              key={feed.id}
              title="Edit Feed"
              feed={editingFeed}
              onSubmit={onUpdate}
              onCancel={onCancelEdit}
            />
          ) : (
            <FeedItem
              key={feed.id}
              feed={feed}
              currentUserId={currentUserId}
              onEdit={onEdit}
              onDelete={onDelete}
            />
          ),
        )}
        {hasMore && (
          <>
            {isLoadingMore && <FeedListSkeleton />}
            <div ref={sentinelRef} data-testid="feed-list-sentinel" className="h-1" />
          </>
        )}
      </div>
    </>
  );
};