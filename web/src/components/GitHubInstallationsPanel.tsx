import { useQuery } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { api } from "../api/client";
import { Icon } from "./Icon";
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Notice,
  Skeleton,
  StatusPill,
} from "./ui";

export function GitHubInstallationsPanel({
  featureEnabled,
  humanSession,
  canSetup,
  navigate = (destination) => window.location.assign(destination),
}: {
  featureEnabled: boolean;
  humanSession: boolean;
  canSetup: boolean;
  navigate?: (destination: string) => void;
}) {
  const setupKeys = useRef(new Map<string, string>());
  const setupTarget = useRef<number | undefined>(undefined);
  const [setupPending, setSetupPending] = useState(false);
  const [setupError, setSetupError] = useState<unknown>();
  const installations = useQuery({
    queryKey: ["github-installations"],
    queryFn: api.githubInstallations,
    enabled: featureEnabled,
    retry: false,
  });

  const beginSetup = async (existingInstallationId?: number) => {
    if (!humanSession || !canSetup || setupPending) return;
    setupTarget.current = existingInstallationId;
    const targetKey = existingInstallationId?.toString() ?? "new";
    if (!setupKeys.current.has(targetKey)) {
      setupKeys.current.set(targetKey, crypto.randomUUID());
    }
    setSetupPending(true);
    setSetupError(undefined);
    try {
      // The sensitive provider state exists only inside this local promise and
      // the validated destination. It never enters React Query or web storage.
      const destination = await api.beginGitHubSetup(
        { returnKey: "source-builds", existingInstallationId },
        setupKeys.current.get(targetKey)!,
      );
      navigate(destination);
    } catch (error) {
      setSetupError(error);
      setSetupPending(false);
    }
  };

  return (
    <Card className="mb-5">
      <CardHeader>
        <div>
          <Eyebrow>Source access</Eyebrow>
          <h2>GitHub App installations</h2>
          <p>
            Link repositories through the verified two-stage GitHub flow.
            Kuberploy never exposes an App key or repository token to this UI.
          </p>
        </div>
        {featureEnabled && humanSession && canSetup ? (
          <Button onClick={() => void beginSetup()} busy={setupPending}>
            <Icon name="git" /> Install GitHub App
          </Button>
        ) : null}
      </CardHeader>

      {!featureEnabled ? (
        <EmptyState
          icon="git"
          title="GitHub App setup is not enabled"
          description="An administrator must configure and verify the GitHub App integration before installation can begin."
          compact
        />
      ) : installations.isPending ? (
        <Skeleton lines={4} />
      ) : installations.error ? (
        <ErrorPanel
          error={installations.error}
          onRetry={() => void installations.refetch()}
        />
      ) : installations.data?.items.length ? (
        <div className="overflow-hidden border border-line rounded-[11px]">
          {installations.data.items.map((installation) => (
            <article
              className="grid grid-cols-[38px_minmax(160px,_1fr)_minmax(120px,_0.6fr)_auto] items-center gap-4 min-h-[72px] py-3 px-4 border-b border-b-line last:border-b-0 [&>div]:grid [&>div]:min-w-0 [&>div]:gap-1 [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_small]:text-ink-faint [&_small]:text-xs to-760:grid-cols-[1fr] to-760:justify-items-start"
              key={installation.id}
            >
              <span className="grid w-[34px] h-[34px] place-items-center rounded-[9px] text-mint-dark bg-surface-soft [&_svg]:w-[17px]">
                <Icon name="git" />
              </span>
              <div>
                <strong>{installation.accountLogin}</strong>
                <small>
                  {installation.accountType} · GitHub installation #
                  {installation.githubInstallationId}
                </small>
              </div>
              <div>
                <strong>{installation.repositoryCount}</strong>
                <small>{installation.repositorySelection} repositories</small>
              </div>
              <StatusPill
                value={installation.visibility}
                label={
                  installation.visibility === "team" ? "Team shared" : "Private"
                }
              />
              {humanSession && canSetup ? (
                <Button
                  variant="secondary"
                  onClick={() =>
                    void beginSetup(installation.githubInstallationId)
                  }
                  busy={
                    setupPending &&
                    setupTarget.current === installation.githubInstallationId
                  }
                >
                  Verify link
                </Button>
              ) : null}
            </article>
          ))}
        </div>
      ) : (
        <EmptyState
          icon="git"
          title="No linked GitHub installation"
          description="Install the GitHub App, authorize the exact account, and Kuberploy will import only verified repository metadata."
          compact
        />
      )}

      {!humanSession ? (
        <Notice tone="warning">
          <div>
            <strong>Human session required</strong>
            <p>
              Service-account sessions can read only their authorized build
              resources. GitHub installation setup is intentionally hidden.
            </p>
          </div>
        </Notice>
      ) : null}
      {humanSession && featureEnabled && !canSetup ? (
        <Notice tone="warning">
          <div>
            <strong>Platform administrator required</strong>
            <p>
              You can use installations already shared with you, but only a
              platform administrator can create or reverify a private link.
            </p>
          </div>
        </Notice>
      ) : null}
      {setupError ? (
        <ErrorPanel
          title="Could not begin GitHub setup"
          error={setupError}
          onRetry={() => void beginSetup(setupTarget.current)}
        />
      ) : null}
    </Card>
  );
}
