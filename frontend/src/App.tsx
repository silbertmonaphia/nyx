import React, { useState } from 'react';
import reactLogo from './assets/react.svg';
import viteLogo from './assets/vite.svg';
import heroImg from './assets/hero.png';
import './App.css';
import { Movie, NewMovie } from './features/movies/types/movie';
import { useMovies } from './features/movies/hooks/useMovies';
import { MovieList } from './features/movies/components/MovieList';
import { MovieForm } from './features/movies/components/MovieForm';
import { useMovieUiStore } from './features/movies/store/movieUiStore';
import { ToastContainer } from './components/ui/ToastContainer';
import { useUiStore } from './store/uiStore';
import { useAuthStore } from './store/authStore';
import { AuthForm } from './features/auth/components/AuthForm';
import { Button } from './components/ui/Button';
import { Input } from './components/ui/Input';
import { Plus, X, Search, LogOut, User as UserIcon } from 'lucide-react';

function App() {
  const { 
    searchTerm, 
    setSearchTerm, 
    showAddForm, 
    setShowAddForm, 
    editingMovie, 
    setEditingMovie,
    resetFormState
  } = useMovieUiStore();

  const { isAuthenticated, user, logout } = useAuthStore();
  const [showAuthForm, setShowAuthForm] = useState(false);

  const { getMovies, addMovie, updateMovie, deleteMovie } = useMovies();
  const { data: movies = [], isLoading } = getMovies(searchTerm);

  const handleAddOrUpdateMovie = async (movieData: NewMovie | Movie) => {
    try {
      if ('id' in movieData) {
        await updateMovie.mutateAsync(movieData);
      } else {
        await addMovie.mutateAsync(movieData);
      }
      resetFormState();
    } catch (err) {
      console.error('Error saving movie:', err);
    }
  };

  const handleDeleteMovie = async (id: number) => {
    if (!window.confirm('Are you sure you want to delete this movie?')) return;
    try {
      await deleteMovie.mutateAsync(id);
    } catch (err) {
      console.error('Error deleting movie:', err);
    }
  };

  return (
    <>
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
        <div className="hero mb-8">
          <img src={heroImg} className="base mx-auto" width="170" height="179" alt="" />
          <img src={reactLogo} className="framework" alt="React logo" />
          <img src={viteLogo} className="vite" alt="Vite logo" />
        </div>
        
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
              handleDeleteMovie(id);
            }}
          />
        </div>
      </section>

      <div className="ticks"></div>

      <section id="next-steps" className="container mx-auto px-4 py-12 border-t grid grid-cols-1 md:grid-cols-2 gap-12 text-left">
        <div id="docs" className="space-y-6">
          <div className="flex items-center gap-3">
            <div className="p-2 rounded-lg bg-primary/10 text-primary">
              <svg className="h-6 w-6" role="presentation" aria-hidden="true">
                <use href="/icons.svg#documentation-icon"></use>
              </svg>
            </div>
            <h2 className="text-2xl font-bold">Documentation</h2>
          </div>
          <p className="text-muted-foreground">Your questions, answered</p>
          <ul className="flex flex-wrap gap-3">
            <li>
              <Button variant="secondary" asChild>
                <a href="https://vite.dev/" target="_blank" rel="noreferrer">
                  <img className="h-4 w-4 mr-2" src={viteLogo} alt="" />
                  Explore Vite
                </a>
              </Button>
            </li>
            <li>
              <Button variant="secondary" asChild>
                <a href="https://react.dev/" target="_blank" rel="noreferrer">
                  <img className="h-4 w-4 mr-2" src={reactLogo} alt="" />
                  Learn more
                </a>
              </Button>
            </li>
          </ul>
        </div>
        <div id="social" className="space-y-6">
          <div className="flex items-center gap-3">
            <div className="p-2 rounded-lg bg-accent/10 text-accent">
              <svg className="h-6 w-6" role="presentation" aria-hidden="true">
                <use href="/icons.svg#social-icon"></use>
              </svg>
            </div>
            <h2 className="text-2xl font-bold">Connect with us</h2>
          </div>
          <p className="text-muted-foreground">Join the Vite community</p>
          <ul className="flex flex-wrap gap-3">
            <li>
              <Button variant="outline" size="sm" asChild>
                <a href="https://github.com/vitejs/vite" target="_blank" rel="noreferrer">
                  <svg className="h-4 w-4 mr-2" role="presentation" aria-hidden="true">
                    <use href="/icons.svg#github-icon"></use>
                  </svg>
                  GitHub
                </a>
              </Button>
            </li>
            <li>
              <Button variant="outline" size="sm" asChild>
                <a href="https://chat.vite.dev/" target="_blank" rel="noreferrer">
                  <svg className="h-4 w-4 mr-2" role="presentation" aria-hidden="true">
                    <use href="/icons.svg#discord-icon"></use>
                  </svg>
                  Discord
                </a>
              </Button>
            </li>
            <li>
              <Button variant="outline" size="sm" asChild>
                <a href="https://x.com/vite_js" target="_blank" rel="noreferrer">
                  <svg className="h-4 w-4 mr-2" role="presentation" aria-hidden="true">
                    <use href="/icons.svg#x-icon"></use>
                  </svg>
                  X.com
                </a>
              </Button>
            </li>
          </ul>
        </div>
      </section>

      <div className="ticks"></div>
      <section id="spacer"></section>
    </>
  );
}

export default App;
