import { useQuery } from "@tanstack/react-query";
import { ApiError, api, errorMessage } from "../api/client";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  Eyebrow,
  Notice,
  Page,
  PageHeader,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
  buttonVariants,
} from "../components/ui";
import { formatDate, shortId, titleCase } from "../lib/format";
import { hasPlatformReleaseCapability } from "../lib/releaseAccess";
import { cn } from "@/lib/utils";

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
    <Page>
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
        <Card className="overflow-hidden !p-0">
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
          <div
            className="grid grid-cols-[auto_minmax(0,_1fr)] gap-y-2 gap-x-4 py-4 px-5 border-t border-t-line text-ink-soft bg-surface-soft text-meta [&>span]:text-ink-faint [&_p]:m-0"
            role="status"
          >
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
            className="grid grid-cols-[minmax(0,_1fr)_45px_minmax(0,_1fr)] items-center gap-3 mb-4 to-580:grid-cols-[1fr]"
            aria-label="Version comparison"
          >
            <Card className="grid min-h-[126px] grid-cols-[1fr_auto] items-end gap-3 py-5 px-5 [&_div]:flex [&_div]:flex-col [&_div]:gap-1 [&_small]:text-ink-faint [&_small]:text-xs [&_small]:uppercase [&_strong]:text-ink [&_strong]:font-mono [&_strong]:text-[24px] [&_strong]:font-semibold [&_strong]:tracking-[-0.02em] border-l border-l-mint">
              <Eyebrow>Installed</Eyebrow>
              <div>
                <small>Current version</small>
                <strong>{latest.data.currentVersion}</strong>
              </div>
              <StatusPill value="active" label="Running" />
            </Card>
            <span className="grid w-[34px] h-[34px] place-items-center justify-self-center border border-line rounded-full text-mint-dark bg-surface [&_svg]:w-[15px] to-580:transform-[rotate(90deg)]">
              <Icon name="arrow" />
            </span>
            <Card className="grid min-h-[126px] grid-cols-[1fr_auto] items-end gap-3 py-5 px-5 [&_div]:flex [&_div]:flex-col [&_div]:gap-1 [&_small]:text-ink-faint [&_small]:text-xs [&_small]:uppercase [&_strong]:text-ink [&_strong]:font-mono [&_strong]:text-[24px] [&_strong]:font-semibold [&_strong]:tracking-[-0.02em] border-mint bg-mint-soft">
              <Eyebrow>Public release</Eyebrow>
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

          <div className="grid grid-cols-[minmax(0,_0.9fr)_minmax(0,_1.1fr)] gap-4 mb-4 to-1120:grid-cols-[1fr]">
            <Card className="p-6">
              <div className="grid grid-cols-[42px_minmax(0,_1fr)_auto] items-center gap-3 [&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-0 [&_h2]:text-section [&_h2]:tracking-[-0.02em] to-580:grid-cols-[42px_1fr]">
                <span
                  // The three tones are the app's tone tokens, so this follows
                  // the theme instead of carrying a second hard-coded dark set.
                  className={cn(
                    "grid size-10 place-items-center rounded-[11px] border [&_svg]:w-[18px]",
                    compatibility.status === "compatible"
                      ? "border-tone-good-line bg-tone-good-surface text-tone-good"
                      : compatibility.status === "incompatible"
                        ? "border-tone-bad-line bg-tone-bad-surface text-tone-bad"
                        : "border-tone-warn-line bg-tone-warn-surface text-tone-warn",
                  )}
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
                  <Eyebrow>Compatibility decision</Eyebrow>
                  <h2>{titleCase(compatibility.status)}</h2>
                </div>
                <StatusPill value={compatibility.status} />
              </div>
              <p className="mt-4 mx-0 mb-0 text-ink-soft text-meta leading-[1.65]">
                {compatibility.status === "compatible"
                  ? "The published source-version and schema ranges accept this installation. The installer chart rechecks every enabled Argo Application after Helm upgrade or rollback."
                  : compatibility.status === "unknown"
                    ? "The operator-owned installer validates Kubernetes and every enabled Argo Application during Helm upgrade or rollback."
                    : "This release cannot be applied to the current installation. Helm must not target it."}
              </p>
              {compatibility.reasons.length ? (
                <ul className="mt-4 mx-0 mb-0 pt-3 pr-3 pb-3 pl-7 border border-line rounded-lg text-ink-soft bg-surface-soft text-meta leading-[1.55]">
                  {compatibility.reasons.map((reason) => (
                    <li key={reason}>{reason}</li>
                  ))}
                </ul>
              ) : null}
              {release.breakingChanges ? (
                <Notice tone="warning">
                  <div>
                    <strong>Breaking changes declared</strong>
                    <p>Read the complete release notes before confirmation.</p>
                  </div>
                </Notice>
              ) : null}
            </Card>

            <Card className="p-6">
              <CardHeader>
                <div>
                  <Eyebrow>Immutable release identity</Eyebrow>
                  <h2>{release.tag}</h2>
                </div>
                <a
                  className="inline-flex items-center gap-1.5 py-0.5 px-0 border-0 rounded-sm text-mint-dark bg-transparent cursor-pointer text-meta font-medium whitespace-nowrap hover:underline hover:underline-offset-[3px] focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[3px] [&_svg]:w-3.5 [&_svg]:h-3.5 pointer-coarse:inline-flex pointer-coarse:min-h-8 pointer-coarse:items-center"
                  href={release.notesUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  Release notes <Icon name="external" />
                </a>
              </CardHeader>
              <dl className="m-0 [&>div]:grid [&>div]:grid-cols-[104px_minmax(0,_1fr)] [&>div]:gap-4 [&>div]:py-2 [&>div]:px-0 [&>div]:border-t [&>div]:border-t-line [&_dt]:text-ink-faint [&_dt]:text-xs [&_dd]:min-w-0 [&_dd]:m-0 [&_dd]:overflow-hidden [&_dd]:text-ink-soft [&_dd]:text-meta [&_dd]:text-ellipsis [&_dd]:whitespace-nowrap [&_code]:text-xs">
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

          <Card className="p-6 mb-4">
            <CardHeader>
              <div>
                <Eyebrow>release-manifest.json</Eyebrow>
                <h2>Verified upgrade envelope</h2>
              </div>
              <PlaceholderBadge>
                Schema {release.manifest.schemaVersion}
              </PlaceholderBadge>
            </CardHeader>
            <div className="grid grid-cols-[repeat(3,_minmax(0,_1fr))] gap-3 to-580:grid-cols-[1fr]">
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
            <details className="mt-3 border border-line rounded-lg [&_summary]:py-3 [&_summary]:px-3 [&_summary]:cursor-pointer [&_summary]:text-ink-soft [&_summary]:text-meta [&_summary]:font-semibold [&_dl]:m-0 [&_dl]:pt-0 [&_dl]:px-3 [&_dl]:pb-3 [&_dl_>_div]:grid [&_dl_>_div]:grid-cols-[70px_minmax(0,_1fr)] [&_dl_>_div]:gap-3 [&_dl_>_div]:py-2 [&_dl_>_div]:px-0 [&_dl_>_div]:border-t [&_dl_>_div]:border-t-line [&_dt]:text-ink-faint [&_dt]:text-xs [&_dt]:capitalize [&_dd]:min-w-0 [&_dd]:m-0 [&_dd]:overflow-hidden [&_dd]:text-ellipsis [&_dd]:whitespace-nowrap [&_code]:text-xs">
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

          <Card className="p-6 flex items-center justify-between gap-6 mb-4 [&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-1.5 [&_h2]:text-lead [&_p]:max-w-[800px] [&_p]:m-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.6] [&_p]:whitespace-pre-line to-580:items-stretch to-580:flex-col">
            <div>
              <Eyebrow>Summary</Eyebrow>
              <h2>Release notes</h2>
              <p>{release.manifest.release.summary}</p>
            </div>
            <a
              className={buttonVariants({ variant: "secondary" })}
              href={release.notesUrl}
              target="_blank"
              rel="noreferrer"
            >
              Read on GitHub <Icon name="external" />
            </a>
          </Card>

          <div className="flex items-center justify-between gap-5 py-4 px-5 border border-mint-line rounded-[11px] bg-mint-soft shadow-panel [&>div]:flex [&>div]:items-center [&>div]:gap-3 [&>div_>_svg]:w-[18px] [&>div_>_svg]:text-mint-dark [&_span]:flex [&_span]:flex-col [&_strong]:text-meta [&_small]:mt-1 [&_small]:text-ink-soft [&_small]:text-xs to-580:items-stretch to-580:flex-col to-580:[&>[data-slot='button']]:w-full">
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
            <Card className="p-6 mb-4">
              <CardHeader>
                <div>
                  <Eyebrow>Cluster administrator</Eyebrow>
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
              </CardHeader>
              <pre className="overflow-auto mt-3 mx-0 mb-0 p-4 border border-line rounded-lg text-ink bg-surface-soft text-xs leading-[1.6]">
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
    </Page>
  );
}

function ManifestFact({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex min-h-[70px] flex-col justify-center py-3 px-4 border border-line rounded-lg bg-surface-soft [&_span]:text-ink-faint [&_span]:text-xs [&_strong]:flex [&_strong]:items-center [&_strong]:gap-1.5 [&_strong]:mt-1.5 [&_strong]:font-mono [&_strong]:text-meta [&_svg]:w-3 [&_svg]:text-mint-dark">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}
