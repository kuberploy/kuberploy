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
