import { useRef } from "react";
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
    onSuccess: async () => {
      form.current?.reset();
      replayKey.current = crypto.randomUUID();
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
    <div className="settings-page">
      <div className="page-heading">
        <div>
          <span className="eyebrow">Platform settings</span>
          <h1>Helm approvals</h1>
          <p>
            Publish immutable chart digest approvals. Credentials, chart
            documents, renderer arguments, and values are never accepted here.
          </p>
        </div>
      </div>
      <Card>
        <div className="card__header card__header--inside">
          <div>
            <h2>Approve an immutable package</h2>
            <p>
              All three SHA-256 digests must come from the trusted offline
              catalog workflow.
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
            create.mutate({
              key: replayKey.current,
              input: {
                repository: String(data.get("repository") ?? ""),
                version: String(data.get("version") ?? ""),
                manifestDigest: String(data.get("manifestDigest") ?? ""),
                packageDigest: String(data.get("packageDigest") ?? ""),
                valuesSchemaDigest: String(
                  data.get("valuesSchemaDigest") ?? "",
                ),
              },
            });
          }}
        >
          <Field label="OCI repository" required>
            <input
              name="repository"
              maxLength={512}
              required
              placeholder="oci://registry.example.com/charts/service"
            />
          </Field>
          <Field label="Chart version" required>
            <input name="version" maxLength={128} required />
          </Field>
          {(
            ["manifestDigest", "packageDigest", "valuesSchemaDigest"] as const
          ).map((name) => (
            <Field key={name} label={name.replace(/([A-Z])/g, " $1")} required>
              <input
                name={name}
                maxLength={71}
                pattern={digestPattern}
                required
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
                  {approval.repository} · {approval.version}
                </strong>
                <small>
                  Approval {approval.revision} ·{" "}
                  {formatDate(approval.createdAt)}
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
