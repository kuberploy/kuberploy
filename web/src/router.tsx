import {
  createRootRoute,
  createRoute,
  createRouter,
  lazyRouteComponent,
  Navigate,
  redirect,
} from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLayoutEffect, useState } from "react";
import { api, isUnauthorized } from "./api/client";
import type { Principal } from "./api/types";
import { AppShell } from "./components/AppShell";
import { AuthScreen } from "./components/AuthScreen";
import { Page, Skeleton } from "./components/ui";
import { NotFoundPage, RouteErrorPage } from "./pages/NotFoundPage";
import {
  clearInvitationFragment,
  invitationTokenFromHash,
} from "./lib/invitationLink";

const DashboardPage = lazyRouteComponent(
  () => import("./pages/DashboardPage"),
  "DashboardPage",
);
const ProjectsPage = lazyRouteComponent(
  () => import("./pages/ProjectsPage"),
  "ProjectsPage",
);
const ProjectPage = lazyRouteComponent(
  () => import("./pages/ProjectPage"),
  "ProjectPage",
);
const EnvironmentPage = lazyRouteComponent(
  () => import("./pages/EnvironmentPage"),
  "EnvironmentPage",
);
const AddAppPage = lazyRouteComponent(
  () => import("./pages/AddAppPage"),
  "AddAppPage",
);
const NewDeploymentPage = lazyRouteComponent(
  () => import("./pages/NewDeploymentPage"),
  "NewDeploymentPage",
);
const ApplicationPage = lazyRouteComponent(
  () => import("./pages/ApplicationPage"),
  "ApplicationPage",
);
const ApplicationOverviewPage = lazyRouteComponent(
  () => import("./pages/ApplicationOverviewPage"),
  "ApplicationOverviewPage",
);
const OperationPage = lazyRouteComponent(
  () => import("./pages/OperationPage"),
  "OperationPage",
);
const SetupPage = lazyRouteComponent(
  () => import("./pages/SetupPage"),
  "SetupPage",
);
const UpgradePage = lazyRouteComponent(
  () => import("./pages/UpgradePage"),
  "UpgradePage",
);
const TeamsPage = lazyRouteComponent(
  () => import("./pages/TeamsPage"),
  "TeamsPage",
);
const MonitoringPage = lazyRouteComponent(
  () => import("./pages/MonitoringPage"),
  "MonitoringPage",
);
const RegistryTargetsPage = lazyRouteComponent(
  () => import("./pages/RegistryTargetsPage"),
  "RegistryTargetsPage",
);
const ExternalDNSPage = lazyRouteComponent(
  () => import("./pages/ExternalDNSPage"),
  "ExternalDNSPage",
);
const SourceBuildsPage = lazyRouteComponent(
  () => import("./pages/SourceBuildsPage"),
  "SourceBuildsPage",
);
const BuildDetailPage = lazyRouteComponent(
  () => import("./pages/BuildDetailPage"),
  "BuildDetailPage",
);
const GitHubSetupCompletePage = lazyRouteComponent(
  () => import("./pages/GitHubSetupCompletePage"),
  "GitHubSetupCompletePage",
);
const PlatformArgoGitBindingPage = lazyRouteComponent(
  () => import("./pages/PlatformArgoGitBindingPage"),
  "PlatformArgoGitBindingPage",
);
const MiddlewareProfilesPage = lazyRouteComponent(
  () => import("./pages/MiddlewareProfilesPage"),
  "MiddlewareProfilesPage",
);
const VariableSetsPage = lazyRouteComponent(
  () => import("./pages/VariableSetsPage"),
  "VariableSetsPage",
);
const CertificateIssuersPage = lazyRouteComponent(
  () => import("./pages/CertificateIssuersPage"),
  "CertificateIssuersPage",
);
const AuditPage = lazyRouteComponent(
  () => import("./pages/AuditPage"),
  "AuditPage",
);
const BuilderSettingsPage = lazyRouteComponent(
  () => import("./pages/BuilderSettingsPage"),
  "BuilderSettingsPage",
);

function RoutePendingPage() {
  return (
    <Page narrow className="min-h-[calc(100vh-68px)] place-content-center">
      <section
        className="grid gap-4 rounded-[16px] border border-line bg-surface p-6"
        role="status"
        aria-label="Loading page"
        aria-busy="true"
      >
        <strong className="text-sm text-ink">Loading page</strong>
        <Skeleton lines={5} />
      </section>
    </Page>
  );
}

export function RootComponent() {
  const queryClient = useQueryClient();
  const [invitationToken, setInvitationToken] = useState(() =>
    invitationTokenFromHash(window.location.hash),
  );
  useLayoutEffect(() => {
    clearInvitationFragment();
    const handleHashChange = (event: HashChangeEvent) => {
      setInvitationToken(invitationTokenFromHash(new URL(event.newURL).hash));
    };
    window.addEventListener("hashchange", handleHashChange);
    return () => window.removeEventListener("hashchange", handleHashChange);
  }, []);
  const me = useQuery<Principal | null>({
    queryKey: ["me"],
    queryFn: api.me,
    retry: false,
    // A successful logout sets null; only an explicit session retry or a new
    // login should replace that known state during this page's lifetime.
    staleTime: (query) => (query.state.data === null ? Infinity : 60_000),
    refetchOnMount: (query) => query.state.data !== null,
    refetchOnReconnect: (query) => query.state.data !== null,
    refetchOnWindowFocus: (query) => query.state.data != null,
  });
  useLayoutEffect(() => {
    if (me.data === null) {
      // Tenant observers are now unmounted, so clearing their cache cannot
      // make a still-mounted page recreate a revoked-session request. Keep
      // the public metadata query used by the signed-out screen.
      queryClient.removeQueries({
        predicate: (query) =>
          query.queryKey[0] !== "me" && query.queryKey[0] !== "meta",
      });
    }
  }, [me.data, queryClient]);
  const finishAuthentication = () => {
    const destination = postAuthenticationDestination(window.location.pathname);
    if (destination) {
      void router.navigate({ to: destination, replace: true });
    }
  };
  if (invitationToken)
    return (
      <AuthScreen
        invitationToken={invitationToken}
        onAuthenticated={finishAuthentication}
        onInvitationAccepted={() => setInvitationToken(null)}
        onInvitationDismissed={() => setInvitationToken(null)}
      />
    );
  if (me.isPending)
    return (
      <div className="flex min-h-screen items-center justify-center gap-3 text-white bg-[var(--dark)] [&_strong]:text-sm">
        <span className="relative inline-flex size-[27px] items-end justify-center gap-0.5 rounded-lg border border-line-strong bg-mint-soft p-1.5 [&>span]:w-[3px] [&>span]:rounded-sm [&>span]:bg-mint [&>span:nth-child(1)]:h-[7px] [&>span:nth-child(2)]:h-[14px] [&>span:nth-child(3)]:h-[10px]">
          <span />
          <span />
          <span />
        </span>
        <strong>Kuberploy</strong>
        <span className="inline-block size-3.5 animate-spin rounded-full border-2 border-current border-r-transparent ml-2.5 text-mint" />
      </div>
    );
  if (me.error || me.data === null)
    return (
      <AuthScreen
        connectionError={isUnauthorized(me.error) ? undefined : me.error}
        onAuthenticated={finishAuthentication}
      />
    );
  return <AppShell user={me.data} />;
}

const rootRoute = createRootRoute({
  component: RootComponent,
  errorComponent: RouteErrorPage,
  notFoundComponent: NotFoundPage,
});
const dashboardRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: DashboardPage,
});
const projectsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects",
  component: ProjectsPage,
});
const projectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId",
  validateSearch: (
    search: Record<string, unknown>,
  ): { gitEnvironmentId?: string } => ({
    gitEnvironmentId:
      typeof search.gitEnvironmentId === "string" &&
      search.gitEnvironmentId.trim()
        ? search.gitEnvironmentId.trim()
        : undefined,
  }),
  component: ProjectPage,
});
const environmentRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId/environments/$environmentId",
  component: EnvironmentPage,
});
const addAppRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId/environments/$environmentId/apps/new",
  component: AddAppPage,
});
const environmentAppRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$projectId/environments/$environmentId/apps/$applicationId",
  component: ApplicationOverviewPage,
});
const variableSetsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/environments/$environmentId/variables",
  component: VariableSetsPage,
});
const teamsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/teams",
  component: TeamsPage,
});
type DeploySearch = {
  projectId?: string;
  environmentId?: string;
  applicationId?: string;
};
export function requireScopedDeploySearch(search: DeploySearch) {
  if (!search.projectId || !search.environmentId || !search.applicationId) {
    throw redirect({ to: "/projects", replace: true });
  }
}
const deployRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/deploy",
  validateSearch: (search: Record<string, unknown>): DeploySearch => ({
    projectId:
      typeof search.projectId === "string" && search.projectId
        ? search.projectId
        : undefined,
    environmentId:
      typeof search.environmentId === "string" && search.environmentId
        ? search.environmentId
        : undefined,
    applicationId:
      typeof search.applicationId === "string" && search.applicationId
        ? search.applicationId
        : undefined,
  }),
  beforeLoad: ({ search }) => requireScopedDeploySearch(search),
  component: NewDeploymentPage,
});
const monitoringRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/monitoring",
  component: MonitoringPage,
});
const auditRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/audit",
  component: AuditPage,
});
const registryRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/registry",
  component: RegistryTargetsPage,
});
const externalDNSRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/external-dns",
  component: ExternalDNSPage,
});
const gitProvidersRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/git",
  component: SourceBuildsPage,
});
const buildDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/builds/$buildId",
  component: BuildDetailPage,
});
const githubSetupCompleteRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/github/setup/complete",
  component: GitHubSetupCompletePage,
});
const applicationRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/applications/$applicationId/deployments/$deploymentId",
  component: ApplicationPage,
});
const applicationOverviewRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/applications/$applicationId",
  component: ApplicationOverviewPage,
});
const operationRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/operations/$operationId",
  component: OperationPage,
});
const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/setup",
  component: SetupPage,
});
const platformReleasesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings/releases",
  component: UpgradePage,
});
const platformArgoGitBindingRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings/argo-git",
  component: PlatformArgoGitBindingPage,
});
const middlewareProfilesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings/middleware-profiles",
  component: MiddlewareProfilesPage,
});
const certificateIssuersRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings/certificate-issuers",
  component: CertificateIssuersPage,
});
const builderSettingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings/builders",
  component: BuilderSettingsPage,
});
export function LegacySettingsRedirect() {
  return <Navigate to="/setup" replace />;
}
export function LegacyBuildsRedirect() {
  return <Navigate to="/git" replace />;
}
const legacySettingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings",
  component: LegacySettingsRedirect,
});
const legacyIntegrationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings/integrations",
  component: LegacySettingsRedirect,
});
const legacyBuildsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/builds",
  component: LegacyBuildsRedirect,
});

const routeTree = rootRoute.addChildren([
  dashboardRoute,
  projectsRoute,
  projectRoute,
  environmentRoute,
  addAppRoute,
  environmentAppRoute,
  variableSetsRoute,
  teamsRoute,
  deployRoute,
  monitoringRoute,
  auditRoute,
  registryRoute,
  externalDNSRoute,
  gitProvidersRoute,
  legacyBuildsRoute,
  buildDetailRoute,
  githubSetupCompleteRoute,
  applicationOverviewRoute,
  applicationRoute,
  operationRoute,
  setupRoute,
  platformReleasesRoute,
  platformArgoGitBindingRoute,
  middlewareProfilesRoute,
  certificateIssuersRoute,
  builderSettingsRoute,
  legacySettingsRoute,
  legacyIntegrationsRoute,
]);

export const router = createRouter({
  routeTree,
  defaultPreload: "intent",
  defaultPendingComponent: RoutePendingPage,
  defaultPendingMs: 150,
  defaultPendingMinMs: 250,
  scrollRestoration: true,
});

export function postAuthenticationDestination(
  pathname: string,
): "/" | "/git" | null {
  if (/^\/builds\/?$/.test(pathname)) return "/git";
  try {
    const hasPageMatch = router
      .matchRoutes(pathname, {})
      .some((match) => match.routeId !== "__root__");
    return hasPageMatch ? null : "/";
  } catch {
    return "/";
  }
}

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
