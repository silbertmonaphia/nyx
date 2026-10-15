import { useEffect, useState } from 'react';

/**
 * Returns a value that only updates after `delay` milliseconds have
 * elapsed since the last change. Useful for throttling fast-changing
 * inputs (e.g. search boxes) that drive expensive downstream work
 * like network requests.
 *
 * The initial value is returned synchronously on the first render so
 * the hook is safe to feed directly into a query key. Subsequent
 * values only land after the caller has stopped updating for the full
 * delay, which is what collapses a burst of keystrokes into a single
 * effect.
 *
 * The pending timer is cleared both on value change and on unmount,
 * so the hook never setStates after the component has gone away.
 */
export function useDebounce<T>(value: T, delay: number = 300): T {
  const [debouncedValue, setDebouncedValue] = useState<T>(value);

  useEffect(() => {
    const timer = setTimeout(() => {
      setDebouncedValue(value);
    }, delay);

    return () => {
      clearTimeout(timer);
    };
  }, [value, delay]);

  return debouncedValue;
}
