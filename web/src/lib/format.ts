export function formatDate(value?: string): string {
  if (!value) return "Not reported";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}

export function relativeTime(value?: string): string {
  if (!value) return "just now";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const deltaSeconds = Math.round((date.getTime() - Date.now()) / 1000);
  const formatter = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  const units: Array<[Intl.RelativeTimeFormatUnit, number]> = [
    ["year", 31_536_000],
    ["month", 2_592_000],
    ["day", 86_400],
    ["hour", 3_600],
    ["minute", 60],
  ];
  for (const [unit, seconds] of units) {
    if (Math.abs(deltaSeconds) >= seconds)
      return formatter.format(Math.round(deltaSeconds / seconds), unit);
  }
  return formatter.format(deltaSeconds, "second");
}

export function shortId(value?: string, size = 8): string {
  if (!value) return "—";
  const cleaned = value.replace(/^sha256:/, "");
  return cleaned.length > size ? cleaned.slice(0, size) : cleaned;
}

export function titleCase(value?: string): string {
  if (!value) return "Unknown";
  return value
    .replace(/([a-z])([A-Z])/g, "$1 $2")
    .replace(/[-_]/g, " ")
    .replace(/^./, (character) => character.toUpperCase());
}

export function operationTone(
  state?: string,
): "good" | "warn" | "bad" | "busy" | "neutral" {
  const normalized = state?.toLowerCase();
  if (
    normalized &&
    ["healthy", "succeeded", "synced", "ready", "active", "available"].includes(
      normalized,
    )
  )
    return "good";
  if (
    normalized &&
    ["failed", "degraded", "error", "unhealthy"].includes(normalized)
  )
    return "bad";
  if (
    normalized &&
    ["pending", "waiting", "superseded", "cancelled", "disabled"].includes(
      normalized,
    )
  )
    return "warn";
  if (
    normalized &&
    [
      "requested",
      "queued",
      "running",
      "building",
      "reconciling",
      "configpending",
      "gitcommitted",
    ].includes(normalized)
  )
    return "busy";
  return "neutral";
}
