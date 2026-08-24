import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import type { Principal } from "../api/types";
import { api } from "../api/client";
import { hasMonitoringNavigationAccess } from "../lib/monitoringAccess";
import { hasRegistryPlatformCapability } from "../lib/registryAccess";
import { hasExternalDNSPlatformCapability } from "../lib/externalDNSAccess";
import { hasPotentialBuildAccess } from "../lib/buildAccess";
import { hasHelmApprovalManagementAccess } from "../lib/helmApprovalAccess";
import { hasPlatformReleaseCapability } from "../lib/releaseAccess";
import {
  applyThemePreference,
  persistThemePreference,
  resolveThemePreference,
  type ThemePreference,
} from "../lib/theme";
import { Icon, type IconName } from "./Icon";
import { Button } from "./ui";
import { ToggleGroup, ToggleGroupItem } from "./shadcn/toggle-group";
import { Monitor, Moon, Sun } from "lucide-react";

const settingsNavigation: Array<{
  to: "/" | "/teams";
  label: string;
  icon: IconName;
}> = [
  { to: "/", label: "Dashboard", icon: "grid" },
  { to: "/teams", label: "Teams", icon: "user" },
];

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
    <div className="app-shell">
      <a className="skip-link" href="#main-content">
        Skip to main content
      </a>
      <aside className={`sidebar ${mobileOpen ? "sidebar--open" : ""}`}>
        <div className="brand">
          <span className="brand__mark" aria-hidden="true">
            <span />
            <span />
            <span />
          </span>
          <span>Kuberploy</span>
        </div>
        <button
          className="sidebar__close"
          onClick={() => setMobileOpen(false)}
          aria-label="Close navigation"
        >
          <Icon name="close" />
        </button>
        <nav aria-label="Main navigation">
          <div className="nav-section-label">Workspace</div>
          <div role="group" aria-label="Primary navigation">
            <Link
              to="/projects"
              activeProps={{ className: "nav-link nav-link--active" }}
              inactiveProps={{ className: "nav-link" }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="layers" />
              <span>Projects</span>
            </Link>
            {registryNavigationVisible ? (
              <Link
                to="/registry"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="layers" />
                <span>Registries</span>
              </Link>
            ) : null}
            {gitProviderNavigationVisible ? (
              <Link
                to="/builds"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="terminal" />
                <span>Git Providers</span>
              </Link>
            ) : null}
            <Link
              to="/setup"
              activeProps={{ className: "nav-link nav-link--active" }}
              inactiveProps={{
                className: settingsActive
                  ? "nav-link nav-link--active"
                  : "nav-link",
              }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="settings" />
              <span>Settings</span>
            </Link>
          </div>
          <div className="nav-section-label nav-section-label--spaced">
            Settings pages
          </div>
          <div role="group" aria-label="Settings navigation">
            {settingsNavigation.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                activeOptions={{ exact: item.to === "/" }}
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name={item.icon} />
                <span>{item.label}</span>
              </Link>
            ))}
            {hasMonitoringNavigationAccess(
              capabilities.data?.capabilities ?? [],
            ) ? (
              <Link
                to="/monitoring"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="metrics" />
                <span>Monitoring</span>
              </Link>
            ) : null}
            <Link
              to="/audit"
              activeProps={{ className: "nav-link nav-link--active" }}
              inactiveProps={{ className: "nav-link" }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="logs" />
              <span>Audit timeline</span>
            </Link>
            {capabilities.data?.features?.externalDNSConfiguration === true &&
            hasExternalDNSPlatformCapability(
              capabilities.data?.capabilities ?? [],
              "external-dns-integrations:read",
            ) ? (
              <Link
                to="/external-dns"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="route" />
                <span>External DNS</span>
              </Link>
            ) : null}
            {user.authentication.kind === "session" &&
            hasPlatformReleaseCapability(
              capabilities.data?.capabilities ?? [],
              "platform-releases:read",
            ) ? (
              <Link
                to="/settings/releases"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="refresh" />
                <span>Platform releases</span>
              </Link>
            ) : null}
            {user.role === "platform-admin" ? (
              <Link
                to="/settings/argo-git"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="route" />
                <span>Argo Git authority</span>
              </Link>
            ) : null}
            {hasHelmApprovalManagementAccess(
              user,
              capabilities.data?.features,
              capabilities.data?.capabilities ?? [],
            ) ? (
              <Link
                to="/settings/helm-approvals"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="layers" />
                <span>Helm approvals</span>
              </Link>
            ) : null}
            {user.authentication.kind === "session" &&
            capabilities.data?.features?.middlewareProfiles === true ? (
              <Link
                to="/settings/middleware-profiles"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="route" />
                <span>Middleware profiles</span>
              </Link>
            ) : null}
            {user.role === "platform-admin" &&
            user.authentication.kind === "session" &&
            capabilities.data?.features?.builds === true ? (
              <Link
                to="/settings/builders"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="terminal" />
                <span>Source builders</span>
              </Link>
            ) : null}
            {user.role === "platform-admin" &&
            user.authentication.kind === "session" &&
            capabilities.data?.features?.certificateIssuerManagement ===
              true ? (
              <Link
                to="/settings/certificate-issuers"
                activeProps={{ className: "nav-link nav-link--active" }}
                inactiveProps={{ className: "nav-link" }}
                onClick={() => setMobileOpen(false)}
              >
                <Icon name="route" />
                <span>Certificate issuers</span>
              </Link>
            ) : null}
          </div>
        </nav>
        <div className="sidebar__footer">
          <div className="cluster-chip">
            <span className="cluster-chip__pulse" />
            Cluster connected
          </div>
          <a href="/docs" className="sidebar-docs">
            <Icon name="code" /> API documentation <Icon name="external" />
          </a>
        </div>
      </aside>
      {mobileOpen ? (
        <button
          className="sidebar-scrim"
          onClick={() => setMobileOpen(false)}
          aria-label="Close navigation"
        />
      ) : null}

      <div className="app-frame">
        <header className="topbar">
          <button
            className="mobile-menu"
            onClick={() => setMobileOpen(true)}
            aria-label="Open navigation"
          >
            <Icon name="menu" />
          </button>
          <div className="topbar__context">
            <span className="topbar__cluster">Kubernetes cluster</span>
            <Icon name="chevron" />
            <span className="topbar__page">{pageName}</span>
          </div>
          <div className="user-menu">
            <ToggleGroup
              type="single"
              aria-label="Appearance"
              value={themePreference}
              onValueChange={(value) => {
                if (!value) return;
                const next = value as ThemePreference;
                persistThemePreference(next);
                setThemePreference(next);
              }}
            >
              {(
                [
                  ["system", Monitor, "Use automatic theme"],
                  ["light", Sun, "Use light theme"],
                  ["dark", Moon, "Use dark theme"],
                ] as const
              ).map(([preference, ThemeIcon, label]) => (
                <ToggleGroupItem
                  key={preference}
                  type="button"
                  aria-label={label}
                  title={label.replace("Use ", "")}
                  value={preference}
                  onKeyDown={(event) => {
                    // Keep Space activation reliable across browsers and
                    // assistive-input drivers. Prevent the native button
                    // default so the explicit click cannot fire twice.
                    if (event.key === " " || event.key === "Spacebar") {
                      event.preventDefault();
                      event.currentTarget.click();
                    }
                  }}
                >
                  <ThemeIcon className="size-3.5" />
                </ToggleGroupItem>
              ))}
            </ToggleGroup>
            <span className="user-menu__avatar">
              {(user.displayName || "A").slice(0, 1).toUpperCase()}
            </span>
            <span className="user-menu__copy">
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
        <main id="main-content" className="main-content" tabIndex={-1}>
          <Outlet />
        </main>
      </div>
    </div>
  );
}
