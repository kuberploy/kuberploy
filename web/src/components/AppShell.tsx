import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import type { Principal } from "../api/types";
import { api } from "../api/client";
import { hasMonitoringNavigationAccess } from "../lib/monitoringAccess";
import { hasRegistryPlatformCapability } from "../lib/registryAccess";
import { hasExternalDNSPlatformCapability } from "../lib/externalDNSAccess";
import { hasPotentialBuildAccess } from "../lib/buildAccess";
import { hasPlatformReleaseCapability } from "../lib/releaseAccess";
import {
  applyThemePreference,
  persistThemePreference,
  resolveThemePreference,
  type ThemePreference,
} from "../lib/theme";
import { Icon, type IconName } from "./Icon";
import { cn } from "@/lib/utils";
import { Button, useRovingFocus } from "./ui";

// One nav row. Declared once instead of on twelve <Link>s: TanStack Router
// takes the class through activeProps/inactiveProps, so it has to be a string.
const navLink =
  "my-0.5 flex min-h-[43px] items-center gap-3 rounded-[9px] border border-transparent px-3 text-sm font-medium text-sidebar-ink transition duration-(--motion-fast) ease-(--ease-standard) hover:bg-surface-soft hover:text-sidebar-ink-strong [&_svg]:size-[18px]";
const navLinkActive = `${navLink} border-line bg-mint-soft text-sidebar-ink-strong shadow-[inset_3px_0_0_var(--mint)] [&_svg]:text-mint`;
// Section parent: reads as "you are somewhere in here" without competing with
// the single current-page row below it.
const navLinkSection = `${navLink} font-semibold text-sidebar-ink-strong [&_svg]:text-sidebar-ink-strong`;

const themeOptions: ReadonlyArray<
  readonly [ThemePreference, IconName, string]
> = [
  ["system", "monitor", "Use automatic theme"],
  ["light", "sun", "Use light theme"],
  ["dark", "moon", "Use dark theme"],
];

/*
 * Single-choice control, so the group is a radiogroup holding one tab stop.
 * `.theme-control` was already in the stylesheet with no markup using it; this
 * puts the tokens back in charge of a control that had been rendering raw
 * Tailwind utilities.
 */
function ThemeControl({
  value,
  onChange,
}: {
  value: ThemePreference;
  onChange: (next: ThemePreference) => void;
}) {
  const activeIndex = themeOptions.findIndex(([option]) => option === value);
  const itemProps = useRovingFocus(themeOptions.length, activeIndex);
  return (
    <div
      className="grid h-[34px] grid-cols-[repeat(3,30px)] items-stretch rounded-lg border border-line bg-surface p-0.5 pointer-coarse:h-auto pointer-coarse:grid-cols-3"
      role="radiogroup"
      aria-label="Appearance"
    >
      {themeOptions.map(([preference, icon, label], index) => (
        <button
          key={preference}
          type="button"
          role="radio"
          className="grid place-items-center rounded-[5px] text-ink-soft outline-none hover:bg-surface-soft hover:text-ink aria-checked:bg-mint-soft aria-checked:text-mint-dark focus-visible:outline-2 focus-visible:-outline-offset-1 focus-visible:outline-mint pointer-coarse:min-h-8 pointer-coarse:min-w-8 [&_svg]:size-3.5"
          aria-checked={preference === value}
          aria-label={label}
          title={label.replace("Use ", "")}
          onClick={() => onChange(preference)}
          {...itemProps(index)}
        >
          <Icon name={icon} />
        </button>
      ))}
    </div>
  );
}

export function AppShell({ user }: { user: Principal }) {
  const [mobileOpen, setMobileOpen] = useState(false);
  const [themePreference, setThemePreference] = useState<ThemePreference>(
    resolveThemePreference,
  );
  const pathname = useRouterState({
    select: (state) => state.location.pathname,
  });
  const queryClient = useQueryClient();
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
    staleTime: 60_000,
  });
  const logout = useMutation({
    mutationFn: api.logout,
    // Remove every cached tenant projection as soon as the server revokes the
    // session, then reset the observed `me` query so the root re-fetches it
    // and transitions to the signed-out screen on the expected 401.
    onSuccess: async () => {
      queryClient.removeQueries({
        predicate: (query) => query.queryKey[0] !== "me",
      });
      await queryClient.resetQueries({ queryKey: ["me"], exact: true });
    },
  });

  useEffect(() => {
    applyThemePreference(themePreference);
  }, [themePreference]);

  const pageName = pathname.match(/^\/projects\/[^/]+$/)
    ? "Project"
    : pathname.match(/^\/applications\/[^/]+\/deployments\/[^/]+$/)
      ? "App"
      : pathname.match(/^\/applications\/[^/]+$/)
        ? "App"
        : pathname === "/deploy"
          ? "Add App"
          : pathname === "/"
            ? "Dashboard"
            : (pathname.split("/").filter(Boolean).at(-1)?.replace(/-/g, " ") ??
              "Dashboard");

  const registryNavigationVisible =
    capabilities.data?.features?.registry === true &&
    hasRegistryPlatformCapability(
      capabilities.data?.capabilities ?? [],
      "registry-targets:read",
    );
  const gitProviderNavigationVisible =
    capabilities.data?.features?.githubAppSetup === true ||
    (capabilities.data?.features?.builds === true &&
      hasPotentialBuildAccess(capabilities.data?.capabilities ?? []));
  const settingsActive =
    pathname === "/" ||
    pathname === "/teams" ||
    pathname === "/monitoring" ||
    pathname === "/audit" ||
    pathname === "/external-dns" ||
    pathname === "/setup" ||
    pathname.startsWith("/settings/");

  return (
    <div className="min-h-screen">
      <a
        className="fixed top-2 left-2 z-[100] -translate-y-[150%] rounded-lg bg-ink px-4 py-3 text-surface focus:translate-y-0"
        href="#main-content"
      >
        Skip to main content
      </a>
      <aside
        className={cn(
          "fixed inset-y-0 left-0 z-30 flex w-60 flex-col border-r border-line bg-sidebar px-4 pt-6 pb-5 text-sidebar-ink",
          // Off-canvas below 820px; the scrim and close button appear with it.
          "to-820:-translate-x-[105%] to-820:shadow-[15px_0_45px_rgba(0,0,0,0.2)] to-820:transition-transform to-820:duration-(--motion-base) to-820:ease-(--ease-standard)",
          mobileOpen && "to-820:translate-x-0",
        )}
      >
        <div className="flex items-center gap-3 px-2 text-lg font-semibold tracking-[-0.02em] text-sidebar-ink-strong">
          <span
            className="relative inline-flex size-[27px] items-end justify-center gap-0.5 rounded-lg border border-line-strong bg-mint-soft p-1.5 [&>span]:w-[3px] [&>span]:rounded-sm [&>span]:bg-mint [&>span:nth-child(1)]:h-[7px] [&>span:nth-child(2)]:h-[14px] [&>span:nth-child(3)]:h-[10px]"
            aria-hidden="true"
          >
            <span />
            <span />
            <span />
          </span>
          <span>Kuberploy</span>
        </div>
        <button
          className="absolute top-5 right-[15px] hidden size-[30px] place-items-center border-0 bg-none text-sidebar-ink to-820:grid to-820:size-9 [&_svg]:w-[17px]"
          onClick={() => setMobileOpen(false)}
          aria-label="Close navigation"
        >
          <Icon name="close" />
        </button>
        <nav
          className="mt-10 flex flex-1 flex-col"
          aria-label="Main navigation"
        >
          <div className="px-3 pb-2 text-[11px] font-semibold tracking-[0.08em] text-ink-faint uppercase">
            Workspace
          </div>
          <div role="group" aria-label="Primary navigation">
            <Link
              to="/projects"
              activeProps={{ className: navLinkActive }}
              inactiveProps={{ className: navLink }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="layers" />
              <span>Projects</span>
            </Link>
            {registryNavigationVisible ? (
              <Link
                to="/registry"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="package" />
                <span>Registries</span>
              </Link>
            ) : null}
            {gitProviderNavigationVisible ? (
              <Link
                to="/builds"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="terminal" />
                <span>Git Providers</span>
              </Link>
            ) : null}
            <Link
              to="/setup"
              activeProps={{ className: navLinkActive }}
              inactiveProps={{
                // Parent of the settings group: marked as the active section
                // without stealing the current-page treatment from its child.
                className: settingsActive ? navLinkSection : navLink,
              }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="settings" />
              <span>Settings</span>
            </Link>
          </div>
          <div className="mt-6 px-3 pb-2 text-[11px] font-semibold tracking-[0.08em] text-ink-faint uppercase">
            Settings pages
          </div>
          <div role="group" aria-label="Settings navigation">
            <Link
              to="/"
              activeOptions={{ exact: true }}
              activeProps={{ className: navLinkActive }}
              inactiveProps={{ className: navLink }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="grid" />
              <span>Dashboard</span>
            </Link>
            {hasMonitoringNavigationAccess(
              capabilities.data?.capabilities ?? [],
            ) ? (
              <Link
                to="/monitoring"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="metrics" />
                <span>Monitoring</span>
              </Link>
            ) : null}
            <Link
              to="/audit"
              activeProps={{ className: navLinkActive }}
              inactiveProps={{ className: navLink }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="logs" />
              <span>Audit timeline</span>
            </Link>
            <Link
              to="/teams"
              activeProps={{ className: navLinkActive }}
              inactiveProps={{ className: navLink }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="user" />
              <span>Teams</span>
            </Link>
            {user.role === "platform-admin" &&
            user.authentication.kind === "session" &&
            capabilities.data?.features?.builds === true ? (
              <Link
                to="/settings/builders"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="terminal" />
                <span>Source builders</span>
              </Link>
            ) : null}
            {user.authentication.kind === "session" &&
            capabilities.data?.features?.middlewareProfiles === true ? (
              <Link
                to="/settings/middleware-profiles"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="route" />
                <span>Middleware profiles</span>
              </Link>
            ) : null}
            {user.role === "platform-admin" &&
            user.authentication.kind === "session" &&
            capabilities.data?.features?.certificateIssuerManagement ===
              true ? (
              <Link
                to="/settings/certificate-issuers"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="shield" />
                <span>Certificate issuers</span>
              </Link>
            ) : null}
            {capabilities.data?.features?.externalDNSConfiguration === true &&
            hasExternalDNSPlatformCapability(
              capabilities.data?.capabilities ?? [],
              "external-dns-integrations:read",
            ) ? (
              <Link
                to="/external-dns"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="globe" />
                <span>External DNS</span>
              </Link>
            ) : null}
            {user.role === "platform-admin" ? (
              <Link
                to="/settings/argo-git"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="git" />
                <span>Argo Git authority</span>
              </Link>
            ) : null}
            {user.authentication.kind === "session" &&
            hasPlatformReleaseCapability(
              capabilities.data?.capabilities ?? [],
              "platform-releases:read",
            ) ? (
              <Link
                to="/settings/releases"
                activeProps={{ className: navLinkActive }}
                inactiveProps={{ className: navLink }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="refresh" />
                <span>Platform releases</span>
              </Link>
            ) : null}
          </div>
        </nav>
        <div className="flex flex-col gap-3 border-t border-line pt-4">
          <div className="flex items-center gap-2 px-2 text-[11px] text-sidebar-ink">
            <span className="size-[7px] rounded-full bg-mint" />
            Cluster connected
          </div>
          <a
            href="/docs"
            className="flex items-center gap-2 p-2 text-[11px] text-sidebar-ink [&_svg]:w-3.5 [&_svg:last-child]:ml-auto"
          >
            <Icon name="code" /> API documentation <Icon name="external" />
          </a>
        </div>
      </aside>
      {mobileOpen ? (
        <button
          className="fixed inset-0 z-[25] hidden animate-[fade-in_var(--motion-base)_var(--ease-standard)] border-0 bg-[rgba(5,15,11,0.5)] backdrop-blur-[2px] to-820:block"
          onClick={() => setMobileOpen(false)}
          aria-label="Close navigation"
        />
      ) : null}

      <div className="ml-60 min-h-screen to-820:ml-0">
        <header className="sticky top-0 z-20 flex h-[68px] items-center justify-between gap-4 border-b border-line bg-topbar px-8 backdrop-blur-xl to-820:justify-start to-820:px-5">
          <button
            className="mr-3 hidden size-[34px] place-items-center rounded-lg border border-line bg-surface text-ink to-820:grid [&_svg]:w-[17px]"
            onClick={() => setMobileOpen(true)}
            aria-label="Open navigation"
          >
            <Icon name="menu" />
          </button>
          <div className="flex min-w-0 items-center gap-2 text-sm whitespace-nowrap to-580:hidden [&_svg]:w-[13px] [&_svg]:text-ink-faint">
            <span className="text-ink-faint to-940:hidden">
              Kubernetes cluster
            </span>
            <Icon name="chevron" />
            <span className="overflow-hidden font-medium text-ellipsis capitalize">
              {pageName}
            </span>
          </div>
          <div className="flex items-center gap-3 to-820:ml-auto">
            <ThemeControl
              value={themePreference}
              onChange={(next) => {
                persistThemePreference(next);
                setThemePreference(next);
              }}
            />
            <span className="grid size-8 place-items-center rounded-[9px] border border-line-strong bg-mint-soft text-xs font-bold text-mint-dark">
              {(user.displayName || "A").slice(0, 1).toUpperCase()}
            </span>
            <span className="flex min-w-[112px] flex-col to-820:hidden [&_small]:text-[11px] [&_small]:text-ink-faint [&_small]:capitalize [&_strong]:max-w-[130px] [&_strong]:overflow-hidden [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_strong]:text-[13px]">
              <strong>{user.displayName}</strong>
              <small>{user.role}</small>
            </span>
            <Button
              variant="ghost"
              onClick={() => logout.mutate()}
              busy={logout.isPending}
            >
              Sign out
            </Button>
          </div>
        </header>
        <main id="main-content" className="w-full outline-none" tabIndex={-1}>
          <Outlet />
        </main>
      </div>
    </div>
  );
}
