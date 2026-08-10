import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { PlatformRelease } from "../api/types";
import { UpgradeConfirmation } from "./UpgradePage";

const digest = `sha256:${"a".repeat(64)}`;
const release: PlatformRelease = {
  tag: "v1.1.0",
  version: "1.1.0",
  manifestDigest: digest,
  publishedAt: "2026-08-06T00:00:00Z",
  notesUrl: "https://github.com/kuberploy/kuberploy/releases/tag/v1.1.0",
  breakingChanges: false,
  chart: {
    name: "kuberploy",
    version: "1.1.0",
    ociReference: "ghcr.io/kuberploy/charts/kuberploy:1.1.0",
    ociDigest: digest,
    package: "kuberploy-1.1.0.tgz",
    packageSha256: digest,
  },
  manifest: {
    $schema:
      "https://raw.githubusercontent.com/kuberploy/kuberploy/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/release/release-manifest.schema.json",
    schemaVersion: "1.0.0",
    release: {
      tag: "v1.1.0",
      version: "1.1.0",
      createdAt: "2026-08-06T00:00:00Z",
      notesUrl: "https://github.com/kuberploy/kuberploy/releases/tag/v1.1.0",
      summary: "A tested upgrade.",
      breakingChanges: false,
    },
    source: {
      repository: "kuberploy/kuberploy",
      commit: "a".repeat(40),
    },
    versions: {
      kuberploy: "1.1.0",
      api: "1.1.0",
      worker: "1.1.0",
      web: "1.1.0",
      upgrader: "1.1.0",
      builderAgent: "1.1.0",
      chart: "1.1.0",
    },
    compatibility: {
      supportedUpgradeFrom: ">=1.0.0 <1.1.0",
      kubernetes: {
        constraint: ">=1.34.0-0 <1.37.0-0",
        testedMinors: ["1.34", "1.35", "1.36"],
      },
      database: {
        engine: "postgresql",
        currentSchema: "003_team_github_access",
        minimumUpgradeableSchema: "001_initial",
        migrationSetSha256: digest,
        strategy: "ordered-expand-contract-with-advisory-lock",
        rollbackPolicy: "Use a compatible control-plane release only.",
      },
    },
    artifacts: {
      images: (
        ["api", "worker", "web", "upgrader", "builder-agent"] as const
      ).map((component) => ({
        component,
        reference: `ghcr.io/kuberploy/kuberploy-${component}`,
        digest,
        platforms: ["linux/amd64", "linux/arm64"],
      })),
      chart: {
        name: "kuberploy",
        version: "1.1.0",
        ociReference: "ghcr.io/kuberploy/charts/kuberploy:1.1.0",
        ociDigest: digest,
        package: "kuberploy-1.1.0.tgz",
        packageSha256: digest,
      },
      componentCharts: [
        "kuberploy-argocd",
        "kuberploy-builder",
        "kuberploy-cert-manager",
        "kuberploy-edge",
        "kuberploy-external-dns",
        "kuberploy-external-secrets",
        "kuberploy-monitoring",
        "kuberploy-postgresql",
        "kuberploy-registry",
        "kuberploy-runtime",
        "kuberploy-sealed-secrets",
        "kuberploy-valkey",
      ].map((name) => ({
        name,
        version: "1.1.0",
        ociReference: `ghcr.io/kuberploy/charts/${name}:1.1.0`,
        ociDigest: digest,
        package: `${name}-1.1.0.tgz`,
        packageSha256: digest,
      })),
    },
    dependencyLock: { file: "DEPENDENCIES.md", sha256: digest },
  },
};

describe("upgrade confirmation", () => {
  it("does not submit until the exact-release acknowledgement is checked", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(
      <UpgradeConfirmation
        release={release}
        compatibilityStatus="unknown"
        busy={false}
        error={null}
        onCancel={vi.fn()}
        onConfirm={onConfirm}
      />,
    );

    const submit = screen.getByRole("button", {
      name: /start verified upgrade/i,
    });
    expect(submit).toBeDisabled();
    expect(screen.getByText(digest)).toBeInTheDocument();

    await user.click(screen.getByRole("checkbox"));
    expect(submit).toBeEnabled();
    await user.click(submit);
    expect(onConfirm).toHaveBeenCalledOnce();
  });
});
