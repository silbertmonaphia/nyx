import { useState, useEffect } from 'react'
import reactLogo from './assets/react.svg'
import viteLogo from './assets/vite.svg'
import heroImg from './assets/hero.png'
import './App.css'

function App() {
  const [movies, setMovies] = useState([])
  const [loading, setLoading] = useState(true)
  const [searchTerm, setSearchTerm] = useState('')
  const [showAddForm, setShowAddForm] = useState(false)
  const [newMovie, setNewMovie] = useState({ title: '', description: '', rating: 5.0 })
  const [editingMovie, setEditingMovie] = useState(null)

  const fetchMovies = async () => {
    setLoading(true)
    try {
      const response = await fetch(`http://localhost:8080/api/movies${searchTerm ? `?q=${searchTerm}` : ''}`)
      const data = await response.json()
      setMovies(data || [])
    } catch (err) {
      console.error('Error fetching movies:', err)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    const timer = setTimeout(() => {
      fetchMovies()
    }, 300)
    return () => clearTimeout(timer)
  }, [searchTerm])

  const handleAddMovie = async (e) => {
    e.preventDefault()
    try {
      const response = await fetch('http://localhost:8080/api/movies', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ...newMovie,
          rating: parseFloat(newMovie.rating)
        })
      })
      if (response.ok) {
        setNewMovie({ title: '', description: '', rating: 5.0 })
        setShowAddForm(false)
        fetchMovies()
      }
    } catch (err) {
      console.error('Error adding movie:', err)
    }
  }

  const handleUpdateMovie = async (e) => {
    e.preventDefault()
    try {
      const response = await fetch(`http://localhost:8080/api/movies/${editingMovie.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ...editingMovie,
          rating: parseFloat(editingMovie.rating)
        })
      })
      if (response.ok) {
        setEditingMovie(null)
        fetchMovies()
      }
    } catch (err) {
      console.error('Error updating movie:', err)
    }
  }

  const handleDeleteMovie = async (id) => {
    if (!window.confirm('Are you sure you want to delete this movie?')) return
    try {
      const response = await fetch(`http://localhost:8080/api/movies/${id}`, {
        method: 'DELETE'
      })
      if (response.ok) {
        fetchMovies()
      }
    } catch (err) {
      console.error('Error deleting movie:', err)
    }
  }

  return (
    <>
      <section id="center">
        <div className="hero">
          <img src={heroImg} className="base" width="170" height="179" alt="" />
          <img src={reactLogo} className="framework" alt="React logo" />
          <img src={viteLogo} className="vite" alt="Vite logo" />
        </div>
        <div>
          <h1>Nyx</h1>
          <p>Your minimalist movie guide</p>
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
            onClick={() => {
              setShowAddForm(!showAddForm)
              setEditingMovie(null)
            }}
          >
            {showAddForm ? 'Cancel' : '+ Add Movie'}
          </button>
        </div>

        {showAddForm && (
          <form className="add-movie-form" onSubmit={handleAddMovie}>
            <input
              type="text"
              placeholder="Title"
              value={newMovie.title}
              onChange={(e) => setNewMovie({...newMovie, title: e.target.value})}
              required
            />
            <textarea
              placeholder="Description"
              value={newMovie.description}
              onChange={(e) => setNewMovie({...newMovie, description: e.target.value})}
            />
            <div className="rating-input">
              <label>Rating: {newMovie.rating}</label>
              <input
                type="range"
                min="0"
                max="10"
                step="0.1"
                value={newMovie.rating}
                onChange={(e) => setNewMovie({...newMovie, rating: e.target.value})}
              />
            </div>
            <button type="submit" className="submit-button">Save Movie</button>
          </form>
        )}

        {editingMovie && (
          <form className="add-movie-form edit-form" onSubmit={handleUpdateMovie}>
            <h3>Edit Movie</h3>
            <input
              type="text"
              placeholder="Title"
              value={editingMovie.title}
              onChange={(e) => setEditingMovie({...editingMovie, title: e.target.value})}
              required
            />
            <textarea
              placeholder="Description"
              value={editingMovie.description}
              onChange={(e) => setEditingMovie({...editingMovie, description: e.target.value})}
            />
            <div className="rating-input">
              <label>Rating: {editingMovie.rating}</label>
              <input
                type="range"
                min="0"
                max="10"
                step="0.1"
                value={editingMovie.rating}
                onChange={(e) => setEditingMovie({...editingMovie, rating: e.target.value})}
              />
            </div>
            <div className="form-actions">
              <button type="submit" className="submit-button">Update Movie</button>
              <button type="button" className="cancel-button" onClick={() => setEditingMovie(null)}>Cancel</button>
            </div>
          </form>
        )}

        <div className="movie-list">
          {loading ? (
            <p>Loading movies...</p>
          ) : movies.length > 0 ? (
            movies.map((movie) => (
              <div key={movie.id} className="movie-item">
                <div className="movie-content">
                  <h3>{movie.title}</h3>
                  <p>{movie.description}</p>
                  <span className="rating">★ {movie.rating}</span>
                </div>
                <div className="movie-actions">
                  <button 
                    className="edit-icon-button"
                    onClick={() => {
                      setEditingMovie(movie)
                      setShowAddForm(false)
                      window.scrollTo({ top: 0, behavior: 'smooth' })
                    }}
                    title="Edit"
                  >
                    ✎
                  </button>
                  <button 
                    className="delete-icon-button"
                    onClick={() => handleDeleteMovie(movie.id)}
                    title="Delete"
                  >
                    ×
                  </button>
                </div>
              </div>
            ))
          ) : (
            <p>No movies found {searchTerm && `matching "${searchTerm}"`}</p>
          )}
        </div>
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
              <a href="https://vite.dev/" target="_blank">
                <img className="logo" src={viteLogo} alt="" />
                Explore Vite
              </a>
            </li>
            <li>
              <a href="https://react.dev/" target="_blank">
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
              <a href="https://github.com/vitejs/vite" target="_blank">
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
              <a href="https://chat.vite.dev/" target="_blank">
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
              <a href="https://x.com/vite_js" target="_blank">
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
              <a href="https://bsky.app/profile/vite.dev" target="_blank">
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
  )
}

export default App
