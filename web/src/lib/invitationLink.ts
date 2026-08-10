const invitationFragmentKey = "invite";

type LocationInput = Pick<Location, "href" | "hash" | "pathname" | "search">;
type HistoryInput = Pick<History, "replaceState" | "state">;

export function buildInvitationLink(
  token: string,
  location: Pick<LocationInput, "href"> = window.location,
): string {
  const url = new URL("/", location.href);
  url.hash = new URLSearchParams({ [invitationFragmentKey]: token }).toString();
  return url.toString();
}

export function invitationTokenFromHash(hash: string): string | null {
  if (!hash.startsWith("#")) return null;
  const token = new URLSearchParams(hash.slice(1)).get(invitationFragmentKey);
  if (!token || token.length > 128 || !/^[A-Za-z0-9_-]+$/.test(token)) {
    return null;
  }
  return token;
}

export function clearInvitationFragment(
  location: LocationInput = window.location,
  history: HistoryInput = window.history,
): void {
  if (
    !location.hash.startsWith("#") ||
    !new URLSearchParams(location.hash.slice(1)).has(invitationFragmentKey)
  ) {
    return;
  }
  history.replaceState(
    history.state,
    "",
    `${location.pathname}${location.search}`,
  );
}
