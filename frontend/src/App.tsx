import React, { useState } from 'react';
import './App.css';
import { Feed, NewFeed } from './features/feeds/types/feed';
import { useFeeds } from './features/feeds/hooks/useFeeds';
import { FeedList } from './features/feeds/components/FeedList';
import { FeedForm } from './features/feeds/components/FeedForm';
import { useFeedUiStore } from './features/feeds/store/feedUiStore';
import { useDebounce } from './hooks/useDebounce';
import { useAuthReconciliation } from './hooks/useAuthReconciliation';
import { ToastContainer } from './components/app/ToastContainer';
import { useUiStore } from './store/uiStore';
import { useAuthStore } from './store/authStore';
import { AuthForm } from './features/auth/components/AuthForm';
import { Button } from './components/ui/Button';
import { Input } from './components/ui/Input';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from './components/ui/Dialog';
import { Plus, X, Search, LogOut, User as UserIcon, MessageSquare } from 'lucide-react';
import { PageMeta } from './components/app/PageMeta';
import { logger } from './services/logger';
import { ChatPanel } from './features/chat/components/ChatPanel';

const DEFAULT_TITLE = 'Nyx — Your minimalist feed guide';
const DEFAULT_DESCRIPTION =
  'A minimalist feed for discovering, tracking, and curating the films you love.';

function App() {
  const {
    searchTerm,
    setSearchTerm,
    showAddForm,
    setShowAddForm,
    editingFeed,
    setEditingFeed,
    resetFormState,
  } = useFeedUiStore();

  const { isAuthenticated, user, logout } = useAuthStore();
  const [showAuthForm, setShowAuthForm] = useState(false);
  const [confirmDeleteId, setConfirmDeleteId] = useState<number | null>(null);
  const [chatOpen, setChatOpen] = useState(false);

  // Boot-time round-trip: if localStorage has a persisted user,
  // reconcile it against the server. SECURITY.md L7.
  useAuthReconciliation();

  // The input stays fully controlled by `searchTerm` (instant typing),
  // but the network query only fires once the user has paused for
  // 300ms. Driving `useFeeds` off the debounced value keeps the
  // query key in sync with what we actually fetched, so the empty-state
  // qualifier and the loaded list agree.
  const debouncedSearchTerm = useDebounce(searchTerm, 300);

  const {
    feeds,
    isLoading,
    hasMore,
    isLoadingMore,
    loadMore,
    addFeed,
    updateFeed,
    deleteFeed,
  } = useFeeds(debouncedSearchTerm);

  const handleAddOrUpdateFeed = async (feedData: NewFeed | Feed) => {
    try {
      if ('id' in feedData) {
        await updateFeed.mutateAsync(feedData);
      } else {
        await addFeed.mutateAsync(feedData);
      }
      resetFormState();
    } catch (err) {
      logger.error('Error saving feed', { error: String(err) });
    }
  };

  const confirmDelete = (id: number) => {
    setConfirmDeleteId(id);
  };

  const executeDelete = async () => {
    if (confirmDeleteId === null) return;
    // Guard against double-fire: the synchronous setState below
    // closes the dialog, but the React re-render is async, so a
    // second click on the same tick can still reach this handler
    // with the stale `confirmDeleteId`. The disabled prop on the
    // confirm button (`deleteFeed.isPending`) is the primary
    // defense — this branch is the belt-and-suspenders backstop.
    // SECURITY.md L9.
    if (deleteFeed.isPending) return;
    const id = confirmDeleteId;
    setConfirmDeleteId(null);
    try {
      await deleteFeed.mutateAsync(id);
    } catch (err) {
      logger.error('Error deleting feed', { error: String(err) });
    }
  };

  // Per-view SEO metadata. Driven by `debouncedSearchTerm` (not the raw
  // input) so the title doesn't churn on every keystroke. Priority order:
  // edit > add > auth > search > default — a modal that opens over a search
  // should still announce itself in the title.
  const trimmedSearch = debouncedSearchTerm.trim();
  const pageTitle = editingFeed
    ? `Edit "${editingFeed.title}" — Nyx`
    : showAddForm && isAuthenticated
    ? 'Add a feed — Nyx'
    : showAuthForm && !isAuthenticated
    ? 'Sign in — Nyx'
    : chatOpen && isAuthenticated
    ? 'Chat — Nyx'
    : trimmedSearch
    ? `"${trimmedSearch}" — Search — Nyx`
    : DEFAULT_TITLE;
  const pageDescription = editingFeed
    ? `Edit the details of "${editingFeed.title}" in your Nyx feed.`
    : showAddForm && isAuthenticated
    ? 'Add a new entry to your Nyx feed.'
    : showAuthForm && !isAuthenticated
    ? 'Sign in or create a Nyx account to curate your feed.'
    : chatOpen && isAuthenticated
    ? 'Stream a chat with the Nyx assistant.'
    : trimmedSearch
    ? `Search Nyx for "${trimmedSearch}".`
    : DEFAULT_DESCRIPTION;

  return (
    <div className="app-root">
      <PageMeta title={pageTitle} description={pageDescription} />
      <ToastContainer />
      <nav className="sticky top-0 z-50 w-full border-b bg-black">
        <div className="w-full px-6 h-16 grid grid-cols-[1fr_auto_1fr] items-center gap-4">
          <div className="text-2xl font-bold bg-gradient-to-r from-primary to-accent bg-clip-text text-transparent justify-self-start">
            Nyx
          </div>
          <div className="relative w-full max-w-3xl">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
            <Input
              type="text"
              placeholder="Search for feeds..."
              value={searchTerm}
              maxLength={200}
              onChange={(e) => setSearchTerm(e.target.value)}
              className="pl-10 h-10"
            />
          </div>
          <div className="flex items-center gap-4 justify-self-end">
            {isAuthenticated ? (
              <>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setChatOpen(true)}
                  aria-label="Open chat"
                  className="gap-2"
                >
                  <MessageSquare className="h-4 w-4" />
                  Chat
                </Button>
                <div className="hidden md:flex items-center gap-2 text-sm font-medium text-muted-foreground">
                  <UserIcon className="h-4 w-4" />
                  <span>{user?.username}</span>
                </div>
                <Button variant="outline" size="sm" onClick={() => logout()} className="gap-2">
                  <LogOut className="h-4 w-4" />
                  Logout
                </Button>
              </>
            ) : (
              <Button size="sm" onClick={() => setShowAuthForm(true)}>Login / Register</Button>
            )}
          </div>
        </div>
      </nav>

      <section id="center" className="container mx-auto px-4 py-8">
        <div className="w-full max-w-2xl mx-auto flex flex-col items-center">
          {isAuthenticated && showAddForm && (
            <FeedForm
              title="New Feed"
              onSubmit={handleAddOrUpdateFeed}
              onCancel={resetFormState}
            />
          )}

          {isAuthenticated && editingFeed && (
            <FeedForm
              title="Edit Feed"
              feed={editingFeed}
              onSubmit={handleAddOrUpdateFeed}
              onCancel={resetFormState}
            />
          )}

          <FeedList
            feeds={feeds}
            loading={isLoading}
            searchTerm={searchTerm}
            hasMore={hasMore}
            isLoadingMore={isLoadingMore}
            onLoadMore={loadMore}
            onEdit={(feed) => {
              if (!isAuthenticated) {
                useUiStore.getState().addToast('Please login to edit feeds', 'info');
                setShowAuthForm(true);
                return;
              }
              setEditingFeed(feed);
              window.scrollTo({ top: 0, behavior: 'smooth' });
            }}
            onDelete={(id) => {
              if (!isAuthenticated) {
                useUiStore.getState().addToast('Please login to delete feeds', 'info');
                setShowAuthForm(true);
                return;
              }
              confirmDelete(id);
            }}
          />

          {isAuthenticated && (
            <Button
              size="icon"
              onClick={() => setShowAddForm(!showAddForm)}
              aria-label={showAddForm ? 'Cancel adding feed' : 'Add feed'}
              className="self-end mt-4 h-14 w-14 rounded-full shadow-lg"
            >
              {showAddForm ? <X className="h-6 w-6" /> : <Plus className="h-6 w-6" />}
            </Button>
          )}
        </div>
      </section>

      <Dialog open={showAuthForm && !isAuthenticated} onOpenChange={setShowAuthForm}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Login or Register</DialogTitle>
            <DialogDescription>
              Sign in to your account, or create a new one to start curating.
            </DialogDescription>
          </DialogHeader>
          <AuthForm
            onSuccess={() => setShowAuthForm(false)}
            onCancel={() => setShowAuthForm(false)}
          />
        </DialogContent>
      </Dialog>

      <Dialog
        open={confirmDeleteId !== null}
        onOpenChange={(open) => {
          if (!open) setConfirmDeleteId(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete feed</DialogTitle>
            <DialogDescription>
              This will remove the feed from your list. This action cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDeleteId(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={executeDelete}
              disabled={deleteFeed.isPending}
              data-testid="confirm-delete"
            >
              {deleteFeed.isPending ? 'Deleting…' : 'Delete'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <ChatPanel open={chatOpen} onOpenChange={setChatOpen} />

      <section id="spacer"></section>
    </div>
  );
}

export default App;