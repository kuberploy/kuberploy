import { useQuery } from "@tanstack/react-query";
import { api, errorMessage } from "../api/client";
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
  const meta = useQuery({ queryKey: ["meta"], queryFn: api.meta });
  const latest = useQuery({
    queryKey: ["platform-release", "latest"],
    queryFn: api.latestPlatformRelease,
    retry: false,
    staleTime: 60_000,
    enabled: canReadReleases,
  });
  const history = useQuery({
    queryKey: ["platform-upgrades"],
    queryFn: api.platformUpgrades,
    retry: false,
    enabled: canReadReleases,
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
    latest.data?.updateAvailable === true &&
    compatibility.status !== "incompatible";

  return (
    <div className="page">
      <PageHeader
        eyebrow="Platform settings"
        title="Kuberploy releases"
        description="Inspect the signed public release and compatibility envelope. Upgrade or rollback the installer Helm release with operator credentials; Kuberploy never mutates an Argo-owned child release."
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
                  ? "The published source-version and schema ranges accept this installation. The installer chart rechecks every enabled Argo Application after Helm upgrade or rollback."
                  : compatibility.status === "unknown"
                    ? "The operator-owned installer validates Kubernetes and every enabled Argo Application during Helm upgrade or rollback."
                    : "This release cannot be applied to the current installation. Helm must not target it."}
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
                <strong>Upgrade the installer release, not its child.</strong>
                <small>
                  Run Helm against the installer release using the exact chart
                  version above. Its lifecycle hooks require every enabled Argo
                  Application to reach the requested revision, Synced and
                  Healthy.
                </small>
              </span>
            </div>
            <StatusPill
              value={canUpgrade ? "available" : "healthy"}
              label={
                latest.data.updateAvailable
                  ? "Operator Helm action required"
                  : "Up to date"
              }
            />
          </div>
        </>
      )}

      {canReadReleases ? (
        <Card className="manifest-card">
          <div className="card__header card__header--inside">
            <div>
              <span className="eyebrow">Pre-stable compatibility</span>
              <h2>Legacy in-app operation history</h2>
              <p>
                Read-only records created by pre-stable builds remain visible
                for audit. Current upgrades and rollbacks are recorded by the
                operator-owned installer Helm release, not this table.
              </p>
            </div>
          </div>
          {history.isPending ? (
            <Skeleton lines={4} />
          ) : history.error ? (
            <div className="notice notice--error" role="alert">
              {errorMessage(history.error)}
            </div>
          ) : history.data?.items.length ? (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Action</th>
                    <th>Version</th>
                    <th>State</th>
                    <th>Created</th>
                  </tr>
                </thead>
                <tbody>
                  {history.data.items.map((item) => (
                    <tr key={item.id}>
                      <td>{titleCase(item.action)}</td>
                      <td>
                        <code>{item.version}</code>
                        {item.helmRevision ? (
                          <small> revision {item.helmRevision}</small>
                        ) : null}
                      </td>
                      <td>
                        <StatusPill value={item.state} />
                      </td>
                      <td>{formatDate(item.createdAt)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <EmptyState
              icon="refresh"
              title="No legacy platform operations"
              description="Use Helm history for current installer upgrade and rollback records."
              compact
            />
          )}
        </Card>
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
