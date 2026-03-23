import React from 'react';
import reactLogo from './assets/react.svg';
import viteLogo from './assets/vite.svg';
import heroImg from './assets/hero.png';
import './App.css';
import { Movie, NewMovie } from './features/movies/types/movie';
import { useMovies } from './features/movies/hooks/useMovies';
import { MovieList } from './features/movies/components/MovieList';
import { MovieForm } from './features/movies/components/MovieForm';
import { useMovieUiStore } from './features/movies/store/movieUiStore';

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
      <section id="center">
        <div className="hero">
          <img src={heroImg} className="base" width="170" height="179" alt="" />
          <img src={reactLogo} className="framework" alt="React logo" />
          <img src={viteLogo} className="vite" alt="Vite logo" />
        </div>
        <div className="text-center">
          <h1 className="text-4xl md:text-6xl font-bold tracking-tight my-4 md:my-8 text-[var(--text-h)]">Nyx</h1>
          <p className="text-lg text-[var(--text)]">Your minimalist movie guide</p>
        </div>

        <div className="controls">
          <div className="search-container">
            <input
              type="text"
              placeholder="Search for movies..."
              value={searchTerm}
              onChange={(e) => setSearchTerm(e.target.value)}
              className="search-input"
            />
          </div>
          <button 
            className="add-button"
            onClick={() => setShowAddForm(!showAddForm)}
          >
            {showAddForm ? 'Cancel' : '+ Add Movie'}
          </button>
        </div>

        {showAddForm && (
          <MovieForm 
            title="New Movie"
            onSubmit={handleAddOrUpdateMovie}
            onCancel={resetFormState}
          />
        )}

        {editingMovie && (
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
            setEditingMovie(movie);
            window.scrollTo({ top: 0, behavior: 'smooth' });
          }}
          onDelete={handleDeleteMovie}
        />
      </section>

      <div className="ticks"></div>

      <section id="next-steps">
        <div id="docs">
          <svg className="icon" role="presentation" aria-hidden="true">
            <use href="/icons.svg#documentation-icon"></use>
          </svg>
          <h2>Documentation</h2>
          <p>Your questions, answered</p>
          <ul>
            <li>
              <a href="https://vite.dev/" target="_blank" rel="noreferrer">
                <img className="logo" src={viteLogo} alt="" />
                Explore Vite
              </a>
            </li>
            <li>
              <a href="https://react.dev/" target="_blank" rel="noreferrer">
                <img className="button-icon" src={reactLogo} alt="" />
                Learn more
              </a>
            </li>
          </ul>
        </div>
        <div id="social">
          <svg className="icon" role="presentation" aria-hidden="true">
            <use href="/icons.svg#social-icon"></use>
          </svg>
          <h2>Connect with us</h2>
          <p>Join the Vite community</p>
          <ul>
            <li>
              <a href="https://github.com/vitejs/vite" target="_blank" rel="noreferrer">
                <svg
                  className="button-icon"
                  role="presentation"
                  aria-hidden="true"
                >
                  <use href="/icons.svg#github-icon"></use>
                </svg>
                GitHub
              </a>
            </li>
            <li>
              <a href="https://chat.vite.dev/" target="_blank" rel="noreferrer">
                <svg
                  className="button-icon"
                  role="presentation"
                  aria-hidden="true"
                >
                  <use href="/icons.svg#discord-icon"></use>
                </svg>
                Discord
              </a>
            </li>
            <li>
              <a href="https://x.com/vite_js" target="_blank" rel="noreferrer">
                <svg
                  className="button-icon"
                  role="presentation"
                  aria-hidden="true"
                >
                  <use href="/icons.svg#x-icon"></use>
                </svg>
                X.com
              </a>
            </li>
            <li>
              <a href="https://bsky.app/profile/vite.dev" target="_blank" rel="noreferrer">
                <svg
                  className="button-icon"
                  role="presentation"
                  aria-hidden="true"
                >
                  <use href="/icons.svg#bluesky-icon"></use>
                </svg>
                Bluesky
              </a>
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
