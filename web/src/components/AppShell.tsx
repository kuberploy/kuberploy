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
import { hasPlatformUpgradeCapability } from "../lib/upgradeAccess";
import {
  applyTheme,
  applyThemePreference,
  persistThemePreference,
  resolveThemePreference,
  watchSystemTheme,
  type ThemePreference,
} from "../lib/theme";
import { Icon, type IconName } from "./Icon";
import { Button } from "./ui";
import { ToggleGroup, ToggleGroupItem } from "./shadcn/toggle-group";
import { Monitor, Moon, Sun } from "lucide-react";

const navigation: Array<{
  to: "/" | "/projects" | "/teams" | "/deploy";
  label: string;
  icon: IconName;
}> = [
  { to: "/", label: "Overview", icon: "grid" },
  { to: "/projects", label: "Projects", icon: "layers" },
  { to: "/teams", label: "Teams & GitHub", icon: "user" },
  { to: "/deploy", label: "Deploy image", icon: "deploy" },
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
    // session. Invalidating only `me` retains its stale Principal when the
    // expected 401 refetch arrives, leaving a signed-out user in the shell.
    onSuccess: () => queryClient.clear(),
  });

  useEffect(() => {
    applyThemePreference(themePreference);
    if (themePreference !== "system") return;
    return watchSystemTheme(applyTheme);
  }, [themePreference]);

  const pageName = pathname.match(/^\/projects\/[^/]+$/)
    ? "Project"
    : pathname.match(/^\/applications\/[^/]+\/deployments\/[^/]+$/)
      ? "Deployment"
      : pathname.match(/^\/applications\/[^/]+$/)
        ? "Service"
        : pathname === "/"
          ? "Overview"
          : (pathname.split("/").filter(Boolean).at(-1)?.replace(/-/g, " ") ??
            "Overview");

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
          {navigation.map((item) => (
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
          {capabilities.data?.features?.githubAppSetup === true ||
          (capabilities.data?.features?.builds === true &&
            hasPotentialBuildAccess(capabilities.data?.capabilities ?? [])) ? (
            <Link
              to="/builds"
              activeProps={{ className: "nav-link nav-link--active" }}
              inactiveProps={{ className: "nav-link" }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="terminal" />
              <span>Source builds</span>
            </Link>
          ) : null}
          {capabilities.data?.features?.registry === true &&
          hasRegistryPlatformCapability(
            capabilities.data?.capabilities ?? [],
            "registry-targets:read",
          ) ? (
            <Link
              to="/registry"
              activeProps={{ className: "nav-link nav-link--active" }}
              inactiveProps={{ className: "nav-link" }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="layers" />
              <span>Registry</span>
            </Link>
          ) : null}
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
          <div className="nav-section-label nav-section-label--spaced">
            Platform
          </div>
          <Link
            to="/audit"
            activeProps={{ className: "nav-link nav-link--active" }}
            inactiveProps={{ className: "nav-link" }}
            onClick={() => setMobileOpen(false)}
          >
            <Icon name="logs" />
            <span>Audit timeline</span>
          </Link>
          <Link
            to="/setup"
            activeProps={{ className: "nav-link nav-link--active" }}
            inactiveProps={{ className: "nav-link" }}
            onClick={() => setMobileOpen(false)}
          >
            <Icon name="settings" />
            <span>Setup & health</span>
          </Link>
          {user.authentication.kind === "session" &&
          hasPlatformUpgradeCapability(
            capabilities.data?.capabilities ?? [],
            "platform-releases:read",
          ) ? (
            <Link
              to="/settings/upgrade"
              activeProps={{ className: "nav-link nav-link--active" }}
              inactiveProps={{ className: "nav-link" }}
              onClick={() => setMobileOpen(false)}
            >
              <Icon name="refresh" />
              <span>Platform upgrade</span>
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
          capabilities.data?.features?.certificateIssuerManagement === true ? (
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
            <span className="topbar__cluster">Local cluster</span>
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
                >
                  <ThemeIcon className="size-3.5" />
                </ToggleGroupItem>
              ))}
            </ToggleGroup>
            <span className="user-menu__avatar">
              {(user.displayName || user.login || "A")
                .slice(0, 1)
                .toUpperCase()}
            </span>
            <span className="user-menu__copy">
              <strong>{user.displayName || user.login}</strong>
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
