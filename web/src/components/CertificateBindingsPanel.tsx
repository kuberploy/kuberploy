import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, errorMessage } from "../api/client";
import type {
  Application,
  Capability,
  CertificateBindingDetail,
  CertificateBindingMetadata,
  Environment,
  Project,
} from "../api/types";
import {
  certificateEnvironments,
  hasCertificateCapability,
} from "../lib/certificateAccess";
import { formatDate } from "../lib/format";
import { writeOnlyRequestSignature } from "../lib/writeOnlyRequest";
import { Icon } from "./Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
} from "./ui";

const certificateNamePattern = /^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/;
const certificatePEMMaxBytes = 65_536;
const privateKeyPEMMaxBytes = 32_768;

type CertificateWritePayload = {
  certificatePem: string;
  privateKeyPem: string;
};

type CertificateWriteCollection =
  { ok: true; payload: CertificateWritePayload } | { ok: false; error: string };

function collectCertificateWritePayload(
  form: HTMLFormElement,
): CertificateWriteCollection {
  const certificate = form.elements.namedItem("certificatePem");
  const privateKey = form.elements.namedItem("privateKeyPem");
  if (
    !(certificate instanceof HTMLTextAreaElement) ||
    !(privateKey instanceof HTMLTextAreaElement)
  ) {
    return { ok: false, error: "Certificate form fields are unavailable." };
  }
  const encoder = new TextEncoder();
  const certificateBytes = encoder.encode(certificate.value).byteLength;
  const privateKeyBytes = encoder.encode(privateKey.value).byteLength;
  if (certificateBytes < 1 || certificateBytes > certificatePEMMaxBytes) {
    return {
      ok: false,
      error: "Certificate PEM must contain 1–65,536 UTF-8 bytes.",
    };
  }
  if (privateKeyBytes < 1 || privateKeyBytes > privateKeyPEMMaxBytes) {
    return {
      ok: false,
      error: "Private-key PEM must contain 1–32,768 UTF-8 bytes.",
    };
  }
  return {
    ok: true,
    payload: {
      certificatePem: certificate.value,
      privateKeyPem: privateKey.value,
    },
  };
}

function clearCertificatePEM(form: HTMLFormElement) {
  form
    .querySelectorAll<HTMLTextAreaElement>("[data-certificate-pem]")
    .forEach((input) => {
      input.value = "";
    });
}

function destroyCertificateWritePayload(payload?: CertificateWritePayload) {
  if (!payload) return;
  payload.certificatePem = "";
  payload.privateKeyPem = "";
}

function CertificatePEMFields({ prefix }: { prefix: string }) {
  return (
    <div className="runtime-secret-write-fields">
      <div className="notice notice--warning" role="status">
        <Icon name="route" />
        <div>
          <strong>Write-only certificate material</strong>
          <p>
            These uncontrolled fields are read only for this request, cleared
            immediately after submission, and never placed in query cache,
            browser storage, URLs, logs, or response state.
          </p>
        </div>
      </div>
      <Field label="Certificate chain PEM" required>
        <textarea
          aria-label={`${prefix} certificate chain PEM`}
          name="certificatePem"
          data-certificate-pem
          rows={9}
          maxLength={certificatePEMMaxBytes}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>
      <Field label="Private key PEM" required>
        <textarea
          aria-label={`${prefix} private key PEM`}
          name="privateKeyPem"
          data-certificate-pem
          rows={7}
          maxLength={privateKeyPEMMaxBytes}
          autoComplete="new-password"
          spellCheck={false}
        />
      </Field>
    </div>
  );
}

function CreateCertificateForm({
  application,
  environment,
  onCreated,
  onClose,
}: {
  application: Application;
  environment: Environment;
  onCreated: (binding: CertificateBindingDetail) => void;
  onClose: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const submitBusyRef = useRef(false);
  const [feedback, setFeedback] = useState("");
  const [retryKey, setRetryKey] = useState("");
  const [retrySignature, setRetrySignature] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busy || submitBusyRef.current) return;
    const form = event.currentTarget;
    const nameInput = form.elements.namedItem("name");
    const name =
      nameInput instanceof HTMLInputElement ? nameInput.value.trim() : "";
    if (!certificateNamePattern.test(name)) {
      setFeedback(
        "Name must be a lowercase DNS label containing at most 63 characters.",
      );
      return;
    }
    const collected = collectCertificateWritePayload(form);
    if (!collected.ok) {
      setFeedback(collected.error);
      clearCertificatePEM(form);
      return;
    }
    const input = {
      environmentId: environment.id,
      name,
      ...collected.payload,
    };
    submitBusyRef.current = true;
    setBusy(true);
    let signature: string;
    try {
      signature = await writeOnlyRequestSignature({
        applicationId: application.id,
        input,
      });
    } catch {
      destroyCertificateWritePayload(input);
      clearCertificatePEM(form);
      submitBusyRef.current = false;
      setBusy(false);
      setFeedback(
        "The write-only certificate request could not be prepared. Try again.",
      );
      return;
    }
    const idempotencyKey =
      retryKey && retrySignature === signature ? retryKey : crypto.randomUUID();
    if (!retryKey || retrySignature !== signature) setRetryKey(idempotencyKey);
    clearCertificatePEM(form);
    setFeedback("");
    try {
      const created = await api.createCertificateBinding(
        application.id,
        input,
        idempotencyKey,
      );
      setRetryKey("");
      setRetrySignature("");
      onCreated(created);
    } catch (error) {
      void error;
      setRetrySignature(signature);
      setFeedback(
        "The write-only certificate request failed. PEM was cleared; re-enter the exact same material to retry with the protected idempotency key.",
      );
    } finally {
      destroyCertificateWritePayload(input);
      submitBusyRef.current = false;
      setBusy(false);
    }
  }

  return (
    <Card className="runtime-secret-form-card">
      <div className="card__header card__header--inside">
        <div>
          <span className="eyebrow">{environment.name}</span>
          <h3>New custom certificate</h3>
          <p>
            The platform validates the key pair and derives its immutable
            Kubernetes TLS Secret identity.
          </p>
        </div>
        <Button
          type="button"
          variant="secondary"
          onClick={onClose}
          disabled={busy}
        >
          Cancel
        </Button>
      </div>
      <form onSubmit={(event) => void submit(event)}>
        <fieldset disabled={busy}>
          <Field label="Binding name" required>
            <input
              aria-label="Certificate binding name"
              name="name"
              autoComplete="off"
              spellCheck={false}
              maxLength={63}
              placeholder="public-edge"
            />
          </Field>
          <CertificatePEMFields prefix="Create certificate" />
          {feedback ? (
            <div className="notice notice--error" role="alert">
              {feedback}
            </div>
          ) : null}
          {retryKey ? (
            <small className="runtime-secret-retry-note">
              A stable idempotency key is retained for this form retry. Re-enter
              the exact same PEM or cancel and start a new request.
            </small>
          ) : null}
          <div className="runtime-secret-form-actions">
            <Button type="submit" busy={busy}>
              Validate and create
            </Button>
          </div>
        </fieldset>
      </form>
    </Card>
  );
}

function CertificateDetail({
  binding,
  canRotate,
  canDelete,
  onChanged,
  onDeleted,
}: {
  binding: CertificateBindingDetail;
  canRotate: boolean;
  canDelete: boolean;
  onChanged: () => void;
  onDeleted: () => void;
}) {
  const [rotateBusy, setRotateBusy] = useState(false);
  const rotateBusyRef = useRef(false);
  const [rotateFeedback, setRotateFeedback] = useState("");
  const [rotateRetryKey, setRotateRetryKey] = useState("");
  const [rotateRetrySignature, setRotateRetrySignature] = useState("");
  const [deleteBusy, setDeleteBusy] = useState(false);
  const deleteBusyRef = useRef(false);
  const [deleteFeedback, setDeleteFeedback] = useState("");
  const [deleteRetryKey, setDeleteRetryKey] = useState("");

  useEffect(() => {
    // A failed retry is tied to the observed certificate CAS version. Do not
    // replay it after another observation advances the active version.
    setRotateRetryKey("");
    setRotateRetrySignature("");
    setRotateFeedback("");
  }, [binding.id, binding.activeVersion]);

  async function rotate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (rotateBusy || rotateBusyRef.current || !binding.activeVersion) return;
    const form = event.currentTarget;
    const collected = collectCertificateWritePayload(form);
    if (!collected.ok) {
      setRotateFeedback(collected.error);
      clearCertificatePEM(form);
      return;
    }
    const input = {
      expectedActiveVersion: binding.activeVersion,
      ...collected.payload,
    };
    rotateBusyRef.current = true;
    setRotateBusy(true);
    let signature: string;
    try {
      signature = await writeOnlyRequestSignature({
        bindingId: binding.id,
        input,
      });
    } catch {
      destroyCertificateWritePayload(input);
      clearCertificatePEM(form);
      rotateBusyRef.current = false;
      setRotateBusy(false);
      setRotateFeedback(
        "The write-only certificate rotation could not be prepared. Try again.",
      );
      return;
    }
    const idempotencyKey =
      rotateRetryKey && rotateRetrySignature === signature
        ? rotateRetryKey
        : crypto.randomUUID();
    if (!rotateRetryKey || rotateRetrySignature !== signature) {
      setRotateRetryKey(idempotencyKey);
    }
    clearCertificatePEM(form);
    setRotateFeedback("");
    try {
      await api.rotateCertificateBinding(binding.id, input, idempotencyKey);
      setRotateRetryKey("");
      setRotateRetrySignature("");
      onChanged();
    } catch (error) {
      void error;
      setRotateRetrySignature(signature);
      setRotateFeedback(
        "The write-only certificate rotation failed. PEM was cleared; re-enter the exact same material to retry with the protected idempotency key.",
      );
    } finally {
      destroyCertificateWritePayload(input);
      rotateBusyRef.current = false;
      setRotateBusy(false);
    }
  }

  async function remove(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (deleteBusy || deleteBusyRef.current) return;
    const form = event.currentTarget;
    const confirmation = form.elements.namedItem("confirmation");
    if (
      !(confirmation instanceof HTMLInputElement) ||
      confirmation.value !== binding.name
    ) {
      setDeleteFeedback("Enter the exact binding name to confirm deletion.");
      return;
    }
    confirmation.value = "";
    const idempotencyKey = deleteRetryKey || crypto.randomUUID();
    if (!deleteRetryKey) setDeleteRetryKey(idempotencyKey);
    deleteBusyRef.current = true;
    setDeleteBusy(true);
    setDeleteFeedback("");
    try {
      await api.deleteCertificateBinding(binding.id, idempotencyKey);
      setDeleteRetryKey("");
      onDeleted();
    } catch (error) {
      setDeleteFeedback(errorMessage(error));
    } finally {
      deleteBusyRef.current = false;
      setDeleteBusy(false);
    }
  }

  return (
    <Card className="runtime-secret-detail">
      <div className="card__header card__header--inside">
        <div>
          <span className="eyebrow">Public certificate metadata</span>
          <h3>{binding.name}</h3>
          <p>
            Binding <code>{binding.id}</code>
          </p>
        </div>
        <StatusPill value={binding.state} />
      </div>
      <div className="runtime-secret-detail-grid">
        <div>
          <span>Environment</span>
          <code>{binding.environmentId}</code>
        </div>
        <div>
          <span>Active version</span>
          <strong>
            {binding.activeVersion ? `v${binding.activeVersion}` : "None"}
          </strong>
        </div>
        <div>
          <span>Updated</span>
          <strong>{formatDate(binding.updatedAt)}</strong>
        </div>
      </div>
      <section className="runtime-secret-versions">
        <div className="runtime-secret-subhead">
          <div>
            <h4>Immutable public attestations</h4>
            <p>Private keys and raw certificate bytes are never returned.</p>
          </div>
        </div>
        <div className="runtime-secret-version-list">
          {binding.versions.map((version) => (
            <article key={version.number}>
              <div>
                <strong>Version {version.number}</strong>
                <small>
                  {formatDate(version.notBefore)} –{" "}
                  {formatDate(version.notAfter)}
                </small>
              </div>
              <ul>
                {version.dnsNames.map((name) => (
                  <li key={name}>
                    <code>{name}</code>
                  </li>
                ))}
              </ul>
              <small>
                Leaf <code>{version.leafFingerprint}</code>
              </small>
            </article>
          ))}
        </div>
      </section>
      {canRotate && binding.state === "ready" && binding.activeVersion ? (
        <form
          className="runtime-secret-rotation-form"
          onSubmit={(event) => void rotate(event)}
        >
          <fieldset disabled={rotateBusy}>
            <div className="runtime-secret-subhead">
              <div>
                <h4>Rotate from version {binding.activeVersion}</h4>
                <p>
                  The active version is used as an exact compare-and-swap guard.
                </p>
              </div>
            </div>
            <CertificatePEMFields prefix="Rotate certificate" />
            {rotateFeedback ? (
              <div className="notice notice--error" role="alert">
                {rotateFeedback}
              </div>
            ) : null}
            {rotateRetryKey ? (
              <PlaceholderBadge>
                Stable rotation retry protected
              </PlaceholderBadge>
            ) : null}
            <Button type="submit" busy={rotateBusy}>
              Validate and rotate
            </Button>
          </fieldset>
        </form>
      ) : null}
      {canDelete ? (
        <form
          className="runtime-secret-delete"
          onSubmit={(event) => void remove(event)}
        >
          <fieldset disabled={deleteBusy}>
            <div>
              <h4>Delete certificate binding</h4>
              <p>
                Deletion fails while any Git, active release, or retained
                rollback reference still uses a version.
              </p>
            </div>
            <Field label={`Type ${binding.name} to confirm`}>
              <input
                aria-label="Exact certificate binding name confirmation"
                name="confirmation"
                autoComplete="off"
                spellCheck={false}
              />
            </Field>
            <Button type="submit" variant="danger" busy={deleteBusy}>
              Delete certificate
            </Button>
            {deleteFeedback ? (
              <div className="notice notice--error" role="alert">
                {deleteFeedback}
              </div>
            ) : null}
            {deleteRetryKey ? (
              <PlaceholderBadge>Stable delete retry protected</PlaceholderBadge>
            ) : null}
          </fieldset>
        </form>
      ) : null}
    </Card>
  );
}

export function CertificateBindingsPanel({
  application,
  environments,
  project,
  capabilities,
  featureEnabled,
  humanSession,
}: {
  application: Application;
  environments: Environment[];
  project?: Project;
  capabilities: Capability[];
  featureEnabled: boolean;
  humanSession: boolean;
}) {
  const queryClient = useQueryClient();
  const [selectedEnvironmentId, setSelectedEnvironmentId] = useState("");
  const [selectedBindingId, setSelectedBindingId] = useState("");
  const [creating, setCreating] = useState(false);
  const formScopeRef = useRef("");
  const selectedBindingRef = useRef("");
  selectedBindingRef.current = selectedBindingId;
  const readableEnvironments = certificateEnvironments(
    capabilities,
    "certificate-bindings:read",
    application,
    environments,
    project,
  );
  const selectedEnvironment =
    readableEnvironments.find(
      (environment) => environment.id === selectedEnvironmentId,
    ) ?? readableEnvironments[0];
  const formScope = `${application.id}:${selectedEnvironment?.id ?? ""}`;
  formScopeRef.current = formScope;
  useEffect(() => {
    formScopeRef.current = formScope;
  }, [formScope]);
  useEffect(() => {
    setSelectedEnvironmentId("");
    setSelectedBindingId("");
    setCreating(false);
  }, [application.id]);
  useEffect(() => {
    if (
      selectedEnvironmentId &&
      !readableEnvironments.some(
        (environment) => environment.id === selectedEnvironmentId,
      )
    ) {
      setSelectedEnvironmentId("");
      setSelectedBindingId("");
      setCreating(false);
    }
  }, [readableEnvironments, selectedEnvironmentId]);
  const list = useQuery({
    queryKey: ["certificate-bindings", application.id, selectedEnvironment?.id],
    queryFn: () =>
      api.certificateBindings(application.id, selectedEnvironment!.id),
    enabled: featureEnabled && humanSession && Boolean(selectedEnvironment?.id),
    retry: false,
    refetchInterval: (query) =>
      query.state.data?.items.some(
        (binding) =>
          binding.state === "provisioning" || binding.state === "deleting",
      )
        ? 1_000
        : false,
  });
  const selectedListedBinding = list.data?.items.find(
    (binding) => binding.id === selectedBindingId,
  );
  const detail = useQuery({
    queryKey: [
      "certificate-binding",
      selectedBindingId,
      selectedListedBinding?.activeVersion ?? 0,
      selectedListedBinding?.state ?? "",
    ],
    queryFn: () => api.certificateBinding(selectedBindingId),
    enabled:
      featureEnabled &&
      humanSession &&
      Boolean(selectedBindingId) &&
      Boolean(selectedListedBinding),
    retry: false,
    refetchInterval: (query) => {
      const binding = query.state.data;
      if (!binding) return false;
      const pendingVersion =
        binding.state === "ready" &&
        binding.versions.some(
          (version) => version.number > (binding.activeVersion ?? 0),
        );
      return binding.state === "provisioning" ||
        binding.state === "deleting" ||
        pendingVersion
        ? 1_000
        : false;
    },
  });

  if (!featureEnabled) return null;
  if (!humanSession) {
    return (
      <EmptyState
        icon="route"
        title="Interactive session required"
        description="Custom-certificate metadata and mutations are intentionally excluded from service-account and agent sessions."
      />
    );
  }
  if (!selectedEnvironment) {
    return (
      <EmptyState
        icon="route"
        title="Certificate metadata access not granted"
        description="An exact certificate-bindings:read capability covering an application environment is required."
      />
    );
  }

  const canCreate = hasCertificateCapability(
    capabilities,
    "certificate-bindings:create",
    application,
    selectedEnvironment,
    project,
  );
  const canRotate = hasCertificateCapability(
    capabilities,
    "certificate-bindings:rotate",
    application,
    selectedEnvironment,
    project,
  );
  const canDelete = hasCertificateCapability(
    capabilities,
    "certificate-bindings:delete",
    application,
    selectedEnvironment,
    project,
  );

  async function refreshList() {
    await queryClient.invalidateQueries({
      queryKey: [
        "certificate-bindings",
        application.id,
        selectedEnvironment.id,
      ],
    });
  }

  return (
    <div className="runtime-secrets-panel">
      <Card className="runtime-secret-toolbar">
        <div>
          <span className="runtime-secret-toolbar__icon">
            <Icon name="route" />
          </span>
          <span>
            <strong>Custom TLS certificates</strong>
            <small>Public metadata is readable; PEM is write-only.</small>
          </span>
        </div>
        <Field label="Application environment">
          <select
            aria-label="Certificate environment"
            value={selectedEnvironment.id}
            onChange={(event) => {
              setSelectedEnvironmentId(event.target.value);
              setSelectedBindingId("");
              setCreating(false);
            }}
          >
            {readableEnvironments.map((environment) => (
              <option key={environment.id} value={environment.id}>
                {environment.name} · {environment.namespace}
              </option>
            ))}
          </select>
        </Field>
        {canCreate ? (
          <Button type="button" onClick={() => setCreating((value) => !value)}>
            <Icon name="plus" /> New certificate
          </Button>
        ) : null}
      </Card>

      {creating && canCreate ? (
        <CreateCertificateForm
          application={application}
          environment={selectedEnvironment}
          onClose={() => setCreating(false)}
          onCreated={(binding) => {
            if (formScopeRef.current !== formScope) return;
            setCreating(false);
            setSelectedBindingId(binding.id);
            void refreshList();
          }}
        />
      ) : null}

      {list.error ? (
        <ErrorPanel
          error={list.error}
          title="Could not load certificate metadata"
          onRetry={() => void list.refetch()}
        />
      ) : list.isPending ? (
        <Card>
          <Skeleton lines={6} />
        </Card>
      ) : list.data?.items.length ? (
        <div className="runtime-secret-layout">
          <Card className="runtime-secret-binding-list">
            <div className="card__header card__header--inside">
              <div>
                <span className="eyebrow">Environment metadata</span>
                <h3>Certificate bindings</h3>
              </div>
              <PlaceholderBadge>
                {list.data.items.length} certificates
              </PlaceholderBadge>
            </div>
            <div>
              {list.data.items.map((binding: CertificateBindingMetadata) => (
                <button
                  type="button"
                  key={binding.id}
                  className={
                    binding.id === selectedBindingId
                      ? "runtime-secret-binding runtime-secret-binding--active"
                      : "runtime-secret-binding"
                  }
                  onClick={() => setSelectedBindingId(binding.id)}
                >
                  <span>
                    <strong>{binding.name}</strong>
                    <small>Custom TLS certificate</small>
                  </span>
                  <span>
                    <StatusPill value={binding.state} />
                    <small>
                      {binding.activeVersion
                        ? `v${binding.activeVersion}`
                        : "No active version"}
                    </small>
                  </span>
                </button>
              ))}
            </div>
          </Card>
          <div>
            {detail.error ? (
              <ErrorPanel
                error={detail.error}
                title="Could not load certificate attestation"
                onRetry={() => void detail.refetch()}
              />
            ) : detail.isPending && selectedBindingId ? (
              <Card>
                <Skeleton lines={7} />
              </Card>
            ) : detail.data ? (
              <CertificateDetail
                key={detail.data.id}
                binding={detail.data}
                canRotate={canRotate}
                canDelete={canDelete}
                onChanged={() => {
                  if (
                    formScopeRef.current !== formScope ||
                    selectedBindingRef.current !== detail.data?.id
                  ) {
                    return;
                  }
                  void refreshList();
                  void detail.refetch();
                }}
                onDeleted={() => {
                  if (
                    formScopeRef.current !== formScope ||
                    selectedBindingRef.current !== detail.data?.id
                  ) {
                    return;
                  }
                  setSelectedBindingId("");
                  void refreshList();
                }}
              />
            ) : (
              <EmptyState
                icon="route"
                title="Select a certificate"
                description="Choose safe metadata to inspect immutable public certificate attestations."
                compact
              />
            )}
          </div>
        </div>
      ) : list.data ? (
        <EmptyState
          icon="route"
          title="No custom certificates"
          description="This environment has no certificate metadata. PEM and private keys are never listed."
          action={
            canCreate ? (
              <Button type="button" onClick={() => setCreating(true)}>
                Add write-only certificate
              </Button>
            ) : (
              <PlaceholderBadge>Read-only</PlaceholderBadge>
            )
          }
        />
      ) : null}
    </div>
  );
}
