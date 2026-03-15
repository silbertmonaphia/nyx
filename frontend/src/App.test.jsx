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

  it('cancels adding a movie', async () => {
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([])
    })

    render(<App />)

    const addButton = screen.getByText(/\+ Add Movie/i)
    fireEvent.click(addButton)
    expect(screen.getByText('New Movie')).toBeInTheDocument()

    const cancelButton = screen.getByText('Cancel')
    fireEvent.click(cancelButton)
    expect(screen.queryByText('New Movie')).not.toBeInTheDocument()
  })

  it('cancels editing a movie', async () => {
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    render(<App />)
    await waitFor(() => expect(screen.getByText('Inception')).toBeInTheDocument())

    const editButton = screen.getByTitle('Edit')
    fireEvent.click(editButton)
    expect(screen.getByText('Edit Movie')).toBeInTheDocument()

    const cancelButton = screen.getByText('Cancel')
    fireEvent.click(cancelButton)
    expect(screen.queryByText('Edit Movie')).not.toBeInTheDocument()
  })

  it('does not delete movie if confirmation is cancelled', async () => {
    fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve([
        { id: 1, title: 'Inception', description: 'Dream sharing', rating: 8.8 }
      ])
    })

    const confirmSpy = vi.spyOn(window, 'confirm').mockImplementation(() => false)

    render(<App />)
    await waitFor(() => expect(screen.getByText('Inception')).toBeInTheDocument())

    const deleteButton = screen.getByTitle('Delete')
    fireEvent.click(deleteButton)

    expect(confirmSpy).toHaveBeenCalled()
    expect(fetch.mock.calls.length).toBe(1) // Only initial fetch
    expect(screen.getByText('Inception')).toBeInTheDocument()

    confirmSpy.mockRestore()
  })

  it('handles API errors gracefully', async () => {
    // Mock console.error to avoid cluttering test output
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    
    fetch.mockResolvedValueOnce({
      ok: false,
      status: 500
    })

    render(<App />)

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalled()
    })

    consoleSpy.mockRestore()
  })

  it('handles errors when adding a movie', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    fetch.mockResolvedValueOnce({ ok: true, json: () => Promise.resolve([]) }) // Initial fetch
    
    render(<App />)
    fireEvent.click(screen.getByText(/\+ Add Movie/i))
    
    fetch.mockResolvedValueOnce({ ok: false }) // Failed POST
    fireEvent.click(screen.getByText('Save Movie'))

    await waitFor(() => {
      // console.error is called because response.ok is false is not handled by a catch block but we can still check if logic flows correctly
      // Actually App.jsx only console.errors on catch. Let's mock a rejection.
    })
    
    fetch.mockRejectedValueOnce(new Error('Network error'))
    fireEvent.click(screen.getByText('Save Movie'))
    await waitFor(() => expect(consoleSpy).toHaveBeenCalledWith('Error adding movie:', expect.any(Error)))
    
    consoleSpy.mockRestore()
  })

  it('handles errors when updating a movie', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    fetch.mockResolvedValueOnce({ ok: true, json: () => Promise.resolve([{ id: 1, title: 'M', rating: 5 }]) })
    
    render(<App />)
    await waitFor(() => expect(screen.getByText('M')).toBeInTheDocument())
    
    fireEvent.click(screen.getByTitle('Edit'))
    fetch.mockRejectedValueOnce(new Error('Update failed'))
    fireEvent.click(screen.getByText('Update Movie'))
    
    await waitFor(() => expect(consoleSpy).toHaveBeenCalledWith('Error updating movie:', expect.any(Error)))
    consoleSpy.mockRestore()
  })

  it('handles errors when deleting a movie', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    fetch.mockResolvedValueOnce({ ok: true, json: () => Promise.resolve([{ id: 1, title: 'M', rating: 5 }]) })
    vi.spyOn(window, 'confirm').mockImplementation(() => true)
    
    render(<App />)
    await waitFor(() => expect(screen.getByText('M')).toBeInTheDocument())
    
    fetch.mockRejectedValueOnce(new Error('Delete failed'))
    fireEvent.click(screen.getByTitle('Delete'))
    
    await waitFor(() => expect(consoleSpy).toHaveBeenCalledWith('Error deleting movie:', expect.any(Error)))
    consoleSpy.mockRestore()
  })

  it('updates description and rating in edit form', async () => {
    fetch.mockResolvedValueOnce({ ok: true, json: () => Promise.resolve([{ id: 1, title: 'M', description: 'D', rating: 5 }]) })
    
    render(<App />)
    await waitFor(() => expect(screen.getByText('M')).toBeInTheDocument())
    
    fireEvent.click(screen.getByTitle('Edit'))
    
    const descInput = screen.getByPlaceholderText('Description')
    fireEvent.change(descInput, { target: { value: 'New Description' } })
    
    const ratingInput = screen.getByLabelText(/Rating:/i)
    fireEvent.change(ratingInput, { target: { value: '8.5' } })
    
    expect(screen.getByText('Rating: 8.5')).toBeInTheDocument()
    expect(descInput.value).toBe('New Description')
  })
})
