import '@testing-library/jest-dom';

// JSDOM doesn't ship IntersectionObserver. Components that use it for
// infinite-scroll need a stub. The real behaviour is exercised in E2E.
// The stub exposes the most-recent instance and its options so tests
// can assert against the observer's root (e.g. pinning it to a
// scroll container instead of the document viewport) and trigger
// callbacks deterministically.
if (typeof globalThis.IntersectionObserver === 'undefined') {
  class IntersectionObserverStub {
    constructor(_cb, options = {}) {
      this._cb = _cb;
      this.root = options.root ?? null;
      this.rootMargin = options.rootMargin ?? '';
      this.thresholds = options.threshold !== undefined ? [options.threshold].flat() : [0];
      IntersectionObserverStub.last = this;
      IntersectionObserverStub.instances.push(this);
    }
    observe() {}
    unobserve() {}
    disconnect() {}
    takeRecords() {
      return [];
    }
    /** Test-only: simulate an intersection event for this observer. */
    trigger(isIntersecting, target = null) {
      this._cb([{ isIntersecting, target, intersectionRatio: isIntersecting ? 1 : 0 }]);
    }
    static last = null;
    static instances = [];
    static reset() {
      IntersectionObserverStub.last = null;
      IntersectionObserverStub.instances = [];
    }
  }
  // @ts-expect-error - test-only global polyfill
  globalThis.IntersectionObserver = IntersectionObserverStub;
}

// JSDOM doesn't ship BroadcastChannel. The tokenStore uses it for
// cross-tab token sync; tests need a working stub. The real
// behaviour is exercised in E2E. This polyfill only delivers
// messages — it doesn't simulate origin partitioning (we're always
// same-origin in tests anyway). Force-install even if jsdom
// already provides one (jsdom's native BC only delivers across
// real browsing contexts, so two instances in the same jsdom
// process never see each other's messages — useless for tests).
{
  class BroadcastChannelStub extends EventTarget {
    name;
    constructor(name) {
      super();
      this.name = name;
      BroadcastChannelStub.channels.add(this);
    }
    postMessage(data) {
      // Fan out to every other instance with the same name. We
      // dispatch on a microtask so receivers see a fresh event
      // object (matching the spec).
      for (const ch of BroadcastChannelStub.channels) {
        if (ch !== this && ch.name === this.name) {
          queueMicrotask(() => {
            ch.dispatchEvent(
              new MessageEvent('message', { data, origin: 'test' }),
            );
          });
        }
      }
    }
    close() {
      BroadcastChannelStub.channels.delete(this);
    }
  }
  BroadcastChannelStub.channels = new Set();
  // @ts-expect-error - test-only global polyfill
  globalThis.BroadcastChannel = BroadcastChannelStub;
}