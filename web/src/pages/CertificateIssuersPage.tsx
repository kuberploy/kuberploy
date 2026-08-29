import { useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type {
  CertificateIssuerAdminEntry,
  CertificateIssuerMutation,
} from "../api/types";
import { formatDate, shortId } from "../lib/format";
import {
  Select,
  Button,
  Card,
  CardHeader,
  ConfirmDialog,
  DetailList,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  FormActions,
  FormGrid,
  Notice,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";

type Draft = CertificateIssuerMutation & { name: string };
type SaveCommand = {
  editorScope: string;
  editorSession: number;
  name: string;
  input: CertificateIssuerMutation;
  issuerId?: string;
  currentRevision?: number;
  idempotencyKey: string;
};

const emptyDraft: Draft = {
  name: "",
  environment: "production",
  email: "",
  accountPrivateKeySecretName: "",
  solver: { type: "http01" },
};

export function CertificateIssuersPage() {
  const queryClient = useQueryClient();
  const replayKey = useRef(crypto.randomUUID());
  const deactivateAttempt = useRef<{
    signature: string;
    key: string;
  } | null>(null);
  const [editing, setEditing] = useState<CertificateIssuerAdminEntry>();
  const [deactivationCandidate, setDeactivationCandidate] =
    useState<CertificateIssuerAdminEntry>();
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const editorSessionRef = useRef(0);
  const editorScope = JSON.stringify({ issuerId: editing?.id ?? null, draft });
  const editorScopeRef = useRef(editorScope);
  const [saveError, setSaveError] = useState<unknown>(null);
  editorScopeRef.current = editorScope;
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
    staleTime: 30_000,
  });
  const allowed =
    principal.data?.role === "platform-admin" &&
    principal.data.authentication.kind === "session" &&
    capabilities.data?.features?.certificateIssuerManagement === true;
  const catalog = useQuery({
    queryKey: ["platform-certificate-issuers"],
    queryFn: api.platformCertificateIssuers,
    enabled: allowed,
    retry: false,
  });
  const currentEditingIssuer = editing
    ? catalog.data?.items.find((issuer) => issuer.id === editing.id)
    : undefined;
  const editingIsCurrent =
    !editing ||
    Boolean(
      currentEditingIssuer &&
      currentEditingIssuer.lifecycle === "active" &&
      currentEditingIssuer.currentRevision === editing.currentRevision,
    );
  const save = useMutation({
    mutationFn: (input: SaveCommand) =>
      input.issuerId
        ? api.revisePlatformCertificateIssuer(
            input.issuerId,
            input.currentRevision ?? 0,
            input.input,
            input.idempotencyKey,
          )
        : api.createPlatformCertificateIssuer(
            input.name,
            input.input,
            input.idempotencyKey,
          ),
    onMutate: () => setSaveError(null),
    onSuccess: async (_value, input) => {
      const isCurrentEditor =
        input.editorScope === editorScopeRef.current &&
        input.editorSession === editorSessionRef.current;
      if (isCurrentEditor) {
        editorSessionRef.current += 1;
        setEditing(undefined);
        setDraft(emptyDraft);
        replayKey.current = crypto.randomUUID();
      }
      await queryClient.invalidateQueries({
        queryKey: ["platform-certificate-issuers"],
      });
    },
    onError: (error, input) => {
      if (
        input.editorScope === editorScopeRef.current &&
        input.editorSession === editorSessionRef.current
      )
        setSaveError(error);
    },
  });
  const deactivate = useMutation({
    mutationFn: ({
      entry,
      idempotencyKey,
    }: {
      entry: CertificateIssuerAdminEntry;
      idempotencyKey: string;
    }) =>
      api.deactivatePlatformCertificateIssuer(
        entry.id,
        entry.currentRevision,
        idempotencyKey,
      ),
    onSuccess: async (_value, input) => {
      if (deactivateAttempt.current?.key === input.idempotencyKey) {
        deactivateAttempt.current = null;
      }
      await queryClient.invalidateQueries({
        queryKey: ["platform-certificate-issuers"],
      });
    },
  });

  const deactivateIssuer = (entry: CertificateIssuerAdminEntry) => {
    const signature = JSON.stringify({
      issuerId: entry.id,
      revision: entry.currentRevision,
    });
    const idempotencyKey =
      deactivateAttempt.current?.signature === signature
        ? deactivateAttempt.current.key
        : crypto.randomUUID();
    deactivateAttempt.current = { signature, key: idempotencyKey };
    deactivate.mutate({ entry, idempotencyKey });
  };

  const change = (next: Draft) => {
    setDraft(next);
    replayKey.current = crypto.randomUUID();
    save.reset();
  };
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (editing && !editingIsCurrent) return;
    const { name, ...input } = draft;
    save.mutate({
      editorScope,
      editorSession: editorSessionRef.current,
      name,
      input,
      issuerId: editing?.id,
      currentRevision: editing?.currentRevision,
      idempotencyKey: replayKey.current,
    });
  };
  const edit = (entry: CertificateIssuerAdminEntry) => {
    editorSessionRef.current += 1;
    setEditing(entry);
    setDraft({
      name: entry.name,
      environment: entry.revision.environment,
      email: entry.revision.email,
      accountPrivateKeySecretName: entry.revision.accountPrivateKeySecretName,
      solver:
        entry.revision.solver === "http01"
          ? { type: "http01" }
          : {
              type: "dns01-cloudflare",
              dnsZones: [...(entry.revision.dnsZones ?? [])],
              apiTokenSecretName: entry.revision.apiTokenSecretName ?? "",
              apiTokenSecretKey: entry.revision.apiTokenSecretKey ?? "",
            },
    });
    replayKey.current = crypto.randomUUID();
  };

  if (principal.isPending || capabilities.isPending)
    return (
      <Page narrow className="[&>header]:mb-0">
        <Skeleton lines={7} />
      </Page>
    );
  if (!allowed)
    return (
      <Page narrow className="[&>header]:mb-0">
        <EmptyState
          title="Certificate issuer management unavailable"
          description="This page requires a human platform-admin session and a freshly ready protected issuer publisher and observer."
        />
      </Page>
    );

  const dnsSolver =
    draft.solver.type === "dns01-cloudflare" ? draft.solver : undefined;

  return (
    <Page narrow className="[&>header]:mb-0">
      <PageHeader
        eyebrow="Platform settings"
        title="Certificate issuers"
        description={
          <>
            Manage versioned Let&apos;s Encrypt ClusterIssuer profiles. Tenant
            workloads can only select a ready approved issuer; they never see
            ACME email, DNS zones, or Secret references.
          </>
        }
      />

      <Card>
        <CardHeader>
          <div>
            <h2>
              {editing ? "Add a revision" : "Create an issuer"}
            </h2>
            <p>
              HTTP-01 uses the protected Traefik solver. DNS-01 currently uses a
              pre-created Cloudflare API-token Secret; credential values are
              never entered here.
            </p>
          </div>
        </CardHeader>
        <FormGrid as="form" onSubmit={submit}>
          <Field label="Issuer name" required>
            <input
              value={draft.name}
              disabled={Boolean(editing)}
              required
              maxLength={63}
              pattern="[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?"
              placeholder="tenant-production"
              onChange={(event) =>
                change({ ...draft, name: event.target.value })
              }
            />
          </Field>
          <Field label="Let’s Encrypt environment" required>
            <Select
              value={draft.environment}
              onChange={(event) =>
                change({
                  ...draft,
                  environment: event.target.value as "production" | "staging",
                })
              }
            >
              <option value="staging">Staging (test certificates)</option>
              <option value="production">Production</option>
            </Select>
          </Field>
          <Field label="ACME account email" required>
            <input
              type="email"
              value={draft.email}
              required
              maxLength={254}
              onChange={(event) =>
                change({ ...draft, email: event.target.value })
              }
            />
          </Field>
          <Field label="ACME account Secret name" required>
            <input
              value={draft.accountPrivateKeySecretName}
              required
              maxLength={253}
              pattern="[a-z0-9](?:[-a-z0-9]{0,251}[a-z0-9])?"
              onChange={(event) =>
                change({
                  ...draft,
                  accountPrivateKeySecretName: event.target.value,
                })
              }
            />
          </Field>
          <Field label="Solver" required>
            <Select
              value={draft.solver.type}
              onChange={(event) =>
                change({
                  ...draft,
                  solver:
                    event.target.value === "http01"
                      ? { type: "http01" }
                      : {
                          type: "dns01-cloudflare",
                          dnsZones: [],
                          apiTokenSecretName: "",
                          apiTokenSecretKey: "api-token",
                        },
                })
              }
            >
              <option value="http01">HTTP-01 (Traefik)</option>
              <option value="dns01-cloudflare">DNS-01 (Cloudflare)</option>
            </Select>
          </Field>
          {dnsSolver ? (
            <>
              <Field
                label="Authorized DNS zones"
                hint="One exact zone per line. Wildcard hosts are allowed only inside these zones."
                required
              >
                <textarea
                  rows={3}
                  required
                  value={dnsSolver.dnsZones.join("\n")}
                  onChange={(event) =>
                    change({
                      ...draft,
                      solver: {
                        ...dnsSolver,
                        dnsZones: event.target.value
                          .split(/\r?\n/)
                          .map((zone) => zone.trim().toLowerCase())
                          .filter(Boolean),
                      },
                    })
                  }
                />
              </Field>
              <Field label="Cloudflare API-token Secret name" required>
                <input
                  required
                  value={dnsSolver.apiTokenSecretName}
                  onChange={(event) =>
                    change({
                      ...draft,
                      solver: {
                        ...dnsSolver,
                        apiTokenSecretName: event.target.value,
                      },
                    })
                  }
                />
              </Field>
              <Field label="Cloudflare API-token Secret key" required>
                <input
                  required
                  value={dnsSolver.apiTokenSecretKey}
                  onChange={(event) =>
                    change({
                      ...draft,
                      solver: {
                        ...dnsSolver,
                        apiTokenSecretKey: event.target.value,
                      },
                    })
                  }
                />
              </Field>
            </>
          ) : null}
          <FormActions>
            <Button
              type="submit"
              disabled={save.isPending || Boolean(editing && !editingIsCurrent)}
            >
              {save.isPending
                ? "Publishing…"
                : editing
                  ? "Publish revision"
                  : "Create issuer"}
            </Button>
            {editing ? (
              <Button
                type="button"
                variant="secondary"
                onClick={() => {
                  editorSessionRef.current += 1;
                  setEditing(undefined);
                  setDraft(emptyDraft);
                  replayKey.current = crypto.randomUUID();
                }}
              >
                Cancel
              </Button>
            ) : null}
          </FormActions>
          {editing && !editingIsCurrent ? (
            <Notice tone="warning">
              This issuer changed, was deactivated, or is no longer available.
              Reload the catalog before publishing a revision.
            </Notice>
          ) : null}
          {saveError ? <ErrorPanel error={saveError} /> : null}
        </FormGrid>
      </Card>

      {catalog.isPending ? <Skeleton lines={5} /> : null}
      {catalog.error ? <ErrorPanel error={catalog.error} /> : null}
      {catalog.data?.items.length === 0 ? (
        <EmptyState
          title="No managed issuers"
          description="Create an HTTP-01 or Cloudflare DNS-01 profile. Bootstrap chart issuers remain separately protected."
        />
      ) : null}
      <div className="grid grid-cols-[repeat(auto-fill,_minmax(min(100%,_320px),_1fr))] items-start gap-4 to-700:grid-cols-[minmax(0,_1fr)]">
        {catalog.data?.items.map((entry) => (
          <Card key={entry.id}>
            <CardHeader>
              <div>
                <h2>{entry.name}</h2>
                <p>
                  Revision {entry.currentRevision} · {shortId(entry.id)} ·{" "}
                  {entry.revision.environment}
                </p>
              </div>
              <StatusPill value={entry.lifecycle} label={entry.lifecycle} />
            </CardHeader>
            <DetailList>
              <div>
                <dt>Solver</dt>
                <dd>{entry.revision.solver}</dd>
              </div>
              <div>
                <dt>Materialization</dt>
                <dd>{entry.observation.state}</dd>
              </div>
              <div>
                <dt>ACME email</dt>
                <dd>{entry.revision.email}</dd>
              </div>
              <div>
                <dt>Account Secret</dt>
                <dd>
                  <code>{entry.revision.accountPrivateKeySecretName}</code>
                </dd>
              </div>
              {entry.revision.solver === "dns01-cloudflare" ? (
                <>
                  <div>
                    <dt>Zones</dt>
                    <dd>{entry.revision.dnsZones?.join(", ")}</dd>
                  </div>
                  <div>
                    <dt>Token Secret</dt>
                    <dd>
                      <code>
                        {entry.revision.apiTokenSecretName}/
                        {entry.revision.apiTokenSecretKey}
                      </code>
                    </dd>
                  </div>
                </>
              ) : null}
              <div>
                <dt>Updated</dt>
                <dd>{formatDate(entry.observation.updatedAt)}</dd>
              </div>
            </DetailList>
            {entry.observation.reason ? (
              <p>{entry.observation.reason}</p>
            ) : null}
            {entry.lifecycle === "active" ? (
              <FormActions>
                <Button
                  type="button"
                  variant="secondary"
                  onClick={() => edit(entry)}
                >
                  Revise
                </Button>
                <Button
                  type="button"
                  variant="danger"
                  disabled={deactivate.isPending}
                  onClick={() => setDeactivationCandidate(entry)}
                >
                  Deactivate
                </Button>
              </FormActions>
            ) : null}
          </Card>
        ))}
      </div>
      {deactivate.error ? <ErrorPanel error={deactivate.error} /> : null}
      {deactivationCandidate ? (
        <ConfirmDialog
          title={`Deactivate ${deactivationCandidate.name}?`}
          description="Existing route references must be removed first."
          confirmLabel="Deactivate issuer"
          icon="close"
          busy={deactivate.isPending}
          onCancel={() => setDeactivationCandidate(undefined)}
          onConfirm={() => {
            const entry = deactivationCandidate;
            setDeactivationCandidate(undefined);
            deactivateIssuer(entry);
          }}
        />
      ) : null}
    </Page>
  );
}
