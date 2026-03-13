import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import App from './App'

// Mock fetch
global.fetch = vi.fn()

describe('App component', () => {
  beforeEach(() => {
    fetch.mockClear()
  })

  it('renders Douban Lite heading', async () => {
    fetch.mockResolvedValue({
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
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    const searchInput = screen.getByPlaceholderText(/Search for movies.../i)
    fireEvent.change(searchInput, { target: { value: 'Incep' } })

    // Wait for debounced search
    await waitFor(() => {
      expect(fetch).toHaveBeenCalledWith(expect.stringContaining('q=Incep'))
    }, { timeout: 1000 })

    await waitFor(() => {
      expect(screen.getByText('Inception')).toBeInTheDocument()
      expect(screen.queryByText('The Matrix')).not.toBeInTheDocument()
    })
  })

  it('shows no movies found message', async () => {
    fetch.mockResolvedValue({
      json: () => Promise.resolve([])
    })

    render(<App />)

    const searchInput = screen.getByPlaceholderText(/Search for movies.../i)
    fireEvent.change(searchInput, { target: { value: 'NonExistent' } })

    await waitFor(() => {
      expect(screen.getByText(/No movies found matching "NonExistent"/i)).toBeInTheDocument()
    }, { timeout: 1000 })
  })
})
