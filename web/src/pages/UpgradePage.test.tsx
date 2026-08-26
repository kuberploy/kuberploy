import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api/client";
import type { LatestPlatformRelease } from "../api/types";
import { UpgradePage } from "./UpgradePage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("platform releases page", () => {
  it("shows an operator Helm command and performs no upgrade-history request", async () => {
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["platform-releases:read"],
        },
      ],
      actions: ["platform-releases:read"],
      features: {},
    });
    vi.spyOn(api, "meta").mockResolvedValue({
      version: "0.1.0-rc.365",
      platformVersion: "0.1.0-rc.365",
      bootstrapRequired: false,
    });
    vi.spyOn(api, "latestPlatformRelease").mockResolvedValue(releaseFixture());
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <UpgradePage />
      </QueryClientProvider>,
    );

    expect(await screen.findByText("Helm upgrade command")).toBeVisible();
    expect(
      screen.getByText(
        'helm upgrade "$RELEASE_NAME" oci://ghcr.io/kuberploy/charts/kuberploy-installer --version 0.1.0-rc.365 --namespace "$NAMESPACE" --values "$VALUES_FILE" --reset-values --server-side=false --wait --timeout 65m',
      ),
    ).toBeVisible();
    expect(screen.getByText(/Do not automatically roll back/)).toBeVisible();
    expect(
      screen.queryByText("Legacy in-app operation history"),
    ).not.toBeInTheDocument();
    expect("platformUpgrades" in api).toBe(false);
  });

  it("does not show a Helm command for an equal release", async () => {
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["platform-releases:read"],
        },
      ],
      actions: ["platform-releases:read"],
      features: {},
    });
    vi.spyOn(api, "meta").mockResolvedValue({
      version: "0.1.0-rc.365",
      platformVersion: "0.1.0-rc.365",
      bootstrapRequired: false,
    });
    const fixture = releaseFixture();
    fixture.currentVersion = fixture.release.version;
    fixture.updateAvailable = false;
    vi.spyOn(api, "latestPlatformRelease").mockResolvedValue(fixture);
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <UpgradePage />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByText("No Helm upgrade command available"),
    ).toBeVisible();
    expect(screen.queryByText(/helm upgrade/)).not.toBeInTheDocument();
  });

  it("does not show a Helm command for an incompatible newer release", async () => {
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["platform-releases:read"],
        },
      ],
      actions: ["platform-releases:read"],
      features: {},
    });
    vi.spyOn(api, "meta").mockResolvedValue({
      version: "0.1.0-rc.365",
      platformVersion: "0.1.0-rc.365",
      bootstrapRequired: false,
    });
    const fixture = releaseFixture();
    fixture.compatibility = {
      status: "incompatible",
      reasons: ["Database schema is outside the release window."],
    };
    vi.spyOn(api, "latestPlatformRelease").mockResolvedValue(fixture);
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <UpgradePage />
      </QueryClientProvider>,
    );

    expect((await screen.findAllByText("Incompatible")).length).toBeGreaterThan(
      0,
    );
    expect(screen.getByText("No Helm upgrade command available")).toBeVisible();
    expect(screen.queryByText(/helm upgrade/)).not.toBeInTheDocument();
  });

  it("explains the stable-only feed on an RC installation", async () => {
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["platform-releases:read"],
        },
      ],
      actions: ["platform-releases:read"],
      features: {},
    });
    vi.spyOn(api, "meta").mockResolvedValue({
      version: "0.1.0-rc.365",
      platformVersion: "0.1.0-rc.365",
      bootstrapRequired: false,
    });
    vi.spyOn(api, "latestPlatformRelease").mockRejectedValue(
      new ApiError(503, {
        code: "NoStableRelease",
        title: "No stable release available",
      }),
    );
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <UpgradePage />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByText("No stable release is published yet"),
    ).toBeVisible();
    expect(
      screen.getByText(
        /release candidate uses an exact operator-managed chart/i,
      ),
    ).toBeVisible();
    expect(screen.getByText("Stable release feed is empty.")).toBeVisible();
  });
});

function releaseFixture(): LatestPlatformRelease {
  const digest = `sha256:${"a".repeat(64)}`;
  const chart = {
    name: "kuberploy-installer",
    version: "0.1.0-rc.365",
    ociReference: "ghcr.io/kuberploy/charts/kuberploy-installer:0.1.0-rc.365",
    ociDigest: digest,
    package: "kuberploy-installer-0.1.0-rc.365.tgz",
    packageSha256: digest,
  };
  return {
    currentVersion: "0.1.0-rc.365",
    updateAvailable: true,
    compatibility: { status: "compatible", reasons: [] },
    lastCheckedAt: "2026-08-14T00:00:00Z",
    release: {
      tag: "v0.1.0-rc.365",
      version: "0.1.0-rc.365",
      manifestDigest: digest,
      publishedAt: "2026-08-14T00:00:00Z",
      notesUrl:
        "https://github.com/kuberploy/kuberploy/releases/tag/v0.1.0-rc.365",
      breakingChanges: false,
      chart,
      manifest: {
        $schema:
          "https://raw.githubusercontent.com/kuberploy/kuberploy/main/release/release-manifest.schema.json",
        schemaVersion: "2.0.0",
        release: {
          tag: "v0.1.0-rc.365",
          version: "0.1.0-rc.365",
          createdAt: "2026-08-14T00:00:00Z",
          notesUrl:
            "https://github.com/kuberploy/kuberploy/releases/tag/v0.1.0-rc.365",
          summary: "Release",
          breakingChanges: false,
        },
        source: { repository: "kuberploy/kuberploy", commit: "b".repeat(40) },
        versions: {
          kuberploy: "0.1.0-rc.365",
          api: "0.1.0-rc.365",
          worker: "0.1.0-rc.365",
          web: "0.1.0-rc.365",
          migration: "0.1.0-rc.365",
          builderAgent: "0.1.0-rc.365",
          chart: "0.1.0-rc.365",
        },
        compatibility: {
          supportedUpgradeFrom: ">=0.1.0 <0.2.0",
          kubernetes: {
            constraint: ">=1.34.0-0 <1.37.0-0",
            testedMinors: ["1.34", "1.35", "1.36"],
          },
          database: {
            engine: "postgresql",
            currentSchema: "006_remove_platform_self_upgrade",
            minimumUpgradeableSchema: "005_helm_unchanged_project_receipt",
            migrationSetSha256: digest,
            strategy: "prisma-migrate-deploy-with-advisory-lock",
            rollbackPolicy: "Operator-managed Helm rollback policy.",
          },
        },
        artifacts: { images: [], chart, componentCharts: [chart] },
        dependencyLock: { file: "DEPENDENCIES.md", sha256: digest },
      },
    },
  };
}
