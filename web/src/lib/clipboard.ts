import { useCallback, useEffect, useRef, useState } from "react";

export type CopyState = "idle" | "copied" | "failed";

/** Legacy selection copy, used when the page is not a secure context. */
function copyWithSelection(value: string): boolean {
  if (typeof document === "undefined") return false;
  const field = document.createElement("textarea");
  field.value = value;
  field.setAttribute("readonly", "");
  field.style.position = "fixed";
  field.style.opacity = "0";
  document.body.append(field);
  try {
    field.select();
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    field.remove();
  }
}

/**
 * Shared clipboard helper so every "copy this value" affordance reports the
 * same three states and resets on its own instead of leaving a stale "Copied"
 * label behind.
 */
export function useCopyToClipboard(resetAfterMs = 2000) {
  const [state, setState] = useState<CopyState>("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
    },
    [],
  );

  const copy = useCallback(
    async (value: string) => {
      if (timer.current) clearTimeout(timer.current);
      let next: CopyState = "failed";
      try {
        if (navigator.clipboard?.writeText) {
          await navigator.clipboard.writeText(value);
          next = "copied";
        } else if (copyWithSelection(value)) {
          // Self-hosted control planes are often reached over plain HTTP, where
          // the async clipboard API is unavailable.
          next = "copied";
        }
      } catch {
        next = copyWithSelection(value) ? "copied" : "failed";
      }
      setState(next);
      timer.current = setTimeout(() => setState("idle"), resetAfterMs);
      return next === "copied";
    },
    [resetAfterMs],
  );

  return { state, copy };
}
