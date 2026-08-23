import { useQuery } from "@tanstack/react-query";
import { ApiError, api, errorMessage } from "../api/client";
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
import { hasPlatformReleaseCapability } from "../lib/releaseAccess";

// Installer health hooks can run for 60 minutes at the supported maximum.
// Keep the operator client alive beyond that server-side deadline.
const operatorHelmTimeout = "65m";

export function UpgradePage() {
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
    staleTime: 60_000,
  });
  const canReadReleases = hasPlatformReleaseCapability(
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
  const currentVersion =
    latest.data?.currentVersion ??
    meta.data?.platformVersion ??
    meta.data?.version;
  const noStableRelease =
    latest.error instanceof ApiError &&
    latest.error.problem?.code === "NoStableRelease";
  const release = latest.data?.release;
  const compatibility = latest.data?.compatibility ?? {
    status: "unknown" as const,
    reasons: [] as string[],
  };
  const canUpgrade =
    latest.data?.updateAvailable === true &&
    compatibility.status !== "incompatible";
  const helmChart = release
    ? `oci://${release.chart.ociReference
        .replace(/^oci:\/\//, "")
        .replace(/:[^/:]+$/, "")}`
    : "";
  const helmCommand =
    release && canUpgrade
      ? `helm upgrade "$RELEASE_NAME" ${helmChart} --version ${release.version} --namespace "$NAMESPACE" --values "$VALUES_FILE" --reset-values --server-side=false --wait --timeout ${operatorHelmTimeout}`
      : "";

  return (
    <div className="page">
      <PageHeader
        eyebrow="Platform settings"
        title="Kuberploy releases"
        description="Inspect the verified immutable public release and compatibility envelope. Upgrade or rollback the installer Helm release with operator credentials; Kuberploy never mutates an Argo-owned child release."
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
            title={
              noStableRelease
                ? "No stable release is published yet"
                : "No public release is available right now"
            }
            description={
              noStableRelease
                ? "This release candidate uses an exact operator-managed chart. The in-product release feed lists stable releases only."
                : "The current control plane keeps running unchanged. Release discovery may be disabled, the first public release may not exist yet, or GitHub may be temporarily unavailable."
            }
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
              {noStableRelease
                ? "Stable release feed is empty."
                : latest.error
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
              <Icon name={canUpgrade ? "check" : "settings"} />
              <span>
                <strong>
                  {canUpgrade
                    ? "Upgrade the installer release, not its child."
                    : "No operator upgrade action is available."}
                </strong>
                <small>
                  {canUpgrade
                    ? "Run Helm against the installer release using the exact chart version above. Its lifecycle hooks require every enabled Argo Application to reach the requested revision, Synced and Healthy."
                    : "The release is equal, older, unknown, or incompatible with this installation."}
                </small>
              </span>
            </div>
            <StatusPill
              value={
                canUpgrade
                  ? "available"
                  : compatibility.status === "incompatible"
                    ? "unavailable"
                    : "healthy"
              }
              label={
                canUpgrade
                  ? "Operator Helm action required"
                  : compatibility.status === "incompatible"
                    ? "Incompatible"
                    : "No action"
              }
            />
          </div>

          {canUpgrade ? (
            <Card className="manifest-card">
              <div className="card__header card__header--inside">
                <div>
                  <span className="eyebrow">Cluster administrator</span>
                  <h2>Helm upgrade command</h2>
                  <p>
                    Set the existing installer release name, namespace, and a
                    reviewed values file for the target chart. Remove any legacy
                    upgrade section, then run this command from an administrator
                    workstation. Do not automatically roll back across a
                    database migration; first verify that the rollback release
                    manifest accepts the current schema.
                  </p>
                </div>
              </div>
              <pre className="code-block">
                <code>{helmCommand}</code>
              </pre>
            </Card>
          ) : (
            <Card>
              <EmptyState
                icon="settings"
                title="No Helm upgrade command available"
                description="Kuberploy only shows a command for a newer compatible release. Equal, older, unknown, or incompatible targets remain read-only."
                compact
              />
            </Card>
          )}
        </>
      )}
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
