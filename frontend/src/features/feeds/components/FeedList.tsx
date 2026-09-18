import React, { useEffect, useRef } from 'react';
import { Feed } from '../types/feed';
import { Card, CardHeader, CardTitle, CardContent } from '~/components/ui/Card';
import { Button } from '~/components/ui/Button';
import { Pencil, Trash2 } from 'lucide-react';
import { formatRelativeTime } from '~/utils/date';
import { FeedForm } from './FeedForm';

interface FeedItemProps {
  feed: Feed;
  onEdit: (feed: Feed) => void;
  onDelete: (id: number) => void;
}

export const FeedItem: React.FC<FeedItemProps> = ({ feed, onEdit, onDelete }) => {
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
  loading: boolean;
  searchTerm: string;
  hasMore: boolean;
  isLoadingMore: boolean;
  onLoadMore: () => void;
  editingFeed: Feed | null;
  onUpdate: (data: Feed) => void;
  onCancelEdit: () => void;
  onEdit: (feed: Feed) => void;
  onDelete: (id: number) => void;
}

export const FeedList: React.FC<FeedListProps> = ({
  feeds,
  loading,
  searchTerm,
  hasMore,
  isLoadingMore,
  onLoadMore,
  editingFeed,
  onUpdate,
  onCancelEdit,
  onEdit,
  onDelete,
}) => {
  const sentinelRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!hasMore || isLoadingMore) return;
    const node = sentinelRef.current;
    if (!node) return;

    const observer = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) {
          onLoadMore();
        }
      },
      { rootMargin: '200px' },
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, [hasMore, isLoadingMore, onLoadMore]);

  if (loading) {
    return (
      <div
        data-testid="feed-list-skeleton"
        className="flex flex-col gap-4 w-full max-w-[600px] my-8 text-left"
      >
        <FeedListSkeleton />
      </div>
    );
  }

  if (feeds.length === 0) {
    return (
      <div
        data-testid="feed-list-empty"
        className="py-10 text-center text-muted-foreground bg-secondary/20 rounded-lg border border-dashed border-border"
      >
        No feeds found {searchTerm && `matching "${searchTerm}"`}
      </div>
    );
  }

  return (
    <div
      data-testid="feed-list"
      className="flex flex-col gap-4 w-full max-w-[600px] my-8 text-left"
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
  );
};