import "@testing-library/jest-dom/vitest";

// jsdom has no layout and reports every floating-control anchor as a zero-size,
// hidden rectangle. Give Base UI listboxes stable geometry so interaction tests
// exercise open, keyboard navigation, and item selection instead of immediately
// closing through Floating UI's detached-anchor guard.
HTMLElement.prototype.getBoundingClientRect = function getBoundingClientRect() {
  return {
    bottom: 40,
    height: 40,
    left: 0,
    right: 240,
    top: 0,
    width: 240,
    x: 0,
    y: 0,
    toJSON: () => ({}),
  };
};

// jsdom doesn't implement matchMedia at all. Default every query to
// non-matching (e.g. a "narrow viewport" media query resolves to desktop)
// so components that check it don't throw in tests that aren't exercising
// that behavior; tests that care stub `window.matchMedia` themselves.
window.matchMedia ??= (query: string) => ({
  matches: false,
  media: query,
  onchange: null,
  addListener: () => {},
  removeListener: () => {},
  addEventListener: () => {},
  removeEventListener: () => {},
  dispatchEvent: () => false,
});
