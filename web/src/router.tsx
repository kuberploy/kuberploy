import {
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { useLayoutEffect, useState } from "react";
import { api, isUnauthorized } from "./api/client";
import { AppShell } from "./components/AppShell";
import { AuthScreen } from "./components/AuthScreen";
import { DashboardPage } from "./pages/DashboardPage";
import { ProjectsPage } from "./pages/ProjectsPage";
import { ProjectPage } from "./pages/ProjectPage";
import { EnvironmentPage } from "./pages/EnvironmentPage";
import { AddAppPage } from "./pages/AddAppPage";
import { NewDeploymentPage } from "./pages/NewDeploymentPage";
import { ApplicationPage } from "./pages/ApplicationPage";
import { ApplicationOverviewPage } from "./pages/ApplicationOverviewPage";
import { OperationPage } from "./pages/OperationPage";
import { SetupPage } from "./pages/SetupPage";
import { UpgradePage } from "./pages/UpgradePage";
import { TeamsPage } from "./pages/TeamsPage";
import { MonitoringPage } from "./pages/MonitoringPage";
import { RegistryTargetsPage } from "./pages/RegistryTargetsPage";
import { ExternalDNSPage } from "./pages/ExternalDNSPage";
import { SourceBuildsPage } from "./pages/SourceBuildsPage";
import { BuildDetailPage } from "./pages/BuildDetailPage";
import { GitHubSetupCompletePage } from "./pages/GitHubSetupCompletePage";
import { PlatformArgoGitBindingPage } from "./pages/PlatformArgoGitBindingPage";
import { MiddlewareProfilesPage } from "./pages/MiddlewareProfilesPage";
import { VariableSetsPage } from "./pages/VariableSetsPage";
import { CertificateIssuersPage } from "./pages/CertificateIssuersPage";
import { AuditPage } from "./pages/AuditPage";
import { BuilderSettingsPage } from "./pages/BuilderSettingsPage";
import {
  clearInvitationFragment,
  invitationTokenFromHash,
} from "./lib/invitationLink";

export function RootComponent() {
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
  const me = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: false,
    staleTime: 60_000,
  });
  if (invitationToken)
    return (
      <AuthScreen
        invitationToken={invitationToken}
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
  if (me.error)
    return (
      <AuthScreen
        connectionError={isUnauthorized(me.error) ? undefined : me.error}
      />
    );
  return <AppShell user={me.data} />;
}

const rootRoute = createRootRoute({ component: RootComponent });
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
const buildsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/builds",
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
  buildsRoute,
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
]);

export const router = createRouter({
  routeTree,
  defaultPreload: "intent",
  scrollRestoration: true,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
