import React, { useState } from 'react';
import './App.css';
import { Movie, NewMovie } from './features/movies/types/movie';
import { useMovies } from './features/movies/hooks/useMovies';
import { MovieList } from './features/movies/components/MovieList';
import { MovieForm } from './features/movies/components/MovieForm';
import { useMovieUiStore } from './features/movies/store/movieUiStore';
import { useDebounce } from './hooks/useDebounce';
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
import { Plus, X, Search, LogOut, User as UserIcon } from 'lucide-react';
import { PageMeta } from './components/app/PageMeta';
import { logger } from './services/logger';

const DEFAULT_TITLE = 'Nyx — Your minimalist movie guide';
const DEFAULT_DESCRIPTION =
  'A minimalist movie guide for discovering, tracking, and curating the films you love.';

function App() {
  const {
    searchTerm,
    setSearchTerm,
    showAddForm,
    setShowAddForm,
    editingMovie,
    setEditingMovie,
    resetFormState,
  } = useMovieUiStore();

  const { isAuthenticated, user, logout } = useAuthStore();
  const [showAuthForm, setShowAuthForm] = useState(false);
  const [confirmDeleteId, setConfirmDeleteId] = useState<number | null>(null);

  // The input stays fully controlled by `searchTerm` (instant typing),
  // but the network query only fires once the user has paused for
  // 300ms. Driving `useMovies` off the debounced value keeps the
  // query key in sync with what we actually fetched, so the empty-state
  // qualifier and the loaded list agree.
  const debouncedSearchTerm = useDebounce(searchTerm, 300);

  const {
    movies,
    isLoading,
    hasMore,
    isLoadingMore,
    loadMore,
    addMovie,
    updateMovie,
    deleteMovie,
  } = useMovies(debouncedSearchTerm);

  const handleAddOrUpdateMovie = async (movieData: NewMovie | Movie) => {
    try {
      if ('id' in movieData) {
        await updateMovie.mutateAsync(movieData);
      } else {
        await addMovie.mutateAsync(movieData);
      }
      resetFormState();
    } catch (err) {
      logger.error('Error saving movie', { error: String(err) });
    }
  };

  const confirmDelete = (id: number) => {
    setConfirmDeleteId(id);
  };

  const executeDelete = async () => {
    if (confirmDeleteId === null) return;
    const id = confirmDeleteId;
    setConfirmDeleteId(null);
    try {
      await deleteMovie.mutateAsync(id);
    } catch (err) {
      logger.error('Error deleting movie', { error: String(err) });
    }
  };

  // Per-view SEO metadata. Driven by `debouncedSearchTerm` (not the raw
  // input) so the title doesn't churn on every keystroke. Priority order:
  // edit > add > auth > search > default — a modal that opens over a search
  // should still announce itself in the title.
  const trimmedSearch = debouncedSearchTerm.trim();
  const pageTitle = editingMovie
    ? `Edit "${editingMovie.title}" — Nyx`
    : showAddForm && isAuthenticated
    ? 'Add a movie — Nyx'
    : showAuthForm && !isAuthenticated
    ? 'Sign in — Nyx'
    : trimmedSearch
    ? `"${trimmedSearch}" — Search — Nyx`
    : DEFAULT_TITLE;
  const pageDescription = editingMovie
    ? `Edit the details of "${editingMovie.title}" in your Nyx movie collection.`
    : showAddForm && isAuthenticated
    ? 'Add a new movie to your Nyx collection.'
    : showAuthForm && !isAuthenticated
    ? 'Sign in or create a Nyx account to curate your movie list.'
    : trimmedSearch
    ? `Search Nyx for "${trimmedSearch}".`
    : DEFAULT_DESCRIPTION;

  return (
    <div className="app-root">
      <PageMeta title={pageTitle} description={pageDescription} />
      <ToastContainer />
      <nav className="sticky top-0 z-50 w-full border-b bg-background/95 backdrop-blur supports-[backdrop-filter]:bg-background/60">
        <div className="container mx-auto px-4 h-16 flex items-center justify-between">
          <div className="text-2xl font-bold bg-gradient-to-r from-primary to-accent bg-clip-text text-transparent">Nyx</div>
          <div className="flex items-center gap-4">
            {isAuthenticated ? (
              <>
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
        <div className="max-w-2xl mx-auto text-center mb-12">
          <h1 className="text-5xl md:text-7xl font-extrabold tracking-tight mb-4">Nyx</h1>
          <p className="text-xl text-muted-foreground">Your minimalist movie guide</p>
        </div>

        <div className="w-full max-w-2xl mx-auto flex flex-col md:flex-row gap-4 items-center mb-8">
          <div className="relative w-full">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
            <Input
              type="text"
              placeholder="Search for movies..."
              value={searchTerm}
              onChange={(e) => setSearchTerm(e.target.value)}
              className="pl-10 h-12 text-lg rounded-xl"
            />
          </div>
          {isAuthenticated && (
            <Button 
              size="lg"
              variant={showAddForm ? "outline" : "default"}
              onClick={() => setShowAddForm(!showAddForm)}
              className="w-full md:w-auto h-12 gap-2 rounded-xl"
            >
              {showAddForm ? (
                <><X className="h-5 w-5" /> Cancel</>
              ) : (
                <><Plus className="h-5 w-5" /> Add Movie</>
              )}
            </Button>
          )}
        </div>

        <div className="w-full max-w-2xl mx-auto flex flex-col items-center">
          {showAuthForm && !isAuthenticated && (
            <AuthForm 
              onSuccess={() => setShowAuthForm(false)}
              onCancel={() => setShowAuthForm(false)}
            />
          )}

          {isAuthenticated && showAddForm && (
            <MovieForm 
              title="New Movie"
              onSubmit={handleAddOrUpdateMovie}
              onCancel={resetFormState}
            />
          )}

          {isAuthenticated && editingMovie && (
            <MovieForm 
              title="Edit Movie"
              movie={editingMovie}
              onSubmit={handleAddOrUpdateMovie}
              onCancel={resetFormState}
            />
          )}

          <MovieList
            movies={movies}
            loading={isLoading}
            searchTerm={searchTerm}
            hasMore={hasMore}
            isLoadingMore={isLoadingMore}
            onLoadMore={loadMore}
            onEdit={(movie) => {
              if (!isAuthenticated) {
                useUiStore.getState().addToast('Please login to edit movies', 'info');
                setShowAuthForm(true);
                return;
              }
              setEditingMovie(movie);
              window.scrollTo({ top: 0, behavior: 'smooth' });
            }}
            onDelete={(id) => {
              if (!isAuthenticated) {
                useUiStore.getState().addToast('Please login to delete movies', 'info');
                setShowAuthForm(true);
                return;
              }
              confirmDelete(id);
            }}
          />
        </div>
      </section>

      <Dialog
        open={confirmDeleteId !== null}
        onOpenChange={(open) => {
          if (!open) setConfirmDeleteId(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete movie</DialogTitle>
            <DialogDescription>
              This will remove the movie from your list. This action cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDeleteId(null)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={executeDelete} data-testid="confirm-delete">
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <section id="spacer"></section>
    </div>
  );
}

export default App;
