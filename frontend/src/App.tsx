import React, { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
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
import { Plus, X, Search, LogOut, User as UserIcon, MessageSquare, ArrowDownNarrowWide, ArrowUpNarrowWide } from 'lucide-react';
import { PageMeta } from './components/app/PageMeta';
import { ThemeToggle } from './components/app/ThemeToggle';
import { LocaleToggle } from './components/app/LocaleToggle';
import { logger } from './services/logger';
import { ChatPanel } from './features/chat/components/ChatPanel';
import { resetFeedStateForNewUser } from './features/feeds/services/resetFeedState';

function App() {
  const { t } = useTranslation();
  const {
    searchTerm,
    setSearchTerm,
    showAddForm,
    setShowAddForm,
    editingFeed,
    setEditingFeed,
    sortOrder,
    toggleSortOrder,
    resetFormState,
  } = useFeedUiStore();

  const { isAuthenticated, user, logout } = useAuthStore();
  const [showAuthForm, setShowAuthForm] = useState(false);
  const [confirmDeleteId, setConfirmDeleteId] = useState<number | null>(null);
  const [chatOpen, setChatOpen] = useState(false);

  // Boot-time round-trip: if localStorage has a persisted user,
  // reconcile it against the server. SECURITY.md L7.
  useAuthReconciliation();

  // Drop every cached feed page + UI state when the authenticated
  // user changes (login / logout / user swap). The query key inside
  // `useFeeds` already includes `userId`, so this is belt-and-suspenders:
  // it guarantees no in-flight stale snapshot serves to a freshly
  // authenticated viewer even if TanStack Query hasn't garbage-collected
  // the old key yet. Fires on logout (user becomes null) too — the
  // UI store reset clears any leftover search / edit modals from the
  // previous user.
  useEffect(() => {
    resetFeedStateForNewUser();
  }, [user?.id]);

  // The input stays fully controlled by `searchTerm` (instant typing),
  // but the network query only fires once the user has paused for
  // 300ms. Driving `useFeeds` off the debounced value keeps the
  // query key in sync with what we actually fetched, so the empty-state
  // qualifier and the loaded list agree.
  const debouncedSearchTerm = useDebounce(searchTerm, 300);

  const {
    feeds,
    totalCount,
    isLoading,
    hasMore,
    isLoadingMore,
    loadMore,
    addFeed,
    updateFeed,
    deleteFeed,
  } = useFeeds(debouncedSearchTerm, sortOrder);

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
    ? t('app.title.edit', { title: editingFeed.title })
    : showAddForm && isAuthenticated
    ? t('app.title.add')
    : showAuthForm && !isAuthenticated
    ? t('app.title.signIn')
    : chatOpen && isAuthenticated
    ? t('app.title.chat')
    : sortOrder === 'asc' && !trimmedSearch
    ? t('app.title.oldestFirst')
    : trimmedSearch
    ? t('app.title.search', { term: trimmedSearch })
    : t('app.title.default');
  const pageDescription = editingFeed
    ? t('app.description.edit', { title: editingFeed.title })
    : showAddForm && isAuthenticated
    ? t('app.description.add')
    : showAuthForm && !isAuthenticated
    ? t('app.description.signIn')
    : chatOpen && isAuthenticated
    ? t('app.description.chat')
    : trimmedSearch
    ? t('app.description.search', { term: trimmedSearch })
    : t('app.description.default');

  return (
    <div className="app-root">
      <PageMeta title={pageTitle} description={pageDescription} />
      <ToastContainer />
      <nav className="sticky top-0 z-50 w-full border-b bg-black">
        <div className="w-full px-6 h-16 grid grid-cols-[1fr_auto_1fr] items-center gap-4">
          <div className="flex items-center gap-3 justify-self-start">
            <div className="text-2xl font-bold bg-gradient-to-r from-primary to-accent bg-clip-text text-transparent">
              Nyx
            </div>
            <LocaleToggle />
            <ThemeToggle />
          </div>
          <div className="relative w-full max-w-3xl">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
            <Input
              type="text"
              placeholder={t('nav.searchPlaceholder')}
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
                  aria-label={t('nav.openChat')}
                  className="gap-2"
                >
                  <MessageSquare className="h-4 w-4" />
                  {t('nav.chat')}
                </Button>
                <div className="hidden md:flex items-center gap-2 text-sm font-medium text-muted-foreground">
                  <UserIcon className="h-4 w-4" />
                  <span>{user?.username}</span>
                </div>
                <Button variant="outline" size="sm" onClick={() => logout()} className="gap-2">
                  <LogOut className="h-4 w-4" />
                  {t('nav.logout')}
                </Button>
              </>
            ) : (
              <Button size="sm" onClick={() => setShowAuthForm(true)}>{t('nav.loginOrRegister')}</Button>
            )}
          </div>
        </div>
      </nav>

      <section id="center" className="container mx-auto px-4 pb-8 flex flex-1 min-h-0 flex-col place-content-start items-stretch">
        <div className="w-full max-w-2xl mx-auto flex flex-1 min-h-0 flex-col items-center">
          <FeedList
            feeds={feeds}
            totalCount={totalCount}
            loading={isLoading}
            searchTerm={searchTerm}
            hasMore={hasMore}
            isLoadingMore={isLoadingMore}
            onLoadMore={loadMore}
            editingFeed={editingFeed}
            currentUserId={user?.id}
            onUpdate={handleAddOrUpdateFeed}
            onCancelEdit={resetFormState}
            onEdit={(feed) => {
              if (!isAuthenticated) {
                useUiStore.getState().addToast(t('app.toast.loginToEdit'), 'info');
                setShowAuthForm(true);
                return;
              }
              // Defense-in-depth: the backend will 404 (not 403) on
              // a non-owner, but surface a friendlier message before
              // the request fires. The list is owner-scoped on the
              // server, so this branch only fires if a stale feed
              // somehow lingers (e.g. optimistic row before cache
              // eviction) — better UX than a silent failure.
              if (feed.user_id !== user?.id) {
                useUiStore.getState().addToast(t('app.toast.ownerOnly'), 'info');
                return;
              }
              setEditingFeed(feed);
            }}
            onDelete={(id) => {
              if (!isAuthenticated) {
                useUiStore.getState().addToast(t('app.toast.loginToDelete'), 'info');
                setShowAuthForm(true);
                return;
              }
              confirmDelete(id);
            }}
          />

          {isAuthenticated && showAddForm && (
            <FeedForm
              title={t('feeds.newFeed')}
              onSubmit={handleAddOrUpdateFeed}
              onCancel={resetFormState}
            />
          )}

          <div className="self-end mt-4 flex items-center gap-3">
            <Button
              size="icon"
              variant="outline"
              onClick={toggleSortOrder}
              aria-label={
                sortOrder === 'desc'
                  ? t('nav.sort.toggleToOldest')
                  : t('nav.sort.toggleToNewest')
              }
              title={sortOrder === 'desc' ? t('nav.sort.newest') : t('nav.sort.oldest')}
              className="h-14 w-14 rounded-full shadow-lg"
              data-testid="toggle-sort-order"
            >
              {sortOrder === 'desc' ? (
                <ArrowDownNarrowWide className="h-6 w-6" />
              ) : (
                <ArrowUpNarrowWide className="h-6 w-6" />
              )}
            </Button>
            {isAuthenticated && (
              <Button
                size="icon"
                onClick={() => setShowAddForm(!showAddForm)}
                aria-label={showAddForm ? t('nav.cancelAddFeed') : t('nav.addFeed')}
                className="h-14 w-14 rounded-full shadow-lg"
              >
                {showAddForm ? <X className="h-6 w-6" /> : <Plus className="h-6 w-6" />}
              </Button>
            )}
          </div>
        </div>
      </section>

      <Dialog open={showAuthForm && !isAuthenticated} onOpenChange={setShowAuthForm}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t('app.authDialog.title')}</DialogTitle>
            <DialogDescription>{t('app.authDialog.description')}</DialogDescription>
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
            <DialogTitle>{t('app.deleteDialog.title')}</DialogTitle>
            <DialogDescription>{t('app.deleteDialog.description')}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDeleteId(null)}>
              {t('common.cancel')}
            </Button>
            <Button
              variant="destructive"
              onClick={executeDelete}
              disabled={deleteFeed.isPending}
              data-testid="confirm-delete"
            >
              {deleteFeed.isPending ? t('app.deleteDialog.pending') : t('app.deleteDialog.confirm')}
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