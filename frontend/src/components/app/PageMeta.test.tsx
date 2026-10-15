import { describe, it, expect, beforeEach } from 'vitest';
import { render } from '@testing-library/react';
import PageMeta from './PageMeta';

describe('PageMeta', () => {
  beforeEach(() => {
    // Reset document.title between tests — it persists across renders.
    document.title = '';
    document.querySelectorAll('meta[name="description"]').forEach((el) => el.remove());
  });

  it('updates document.title to the provided title', () => {
    render(<PageMeta title="Hello" />);
    expect(document.title).toBe('Hello');
  });

  it('writes the meta description when provided', () => {
    render(<PageMeta title="x" description="a short desc" />);
    const meta = document.querySelector('meta[name="description"]');
    expect(meta).not.toBeNull();
    expect(meta?.getAttribute('content')).toBe('a short desc');
  });

  it('does not emit a meta tag when description is omitted', () => {
    render(<PageMeta title="no desc" />);
    expect(document.querySelector('meta[name="description"]')).toBeNull();
  });

  it('updates the title when re-rendered with a new value', () => {
    const { rerender } = render(<PageMeta title="first" />);
    expect(document.title).toBe('first');
    rerender(<PageMeta title="second" />);
    expect(document.title).toBe('second');
  });
});