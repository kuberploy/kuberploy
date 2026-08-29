import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { GitHubInstallationsPanel } from "../components/GitHubInstallationsPanel";
import {
  Card,
  EmptyState,
  ErrorPanel,
  Page,
  PageHeader,
  Skeleton,
} from "../components/ui";

export function SourceBuildsPage() {
  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const githubSetupEnabled =
    capabilities.data?.features?.githubAppSetup === true;
  const humanSession = me.data?.authentication?.kind === "session";
  const canSetupGitHub = effectiveCapabilities.some(
    (capability) =>
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      capability.actions?.includes("github-installations:setup"),
  );
  const loadError = me.error ?? capabilities.error;

  return (
    <Page>
      <PageHeader
        eyebrow="Settings"
        title="Git providers"
        description="Connect and manage provider accounts. Repository, branch, build, and deployment settings belong to each App."
      />

      {loadError ? (
        <ErrorPanel
          error={loadError}
          onRetry={() =>
            void Promise.all([me.refetch(), capabilities.refetch()])
          }
        />
      ) : me.isPending || capabilities.isPending ? (
        <Card>
          <Skeleton lines={6} />
        </Card>
      ) : (
        <GitHubInstallationsPanel
          featureEnabled={githubSetupEnabled}
          humanSession={humanSession}
          canSetup={canSetupGitHub}
        />
      )}

      <Card>
        <EmptyState
          icon="git"
          title="More Git providers"
          description="GitLab, Bitbucket, and Gitea provider connections can be added here later without changing App-owned source settings. Git SSH already works from each App with an App or Project key."
          compact
        />
      </Card>
    </Page>
  );
}
