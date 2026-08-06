import '@testing-library/jest-dom';

// JSDOM doesn't ship IntersectionObserver. Components that use it for
// infinite-scroll need a stub. The real behaviour is exercised in E2E.
if (typeof globalThis.IntersectionObserver === 'undefined') {
  class IntersectionObserverStub {
    observe() {}
    unobserve() {}
    disconnect() {}
    takeRecords() {
      return [];
    }
    root = null;
    rootMargin = '';
    thresholds = [];
  }
  // @ts-expect-error - test-only global polyfill
  globalThis.IntersectionObserver = IntersectionObserverStub;
}