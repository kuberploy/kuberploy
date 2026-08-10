export type Theme = "light" | "dark";
export type ThemePreference = Theme | "system";

const storageKey = "kuberploy-theme";

export function resolveThemePreference(): ThemePreference {
  const saved = globalThis.localStorage?.getItem(storageKey);
  if (saved === "light" || saved === "dark" || saved === "system") return saved;
  return "system";
}

export function resolveSystemTheme(): Theme {
  return globalThis.matchMedia?.("(prefers-color-scheme: dark)").matches
    ? "dark"
    : "light";
}

export function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme;
  document.documentElement.style.colorScheme = theme;
  document
    .querySelector('meta[name="theme-color"]')
    ?.setAttribute("content", theme === "dark" ? "#111214" : "#f6f7f8");
}

export function applyThemePreference(preference: ThemePreference) {
  document.documentElement.dataset.themePreference = preference;
  applyTheme(preference === "system" ? resolveSystemTheme() : preference);
}

export function persistThemePreference(preference: ThemePreference) {
  globalThis.localStorage?.setItem(storageKey, preference);
  applyThemePreference(preference);
}

export function watchSystemTheme(onChange: (theme: Theme) => void) {
  const query = globalThis.matchMedia?.("(prefers-color-scheme: dark)");
  if (!query) return () => undefined;
  const handleChange = () => onChange(query.matches ? "dark" : "light");
  query.addEventListener?.("change", handleChange);
  return () => query.removeEventListener?.("change", handleChange);
}
