import { renderHook, act } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useDebounce } from './useDebounce';

describe('useDebounce', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('returns the initial value immediately without waiting for the delay', () => {
    const { result } = renderHook(() => useDebounce('initial', 300));

    expect(result.current).toBe('initial');
  });

  it('updates the returned value only after the delay elapses', () => {
    const { result, rerender } = renderHook(
      ({ value }: { value: string }) => useDebounce(value, 300),
      { initialProps: { value: 'initial' } },
    );

    rerender({ value: 'next' });

    // Right after the change, the debounced value still reflects the
    // previous one — the user is mid-typing.
    expect(result.current).toBe('initial');

    act(() => {
      vi.advanceTimersByTime(299);
    });
    expect(result.current).toBe('initial');

    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(result.current).toBe('next');
  });

  it('collapses a burst of rapid changes into the final value', () => {
    const { result, rerender } = renderHook(
      ({ value }: { value: string }) => useDebounce(value, 300),
      { initialProps: { value: '' } },
    );

    rerender({ value: 'a' });
    act(() => {
      vi.advanceTimersByTime(100);
    });

    rerender({ value: 'ab' });
    act(() => {
      vi.advanceTimersByTime(100);
    });

    rerender({ value: 'abc' });
    act(() => {
      vi.advanceTimersByTime(100);
    });

    // Still hasn't settled — the timer keeps getting reset.
    expect(result.current).toBe('');

    rerender({ value: 'abcd' });
    act(() => {
      vi.advanceTimersByTime(300);
    });

    expect(result.current).toBe('abcd');
  });

  it('clears the pending timer on unmount so no late setState fires', () => {
    const { result, rerender, unmount } = renderHook(
      ({ value }: { value: string }) => useDebounce(value, 300),
      { initialProps: { value: 'initial' } },
    );

    rerender({ value: 'next' });

    // Panics if a setState lands after unmount: React logs "Can't
    // perform a React state update on an unmounted component".
    unmount();
    act(() => {
      vi.advanceTimersByTime(500);
    });

    // result is frozen at the last committed value; most importantly
    // this should not throw.
    expect(result.current).toBe('initial');
  });

  it('respects a custom delay', () => {
    const { result, rerender } = renderHook(
      ({ value }: { value: string }) => useDebounce(value, 1000),
      { initialProps: { value: 'initial' } },
    );

    rerender({ value: 'next' });

    act(() => {
      vi.advanceTimersByTime(999);
    });
    expect(result.current).toBe('initial');

    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(result.current).toBe('next');
  });

  it('works with non-string values', () => {
    type Filter = { q: string; page: number };
    const { result, rerender } = renderHook(
      ({ value }: { value: Filter }) => useDebounce<Filter>(value, 300),
      {
        initialProps: { value: { q: '', page: 1 } as Filter },
      },
    );

    const next = { q: 'matrix', page: 1 };
    rerender({ value: next });

    act(() => {
      vi.advanceTimersByTime(300);
    });

    expect(result.current).toEqual(next);
  });
});
