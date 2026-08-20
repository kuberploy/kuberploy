import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  applyTheme,
  applyThemePreference,
  persistThemePreference,
  resolveSystemTheme,
  resolveThemePreference,
  watchSystemTheme,
} from "./theme";

beforeEach(() => {
  const values = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    clear: () => values.clear(),
    getItem: (key: string) => values.get(key) ?? null,
    removeItem: (key: string) => values.delete(key),
    setItem: (key: string, value: string) => values.set(key, value),
  });
});

afterEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-theme-preference");
  document.documentElement.removeAttribute("style");
  vi.unstubAllGlobals();
});

describe("theme", () => {
  it("uses a saved preference before the system preference", () => {
    localStorage.setItem("kuberploy-theme", "light");
    vi.stubGlobal("matchMedia", () => ({ matches: true }));
    expect(resolveThemePreference()).toBe("light");
  });

  it("defaults to auto and resolves the system preference", () => {
    vi.stubGlobal("matchMedia", () => ({ matches: true }));
    expect(resolveThemePreference()).toBe("system");
    expect(resolveSystemTheme()).toBe("dark");
  });

  it("persists and applies an explicit theme", () => {
    persistThemePreference("dark");
    expect(localStorage.getItem("kuberploy-theme")).toBe("dark");
    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(document.documentElement.style.colorScheme).toBe("dark");

    applyTheme("light");
    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it("tracks system changes while auto mode is selected", () => {
    let dark = false;
    let listener: (() => void) | undefined;
    vi.stubGlobal("matchMedia", () => ({
      get matches() {
        return dark;
      },
      addEventListener: (_event: string, next: () => void) => {
        listener = next;
      },
      removeEventListener: vi.fn(),
    }));
    applyThemePreference("system");
    expect(document.documentElement.dataset.theme).toBe("light");
    const stop = watchSystemTheme(applyTheme);
    dark = true;
    listener?.();
    expect(document.documentElement.dataset.theme).toBe("dark");
    stop();
  });

  it("supports legacy system-theme listeners", () => {
    let dark = false;
    let listener: (() => void) | undefined;
    const removeListener = vi.fn();
    vi.stubGlobal("matchMedia", () => ({
      get matches() {
        return dark;
      },
      addListener: (next: () => void) => {
        listener = next;
      },
      removeListener,
    }));

    const stop = watchSystemTheme(applyTheme);
    dark = true;
    listener?.();

    expect(document.documentElement.dataset.theme).toBe("dark");
    stop();
    expect(removeListener).toHaveBeenCalledOnce();
  });
});
