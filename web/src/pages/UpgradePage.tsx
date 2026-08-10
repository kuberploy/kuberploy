import { useMutation, useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { api, errorMessage } from "../api/client";
import type { PlatformRelease } from "../api/types";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  EmptyState,
  PageHeader,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
} from "../components/ui";
import { formatDate, shortId, titleCase } from "../lib/format";
import { hasPlatformUpgradeCapability } from "../lib/upgradeAccess";

export function UpgradePage() {
  const navigate = useNavigate();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
    staleTime: 60_000,
  });
  const canReadReleases = hasPlatformUpgradeCapability(
    capabilities.data?.capabilities ?? [],
    "platform-releases:read",
  );
  const canCreateUpgrade = hasPlatformUpgradeCapability(
    capabilities.data?.capabilities ?? [],
    "platform-upgrades:create",
  );
  const meta = useQuery({ queryKey: ["meta"], queryFn: api.meta });
  const latest = useQuery({
    queryKey: ["platform-release", "latest"],
    queryFn: api.latestPlatformRelease,
    retry: false,
    staleTime: 60_000,
    enabled: canReadReleases,
  });
  const upgrade = useMutation({
    mutationFn: (release: PlatformRelease) =>
      api.startPlatformUpgrade({
        targetVersion: release.version,
        manifestDigest: release.manifestDigest,
      }),
    onSuccess: async (operation) => {
      setConfirmOpen(false);
      await navigate({
        to: "/operations/$operationId",
        params: { operationId: operation.id },
      });
    },
  });

  const currentVersion =
    latest.data?.currentVersion ??
    meta.data?.platformVersion ??
    meta.data?.version;
  const release = latest.data?.release;
  const compatibility = latest.data?.compatibility ?? {
    status: "unknown" as const,
    reasons: [] as string[],
  };
  const canUpgrade =
    canCreateUpgrade &&
    latest.data?.updateAvailable === true &&
    compatibility.status !== "incompatible";

  return (
    <div className="page">
      <PageHeader
        eyebrow="Platform settings"
        title="Upgrade Kuberploy"
        description="Discover a signed public release, inspect its immutable manifest and compatibility decision, then start one namespaced control-plane upgrade operation."
        actions={
          canReadReleases ? (
            <Button
              variant="secondary"
              busy={latest.isFetching}
              onClick={() => void latest.refetch()}
            >
              <Icon name="refresh" /> Check for releases
            </Button>
          ) : undefined
        }
      />

      {capabilities.isPending ? (
        <Card>
          <Skeleton lines={4} />
        </Card>
      ) : !canReadReleases ? (
        <Card>
          <EmptyState
            icon="settings"
            title="Platform administrator access required"
            description="This account does not have the exact platform release-read capability. No release lookup was performed."
            compact
          />
        </Card>
      ) : latest.isPending ? (
        <Card>
          <Skeleton lines={10} />
        </Card>
      ) : latest.error || !release ? (
        <Card className="upgrade-unavailable">
          <EmptyState
            icon="refresh"
            title="No public release is available right now"
            description="The current control plane keeps running unchanged. Release discovery may be disabled, the first public release may not exist yet, or GitHub may be temporarily unavailable."
            action={
              <Button variant="secondary" onClick={() => void latest.refetch()}>
                <Icon name="refresh" /> Try again
              </Button>
            }
            compact
          />
          <div className="release-error-detail" role="status">
            <span>Current version</span>
            <code>{currentVersion ?? "Not reported"}</code>
            <span>Last check error</span>
            <p>
              {latest.error
                ? errorMessage(latest.error)
                : "No release returned."}
            </p>
          </div>
        </Card>
      ) : (
        <>
          <section
            className="version-comparison"
            aria-label="Version comparison"
          >
            <Card className="version-card version-card--current">
              <span className="eyebrow">Installed</span>
              <div>
                <small>Current version</small>
                <strong>{latest.data.currentVersion}</strong>
              </div>
              <StatusPill value="active" label="Running" />
            </Card>
            <span className="version-comparison__arrow">
              <Icon name="arrow" />
            </span>
            <Card className="version-card version-card--target">
              <span className="eyebrow">Public release</span>
              <div>
                <small>Latest version</small>
                <strong>{release.version}</strong>
              </div>
              <StatusPill
                value={latest.data.updateAvailable ? "available" : "healthy"}
                label={
                  latest.data.updateAvailable
                    ? "Update available"
                    : "Up to date"
                }
              />
            </Card>
          </section>

          <div className="upgrade-grid">
            <Card className="upgrade-decision-card">
              <div className="upgrade-decision-card__head">
                <span
                  className={`decision-mark decision-mark--${compatibility.status}`}
                >
                  <Icon
                    name={
                      compatibility.status === "compatible"
                        ? "check"
                        : compatibility.status === "incompatible"
                          ? "close"
                          : "refresh"
                    }
                  />
                </span>
                <div>
                  <span className="eyebrow">Compatibility decision</span>
                  <h2>{titleCase(compatibility.status)}</h2>
                </div>
                <StatusPill value={compatibility.status} />
              </div>
              <p className="decision-summary">
                {compatibility.status === "compatible"
                  ? "The published source-version and schema ranges accept this installation. The upgrader rechecks Kubernetes and every manifest digest before mutation."
                  : compatibility.status === "unknown"
                    ? "Runtime Kubernetes validation is intentionally deferred to the isolated upgrader Job. Starting the operation does not bypass that fail-closed check."
                    : "This release cannot be applied to the current installation. No upgrade operation will be created."}
              </p>
              {compatibility.reasons.length ? (
                <ul className="decision-reasons">
                  {compatibility.reasons.map((reason) => (
                    <li key={reason}>{reason}</li>
                  ))}
                </ul>
              ) : null}
              {release.breakingChanges ? (
                <div className="notice notice--warning">
                  <div>
                    <strong>Breaking changes declared</strong>
                    <p>Read the complete release notes before confirmation.</p>
                  </div>
                </div>
              ) : null}
            </Card>

            <Card className="release-identity-card">
              <div className="card__header card__header--inside">
                <div>
                  <span className="eyebrow">Immutable release identity</span>
                  <h2>{release.tag}</h2>
                </div>
                <a
                  className="text-link"
                  href={release.notesUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  Release notes <Icon name="external" />
                </a>
              </div>
              <dl className="digest-list">
                <div>
                  <dt>Manifest digest</dt>
                  <dd title={release.manifestDigest}>
                    <code>{release.manifestDigest}</code>
                  </dd>
                </div>
                <div>
                  <dt>Chart</dt>
                  <dd title={release.chart.ociReference}>
                    <code>{release.chart.ociReference}</code>
                  </dd>
                </div>
                <div>
                  <dt>Chart digest</dt>
                  <dd title={release.chart.ociDigest}>
                    <code>{release.chart.ociDigest}</code>
                  </dd>
                </div>
                <div>
                  <dt>Source commit</dt>
                  <dd title={release.manifest.source.commit}>
                    <code>{shortId(release.manifest.source.commit, 12)}</code>
                  </dd>
                </div>
                <div>
                  <dt>Published</dt>
                  <dd>{formatDate(release.publishedAt)}</dd>
                </div>
                <div>
                  <dt>Last checked</dt>
                  <dd>{formatDate(latest.data.lastCheckedAt)}</dd>
                </div>
              </dl>
            </Card>
          </div>

          <Card className="manifest-card">
            <div className="card__header card__header--inside">
              <div>
                <span className="eyebrow">release-manifest.json</span>
                <h2>Verified upgrade envelope</h2>
              </div>
              <PlaceholderBadge>
                Schema {release.manifest.schemaVersion}
              </PlaceholderBadge>
            </div>
            <div className="manifest-grid">
              <ManifestFact
                label="Kubernetes"
                value={release.manifest.compatibility.kubernetes.constraint}
              />
              <ManifestFact
                label="Upgrade window"
                value={release.manifest.compatibility.supportedUpgradeFrom}
              />
              <ManifestFact
                label="Database schema"
                value={`${release.manifest.compatibility.database.minimumUpgradeableSchema} → ${release.manifest.compatibility.database.currentSchema}`}
              />
            </div>
            <details className="manifest-details">
              <summary>Show immutable control-plane images</summary>
              <dl>
                {release.manifest.artifacts.images.map((image) => (
                  <div key={image.component}>
                    <dt>{image.component}</dt>
                    <dd>
                      <code>
                        {image.reference}@{image.digest}
                      </code>
                    </dd>
                  </div>
                ))}
              </dl>
            </details>
          </Card>

          <Card className="release-notes-card">
            <div>
              <span className="eyebrow">Summary</span>
              <h2>Release notes</h2>
              <p>{release.manifest.release.summary}</p>
            </div>
            <a
              className="button button--secondary"
              href={release.notesUrl}
              target="_blank"
              rel="noreferrer"
            >
              Read on GitHub <Icon name="external" />
            </a>
          </Card>

          <div className="upgrade-action-bar">
            <div>
              <Icon name="check" />
              <span>
                <strong>Tenant workloads are outside this Helm release.</strong>
                <small>
                  The upgrade replaces control-plane Pods only; Argo-managed
                  applications are not rendered or restarted.
                </small>
              </span>
            </div>
            <Button disabled={!canUpgrade} onClick={() => setConfirmOpen(true)}>
              <Icon name="deploy" />
              {latest.data.updateAvailable
                ? `Upgrade to ${release.version}`
                : "Already up to date"}
            </Button>
          </div>

          {upgrade.error ? (
            <div className="notice notice--error" role="alert">
              <div>
                <strong>Upgrade was not accepted</strong>
                <p>{errorMessage(upgrade.error)}</p>
              </div>
            </div>
          ) : null}
        </>
      )}

      {confirmOpen && release ? (
        <UpgradeConfirmation
          release={release}
          compatibilityStatus={compatibility.status}
          busy={upgrade.isPending}
          error={upgrade.error}
          onCancel={() => setConfirmOpen(false)}
          onConfirm={() => upgrade.mutate(release)}
        />
      ) : null}
    </div>
  );
}

function ManifestFact({ label, value }: { label: string; value: string }) {
  return (
    <div className="manifest-range">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

export function UpgradeConfirmation({
  release,
  compatibilityStatus,
  busy,
  error,
  onCancel,
  onConfirm,
}: {
  release: PlatformRelease;
  compatibilityStatus: "compatible" | "incompatible" | "unknown";
  busy: boolean;
  error: unknown;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const [confirmed, setConfirmed] = useState(false);
  return (
    <div className="confirmation-backdrop">
      <section
        className="confirmation-dialog"
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="upgrade-confirmation-title"
      >
        <span className="confirmation-dialog__icon">
          <Icon name="deploy" />
        </span>
        <span className="eyebrow">Explicit confirmation</span>
        <h2 id="upgrade-confirmation-title">
          Upgrade the control plane to {release.version}?
        </h2>
        <p>
          Kuberploy will submit the exact version and manifest digest below. The
          upgrader Job re-fetches the immutable manifest and fails closed before
          Helm mutation if any digest or compatibility check differs.
        </p>
        <dl className="confirmation-identity">
          <div>
            <dt>Target</dt>
            <dd>{release.version}</dd>
          </div>
          <div>
            <dt>Manifest</dt>
            <dd>
              <code>{release.manifestDigest}</code>
            </dd>
          </div>
          <div>
            <dt>Decision</dt>
            <dd>
              <StatusPill value={compatibilityStatus} />
            </dd>
          </div>
        </dl>
        <label className="confirmation-check">
          <input
            type="checkbox"
            checked={confirmed}
            onChange={(event) => setConfirmed(event.target.checked)}
          />
          <span>
            I reviewed the target version, immutable manifest digest, release
            notes, and compatibility decision.
          </span>
        </label>
        {error ? (
          <div className="notice notice--error" role="alert">
            {errorMessage(error)}
          </div>
        ) : null}
        <div className="confirmation-dialog__actions">
          <Button variant="ghost" disabled={busy} onClick={onCancel}>
            Cancel
          </Button>
          <Button disabled={!confirmed} busy={busy} onClick={onConfirm}>
            Start verified upgrade <Icon name="arrow" />
          </Button>
        </div>
      </section>
    </div>
  );
}
