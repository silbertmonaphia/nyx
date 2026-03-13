import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import App from './App'

// Mock fetch
global.fetch = vi.fn()

describe('App component', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    fetch.mockReset()
  })

  it('renders Douban Lite heading', async () => {
    fetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    render(<App />)
    expect(screen.getByText(/Douban Lite/i)).toBeInTheDocument()
    
    // Check if loading state is shown initially
    expect(screen.getByText(/Loading movies.../i)).toBeInTheDocument()

    // Wait for movies to be loaded
    await waitFor(() => {
      expect(screen.getByText('Inception')).toBeInTheDocument()
    })
    expect(screen.getByText('★ 8.8')).toBeInTheDocument()
  })

  it('filters movies based on search input', async () => {
    // Initial fetch
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 },
        { id: 2, title: 'The Matrix', description: 'Simulation', rating: 8.7 }
      ])
    })

    render(<App />)

    await waitFor(() => {
      expect(screen.getByText('Inception')).toBeInTheDocument()
      expect(screen.getByText('The Matrix')).toBeInTheDocument()
    })

    // Mock search fetch
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    const searchInput = screen.getByPlaceholderText(/Search for movies.../i)
    fireEvent.change(searchInput, { target: { value: 'Incep' } })

    // Wait for debounced search
    await waitFor(() => {
      // Check if any call matches the search URL
      const hasSearchCall = fetch.mock.calls.some(call => call[0].includes('q=Incep'))
      expect(hasSearchCall).toBe(true)
    }, { timeout: 1500 })

    await waitFor(() => {
      expect(screen.getByText('Inception')).toBeInTheDocument()
      expect(screen.queryByText('The Matrix')).not.toBeInTheDocument()
    })
  })

  it('shows no movies found message', async () => {
    fetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve([])
    })

    render(<App />)

    const searchInput = screen.getByPlaceholderText(/Search for movies.../i)
    fireEvent.change(searchInput, { target: { value: 'NonExistent' } })

    await waitFor(() => {
      expect(screen.getByText(/No movies found matching "NonExistent"/i)).toBeInTheDocument()
    }, { timeout: 1500 })
  })

  it('adds a new movie through the form', async () => {
    // 1. Initial GET on mount
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([])
    })

    render(<App />)

    // Open form
    const addButton = screen.getByText(/\+ Add Movie/i)
    fireEvent.click(addButton)

    // Fill form
    fireEvent.change(screen.getByPlaceholderText('Title'), { target: { value: 'Interstellar' } })
    fireEvent.change(screen.getByPlaceholderText('Description'), { target: { value: 'Space odyssey' } })
    
    // 2. POST response
    fetch.mockResolvedValueOnce({ 
      ok: true,
      json: () => Promise.resolve({ id: 1, title: 'Interstellar', description: 'Space odyssey', rating: 5.0 })
    })

    // 3. GET refresh response
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Interstellar', description: 'Space odyssey', rating: 5.0 }
      ])
    })

    fireEvent.click(screen.getByText('Save Movie'))

    // Verify POST was called
    await waitFor(() => {
      const postCall = fetch.mock.calls.find(call => call[1]?.method === 'POST')
      expect(postCall).toBeDefined()
      expect(postCall[0]).toBe('http://localhost:8080/api/movies')
      expect(postCall[1].body).toContain('Interstellar')
    })

    // Verify list updated
    await waitFor(() => {
      expect(screen.getByText('Interstellar')).toBeInTheDocument()
    })
  })
})
