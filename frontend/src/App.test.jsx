import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import App from './App'

describe('App component', () => {
  it('renders get started heading', () => {
    render(<App />)
    expect(screen.getByText(/Get started/i)).toBeInTheDocument()
  })

  it('increments count when button is clicked', () => {
    render(<App />)
    const button = screen.getByRole('button', { name: /Count is 0/i })
    fireEvent.click(button)
    expect(screen.getByRole('button', { name: /Count is 1/i })).toBeInTheDocument()
  })
})
