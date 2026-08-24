import { useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { CreateHelmApproval } from "../api/types";
import { hasHelmApprovalManagementAccess } from "../lib/helmApprovalAccess";
import { formatDate, shortId } from "../lib/format";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  Skeleton,
} from "../components/ui";

const digestPattern = "sha256:[0-9a-f]{64}";

export function HelmApprovalsPage() {
  const client = useQueryClient();
  const form = useRef<HTMLFormElement>(null);
  const replayKey = useRef(crypto.randomUUID());
  const [sourceKind, setSourceKind] =
    useState<CreateHelmApproval["sourceKind"]>("oci");
  const principal = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: false,
    staleTime: 60_000,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
    staleTime: 60_000,
  });
  const allowed = hasHelmApprovalManagementAccess(
    principal.data,
    capabilities.data?.features,
    capabilities.data?.capabilities ?? [],
  );
  const catalog = useQuery({
    queryKey: ["platform-helm-approvals"],
    queryFn: api.platformHelmApprovals,
    enabled: allowed,
    retry: false,
  });
  const create = useMutation({
    mutationFn: ({ input, key }: { input: CreateHelmApproval; key: string }) =>
      api.createPlatformHelmApproval(input, key),
    onSuccess: async (_value, input) => {
      const current = form.current
        ? new FormData(form.current)
        : new FormData();
      const submitted = input.input as unknown as Record<
        string,
        string | undefined
      >;
      const sameDraft = [
        "repository",
        "version",
        "chartName",
        "sourceRevision",
        "chartPath",
        "manifestDigest",
        "packageDigest",
        "valuesSchemaDigest",
      ].every(
        (field) =>
          String(current.get(field) ?? "") === String(submitted[field] ?? ""),
      );
      if (sameDraft) {
        form.current?.reset();
        replayKey.current = crypto.randomUUID();
      }
      await client.invalidateQueries({ queryKey: ["platform-helm-approvals"] });
    },
  });
  if (principal.isPending || capabilities.isPending)
    return <Skeleton lines={6} />;
  if (!allowed) {
    return (
      <EmptyState
        title="Helm approval management unavailable"
        description="This page requires a human session, the Helm approvals feature, and an exact platform helm-approvals:manage grant."
      />
    );
  }
  return (
    <div className="page page--narrow settings-page">
      <div className="page-header page-heading">
        <div>
          <span className="eyebrow">Platform settings</span>
          <h1>Helm approvals</h1>
          <p>
            Resolve OCI, classic Helm repository, or public Git chart sources
            into one immutable offline-rendered package.
          </p>
        </div>
      </div>
      <Card>
        <div className="card__header card__header--inside">
          <div>
            <h2>Approve an immutable package</h2>
            <p>
              OCI requires its immutable manifest and package digests. Helm
              repositories and Git are resolved server-side, then pinned before
              the approval becomes visible to Apps.
            </p>
          </div>
        </div>
        <form
          ref={form}
          className="form-grid"
          onChange={() => {
            replayKey.current = crypto.randomUUID();
            create.reset();
          }}
          onSubmit={(event) => {
            event.preventDefault();
            const data = new FormData(event.currentTarget);
            const common = {
              repository: String(data.get("repository") ?? ""),
              version: String(data.get("version") ?? ""),
              manifestDigest:
                String(data.get("manifestDigest") ?? "") || undefined,
              packageDigest:
                String(data.get("packageDigest") ?? "") || undefined,
              valuesSchemaDigest:
                String(data.get("valuesSchemaDigest") ?? "") || undefined,
            };
            const input: CreateHelmApproval =
              sourceKind === "oci"
                ? { ...common, sourceKind: "oci" }
                : sourceKind === "helm-repository"
                  ? {
                      ...common,
                      sourceKind: "helm-repository",
                      chartName: String(data.get("chartName") ?? ""),
                    }
                  : {
                      ...common,
                      sourceKind: "git",
                      chartName: String(data.get("chartName") ?? ""),
                      sourceRevision: String(data.get("sourceRevision") ?? ""),
                      chartPath: String(data.get("chartPath") ?? ""),
                    };
            create.mutate({
              key: replayKey.current,
              input,
            });
          }}
        >
          <Field label="Chart source" required>
            <select
              name="sourceKind"
              value={sourceKind}
              onChange={(event) =>
                setSourceKind(
                  event.target.value as CreateHelmApproval["sourceKind"],
                )
              }
            >
              <option value="oci">OCI registry</option>
              <option value="helm-repository">Helm repository</option>
              <option value="git">Public Git repository</option>
            </select>
          </Field>
          <Field
            label={
              sourceKind === "oci"
                ? "OCI repository"
                : sourceKind === "helm-repository"
                  ? "Repository URL"
                  : "Git repository URL"
            }
            required
          >
            <input
              name="repository"
              maxLength={2048}
              required
              placeholder={
                sourceKind === "oci"
                  ? "oci://registry.example.com/charts/service"
                  : sourceKind === "helm-repository"
                    ? "https://charts.example.com/stable"
                    : "https://git.example.com/team/charts.git"
              }
            />
          </Field>
          {sourceKind !== "oci" ? (
            <Field label="Chart name" required>
              <input name="chartName" maxLength={253} required />
            </Field>
          ) : null}
          <Field label="Chart version" required>
            <input name="version" maxLength={128} required />
          </Field>
          {sourceKind === "git" ? (
            <>
              <Field label="Commit SHA" required>
                <input
                  name="sourceRevision"
                  minLength={40}
                  maxLength={64}
                  pattern="[0-9a-f]{40}|[0-9a-f]{64}"
                  required
                  spellCheck={false}
                />
              </Field>
              <Field label="Chart path" required>
                <input
                  name="chartPath"
                  maxLength={512}
                  required
                  placeholder="charts/service"
                />
              </Field>
            </>
          ) : null}
          {(
            ["manifestDigest", "packageDigest", "valuesSchemaDigest"] as const
          ).map((name) => (
            <Field
              key={name}
              label={name.replace(/([A-Z])/g, " $1")}
              required={sourceKind === "oci" && name !== "valuesSchemaDigest"}
              hint={
                sourceKind === "oci" && name !== "valuesSchemaDigest"
                  ? "Required for OCI admission."
                  : "Optional expected digest; Kuberploy calculates and pins the actual value."
              }
            >
              <input
                name={name}
                maxLength={71}
                pattern={digestPattern}
                required={sourceKind === "oci" && name !== "valuesSchemaDigest"}
                placeholder="sha256:…"
                spellCheck={false}
              />
            </Field>
          ))}
          <Button type="submit" busy={create.isPending}>
            Create immutable approval
          </Button>
        </form>
        {create.error ? <ErrorPanel error={create.error} /> : null}
        {create.data ? (
          <div className="notice notice--success" role="status">
            <div>
              <strong>Approval created</strong>
              <p>
                Approval {create.data.revision} ·{" "}
                <code>{shortId(create.data.id)}</code>
              </p>
            </div>
          </div>
        ) : null}
      </Card>
      <Card>
        <div className="card__header card__header--inside">
          <div>
            <h2>Immutable catalog</h2>
            <p>Catalog entries cannot be edited from this interface.</p>
          </div>
        </div>
        {catalog.isPending ? <Skeleton lines={5} /> : null}
        {catalog.error ? <ErrorPanel error={catalog.error} /> : null}
        {catalog.data?.items.length === 0 ? (
          <EmptyState
            title="No Helm approvals"
            description="Create the first immutable digest approval above."
          />
        ) : null}
        <div className="helm-history-list">
          {catalog.data?.items.map((approval) => (
            <article
              className="helm-history-item"
              key={`${approval.id}:${approval.revision}`}
            >
              <div>
                <strong>
                  {approval.chartName} · {approval.version}
                </strong>
                <small>
                  {approval.sourceKind} · {approval.repository} · Approval{" "}
                  {approval.revision} · {formatDate(approval.createdAt)}
                </small>
                <small>
                  Manifest <code>{approval.manifestDigest}</code>
                </small>
                <small>
                  Package <code>{approval.packageDigest}</code>
                </small>
                <small>
                  Values schema <code>{approval.valuesSchemaDigest}</code>
                </small>
              </div>
            </article>
          ))}
        </div>
      </Card>
    </div>
  );
}
