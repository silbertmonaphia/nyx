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

  it('renders Nyx heading', async () => {
    fetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    render(<App />)
    expect(screen.getByText(/Nyx/i)).toBeInTheDocument()
    
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

  it('edits an existing movie', async () => {
    // Initial fetch
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    render(<App />)

    await waitFor(() => {
      expect(screen.getByText('Inception')).toBeInTheDocument()
    })

    // Click edit button
    const editButton = screen.getByTitle('Edit')
    fireEvent.click(editButton)

    // Check if edit form is shown
    expect(screen.getByText('Edit Movie')).toBeInTheDocument()
    const titleInput = screen.getByDisplayValue('Inception')
    fireEvent.change(titleInput, { target: { value: 'Inception Updated' } })

    // Mock PUT response
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({ id: 1, title: 'Inception Updated', description: 'Dream sharing', rating: 8.8 })
    })

    // Mock refresh response
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception Updated', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    const updateButton = screen.getByText('Update Movie')
    fireEvent.click(updateButton)

    // Verify PUT was called
    await waitFor(() => {
      const putCall = fetch.mock.calls.find(call => call[1]?.method === 'PUT')
      expect(putCall).toBeDefined()
      expect(putCall[0]).toBe('http://localhost:8080/api/movies/1')
      expect(JSON.parse(putCall[1].body).title).toBe('Inception Updated')
    })

    // Verify list updated
    await waitFor(() => {
      expect(screen.getByText('Inception Updated')).toBeInTheDocument()
    })
  })

  it('deletes a movie after confirmation', async () => {
    // Initial fetch
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    // Mock window.confirm
    const confirmSpy = vi.spyOn(window, 'confirm').mockImplementation(() => true)

    render(<App />)

    await waitFor(() => {
      expect(screen.getByText('Inception')).toBeInTheDocument()
    })

    // Mock DELETE response
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({})
    })

    // Mock refresh response
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([])
    })

    // Click delete button
    const deleteButton = screen.getByTitle('Delete')
    fireEvent.click(deleteButton)

    expect(confirmSpy).toHaveBeenCalled()

    // Verify DELETE was called
    await waitFor(() => {
      const deleteCall = fetch.mock.calls.find(call => call[1]?.method === 'DELETE')
      expect(deleteCall).toBeDefined()
      expect(deleteCall[0]).toBe('http://localhost:8080/api/movies/1')
    })

    // Verify item is gone
    await waitFor(() => {
      expect(screen.queryByText('Inception')).not.toBeInTheDocument()
    })

    confirmSpy.mockRestore()
  })
})
