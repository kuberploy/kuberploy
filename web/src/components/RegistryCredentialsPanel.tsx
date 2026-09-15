import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, errorMessage } from "../api/client";
import type {
  Application,
  Capability,
  RegistryCredentialBindingDetail,
  RegistryCredentialBindingMetadata,
  Environment,
  Project,
} from "../api/types";
import {
  registryCredentialEnvironments,
  hasRegistryCredentialCapability,
} from "../lib/registryCredentialAccess";
import { formatDate } from "../lib/format";
import { writeOnlyRequestSignature } from "../lib/writeOnlyRequest";
import { Icon } from "./Icon";
import {
  Select,
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  Notice,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
} from "./ui";

const registryCredentialNamePattern = /^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/;
const hostMaxBytes = 253;
const usernameMaxBytes = 256;
const passwordMaxBytes = 8_192;
const emailMaxBytes = 254;

type RegistryCredentialWritePayload = {
  host: string;
  username: string;
  password: string;
  email?: string;
};

type RegistryCredentialWriteCollection =
  | { ok: true; payload: RegistryCredentialWritePayload }
  | { ok: false; error: string };

function collectRegistryCredentialWritePayload(
  form: HTMLFormElement,
): RegistryCredentialWriteCollection {
  const host = form.elements.namedItem("host");
  const username = form.elements.namedItem("username");
  const password = form.elements.namedItem("password");
  const email = form.elements.namedItem("email");
  if (
    !(host instanceof HTMLInputElement) ||
    !(username instanceof HTMLInputElement) ||
    !(password instanceof HTMLInputElement) ||
    !(email instanceof HTMLInputElement)
  ) {
    return {
      ok: false,
      error: "Registry credential form fields are unavailable.",
    };
  }
  const encoder = new TextEncoder();
  const hostBytes = encoder.encode(host.value).byteLength;
  const usernameBytes = encoder.encode(username.value).byteLength;
  const passwordBytes = encoder.encode(password.value).byteLength;
  const emailBytes = encoder.encode(email.value).byteLength;
  if (hostBytes < 1 || hostBytes > hostMaxBytes) {
    return { ok: false, error: "Registry host must contain 1–253 UTF-8 bytes." };
  }
  if (usernameBytes < 1 || usernameBytes > usernameMaxBytes) {
    return { ok: false, error: "Username must contain 1–256 UTF-8 bytes." };
  }
  if (passwordBytes < 1 || passwordBytes > passwordMaxBytes) {
    return { ok: false, error: "Password must contain 1–8,192 UTF-8 bytes." };
  }
  if (emailBytes > emailMaxBytes) {
    return { ok: false, error: "Email must contain at most 254 UTF-8 bytes." };
  }
  return {
    ok: true,
    payload: {
      host: host.value,
      username: username.value,
      password: password.value,
      ...(email.value ? { email: email.value } : {}),
    },
  };
}

function clearRegistryCredentialFields(form: HTMLFormElement) {
  form
    .querySelectorAll<HTMLInputElement>("[data-registry-credential-secret]")
    .forEach((input) => {
      input.value = "";
    });
}

function destroyRegistryCredentialWritePayload(
  payload?: RegistryCredentialWritePayload,
) {
  if (!payload) return;
  payload.host = "";
  payload.username = "";
  payload.password = "";
  payload.email = "";
}

function RegistryCredentialFields({ prefix }: { prefix: string }) {
  return (
    <div className="grid gap-5 [&_+_[data-slot='notice']]:mt-4">
      <Notice tone="warning" role="status">
        <Icon name="route" />
        <div>
          <strong>Write-only registry credential material</strong>
          <p>
            The password is read only for this request, sealed immediately,
            cleared after submission, and never placed in query cache,
            browser storage, URLs, logs, or response state.
          </p>
        </div>
      </Notice>
      <Field label="Registry host" required>
        <input
          aria-label={`${prefix} registry host`}
          name="host"
          data-registry-credential-secret
          maxLength={hostMaxBytes}
          autoComplete="off"
          spellCheck={false}
          placeholder="registry.example.com"
        />
      </Field>
      <Field label="Username" required>
        <input
          aria-label={`${prefix} username`}
          name="username"
          data-registry-credential-secret
          maxLength={usernameMaxBytes}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>
      <Field label="Password or token" required>
        <input
          aria-label={`${prefix} password`}
          name="password"
          type="password"
          data-registry-credential-secret
          maxLength={passwordMaxBytes}
          autoComplete="new-password"
          spellCheck={false}
        />
      </Field>
      <Field label="Email (optional)">
        <input
          aria-label={`${prefix} email`}
          name="email"
          type="email"
          data-registry-credential-secret
          maxLength={emailMaxBytes}
          autoComplete="off"
          spellCheck={false}
        />
      </Field>
    </div>
  );
}

function CreateRegistryCredentialForm({
  application,
  environment,
  onCreated,
  onClose,
}: {
  application: Application;
  environment: Environment;
  onCreated: (binding: RegistryCredentialBindingDetail) => void;
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
    if (!registryCredentialNamePattern.test(name)) {
      setFeedback(
        "Name must be a lowercase DNS label containing at most 63 characters.",
      );
      return;
    }
    const collected = collectRegistryCredentialWritePayload(form);
    if (!collected.ok) {
      setFeedback(collected.error);
      clearRegistryCredentialFields(form);
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
      destroyRegistryCredentialWritePayload(input);
      clearRegistryCredentialFields(form);
      submitBusyRef.current = false;
      setBusy(false);
      setFeedback(
        "The write-only registry credential request could not be prepared. Try again.",
      );
      return;
    }
    const idempotencyKey =
      retryKey && retrySignature === signature ? retryKey : crypto.randomUUID();
    if (!retryKey || retrySignature !== signature) setRetryKey(idempotencyKey);
    clearRegistryCredentialFields(form);
    setFeedback("");
    try {
      const created = await api.createRegistryCredentialBinding(
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
        "The write-only registry credential request failed. The password was cleared; re-enter the exact same material to retry with the protected idempotency key.",
      );
    } finally {
      destroyRegistryCredentialWritePayload(input);
      submitBusyRef.current = false;
      setBusy(false);
    }
  }

  return (
    <Card className="p-0 [&>form]:p-5">
      <CardHeader bar>
        <div>
          <Eyebrow>{environment.name}</Eyebrow>
          <h3>New registry pull credential</h3>
          <p>
            The platform seals the password and derives its target Kubernetes
            image-pull Secret identity.
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
      </CardHeader>
      <form onSubmit={(event) => void submit(event)}>
        <fieldset disabled={busy}>
          <Field label="Credential name" required>
            <input
              aria-label="Registry credential binding name"
              name="name"
              autoComplete="off"
              spellCheck={false}
              maxLength={63}
              placeholder="private-registry"
            />
          </Field>
          <RegistryCredentialFields prefix="Create credential" />
          {feedback ? (
            <Notice tone="error" role="alert">
              {feedback}
            </Notice>
          ) : null}
          {retryKey ? (
            <small className="mt-4 flex items-center gap-2 text-ink-soft text-xs">
              A stable idempotency key is retained for this form retry.
              Re-enter the exact same credential or cancel and start a new
              request.
            </small>
          ) : null}
          <div className="flex justify-end mt-4">
            <Button type="submit" busy={busy}>
              Validate and seal
            </Button>
          </div>
        </fieldset>
      </form>
    </Card>
  );
}

function RegistryCredentialDetail({
  binding,
  canRotate,
  canDelete,
  onChanged,
  onDeleted,
}: {
  binding: RegistryCredentialBindingDetail;
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
    // A failed retry is tied to the observed credential CAS version. Do not
    // replay it after another observation advances the active version.
    setRotateRetryKey("");
    setRotateRetrySignature("");
    setRotateFeedback("");
  }, [binding.id, binding.activeVersion]);

  async function rotate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (rotateBusy || rotateBusyRef.current || !binding.activeVersion) return;
    const form = event.currentTarget;
    const collected = collectRegistryCredentialWritePayload(form);
    if (!collected.ok) {
      setRotateFeedback(collected.error);
      clearRegistryCredentialFields(form);
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
      destroyRegistryCredentialWritePayload(input);
      clearRegistryCredentialFields(form);
      rotateBusyRef.current = false;
      setRotateBusy(false);
      setRotateFeedback(
        "The write-only registry credential rotation could not be prepared. Try again.",
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
    clearRegistryCredentialFields(form);
    setRotateFeedback("");
    try {
      await api.rotateRegistryCredentialBinding(
        binding.id,
        input,
        idempotencyKey,
      );
      setRotateRetryKey("");
      setRotateRetrySignature("");
      onChanged();
    } catch (error) {
      void error;
      setRotateRetrySignature(signature);
      setRotateFeedback(
        "The write-only registry credential rotation failed. The password was cleared; re-enter the exact same material to retry with the protected idempotency key.",
      );
    } finally {
      destroyRegistryCredentialWritePayload(input);
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
      await api.deleteRegistryCredentialBinding(binding.id, idempotencyKey);
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
    <Card className="p-0">
      <CardHeader bar>
        <div>
          <Eyebrow>Public registry credential metadata</Eyebrow>
          <h3>{binding.name}</h3>
          <p>
            Binding <code>{binding.id}</code>
          </p>
        </div>
        <StatusPill value={binding.state} />
      </CardHeader>
      <div className="grid grid-cols-[1.4fr_0.7fr_0.9fr] gap-px border-y border-y-line bg-line [&>div]:min-w-0 [&>div]:py-3 [&>div]:px-4 [&>div]:bg-surface-soft [&_span]:block [&_span]:mb-1.5 [&_span]:text-ink-faint [&_span]:text-[11px] [&_span]:font-semibold [&_span]:tracking-[0.06em] [&_span]:uppercase [&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-xs [&_strong]:text-ellipsis [&_code]:block [&_code]:overflow-hidden [&_code]:text-xs [&_code]:text-ellipsis to-580:grid-cols-[1fr]">
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
      <section className="p-5">
        <div className="flex items-start justify-between gap-4 mb-3 [&_h4]:m-0 [&_h4]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-xs [&_p]:leading-[1.55]">
          <div>
            <h4>Public attestations</h4>
            <p>The password is never returned.</p>
          </div>
        </div>
        <div className="grid gap-2 [&_article]:py-3 [&_article]:px-4 [&_article]:border [&_article]:border-line [&_article]:rounded-lg [&_article]:bg-surface-soft [&_article_>_div]:flex [&_article_>_div]:items-center [&_article_>_div]:justify-between [&_article_>_div]:gap-3 [&_strong]:text-meta [&_small]:block [&_small]:mt-1 [&_small]:text-ink-faint [&_small]:text-[11px] [&_code]:text-mint-dark [&_code]:text-[11px] [&_code]:break-words">
          {binding.versions.map((version) => (
            <article key={version.number}>
              <div>
                <strong>Version {version.number}</strong>
                <small>{formatDate(version.createdAt)}</small>
              </div>
              <small>
                <code>{version.username}</code> @ <code>{version.host}</code>
              </small>
            </article>
          ))}
        </div>
      </section>
      {canRotate && binding.state === "ready" && binding.activeVersion ? (
        <form
          className="p-5 border-t border-t-line"
          onSubmit={(event) => void rotate(event)}
        >
          <fieldset disabled={rotateBusy}>
            <div className="flex items-start justify-between gap-4 mb-3 [&_h4]:m-0 [&_h4]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-xs [&_p]:leading-[1.55]">
              <div>
                <h4>Rotate from version {binding.activeVersion}</h4>
                <p>
                  The active version is used as an exact compare-and-swap
                  guard.
                </p>
              </div>
            </div>
            <RegistryCredentialFields prefix="Rotate credential" />
            {rotateFeedback ? (
              <Notice tone="error" role="alert">
                {rotateFeedback}
              </Notice>
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
          className="p-5 border-t border-t-line bg-tone-bad-surface [&_h4]:m-0 [&_h4]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-xs [&_p]:leading-[1.55]"
          onSubmit={(event) => void remove(event)}
        >
          <fieldset
            className="grid grid-cols-[minmax(240px,_1fr)_minmax(190px,_0.7fr)_auto] items-end gap-3 [&_[data-slot='notice']]:col-[1_/_-1] [&>[data-slot='placeholder-badge']]:col-[1_/_-1] to-820:grid-cols-[1fr_auto] to-820:[&>div:first-child]:col-[1_/_-1] to-580:grid-cols-[1fr] to-580:[&>div:first-child]:row-[auto] to-580:[&>div:first-child]:col-[auto]"
            disabled={deleteBusy}
          >
            <div>
              <h4>Delete registry credential binding</h4>
              <p>
                Deletion fails while any App still selects this credential or
                a retained reference remains.
              </p>
            </div>
            <Field label={`Type ${binding.name} to confirm`}>
              <input
                aria-label="Exact registry credential binding name confirmation"
                name="confirmation"
                autoComplete="off"
                spellCheck={false}
              />
            </Field>
            <Button type="submit" variant="danger" busy={deleteBusy}>
              Delete credential
            </Button>
            {deleteFeedback ? (
              <Notice tone="error" role="alert">
                {deleteFeedback}
              </Notice>
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

export function RegistryCredentialsPanel({
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
  // The picked environment is a preference; the environment actually selected
  // is derived below from the environments readable in this render.
  const [environmentChoice, setSelectedEnvironmentId] = useState("");
  const [selectedBindingId, setSelectedBindingId] = useState("");
  const [creatingChoice, setCreating] = useState(false);
  const formScopeRef = useRef("");
  const selectedBindingRef = useRef("");
  selectedBindingRef.current = selectedBindingId;
  const readableEnvironments = registryCredentialEnvironments(
    capabilities,
    "registry-credential-bindings:read",
    application,
    environments,
    project,
  );
  const selectedEnvironmentId = readableEnvironments.some(
    (environment) => environment.id === environmentChoice,
  )
    ? environmentChoice
    : "";
  const selectedEnvironment =
    readableEnvironments.find(
      (environment) => environment.id === selectedEnvironmentId,
    ) ?? readableEnvironments[0];
  // The create form belongs to an environment; with no readable environment
  // left there is nothing to create against.
  const creating = creatingChoice && Boolean(selectedEnvironment);
  const formScope = `${application.id}:${selectedEnvironment?.id ?? ""}`;
  formScopeRef.current = formScope;
  useEffect(() => {
    formScopeRef.current = formScope;
  }, [formScope]);
  const list = useQuery({
    queryKey: [
      "registry-credential-bindings",
      application.id,
      selectedEnvironment?.id,
    ],
    queryFn: () =>
      api.registryCredentialBindings(application.id, selectedEnvironment!.id),
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
      "registry-credential-binding",
      selectedBindingId,
      selectedListedBinding?.activeVersion ?? 0,
      selectedListedBinding?.state ?? "",
    ],
    queryFn: () => api.registryCredentialBinding(selectedBindingId),
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
        description="Registry credential metadata and mutations are intentionally excluded from service-account and agent sessions."
      />
    );
  }
  if (!selectedEnvironment) {
    return (
      <EmptyState
        icon="route"
        title="Registry credential access not granted"
        description="An exact registry-credential-bindings:read capability covering an App Environment is required."
      />
    );
  }

  const canCreate = hasRegistryCredentialCapability(
    capabilities,
    "registry-credential-bindings:create",
    application,
    selectedEnvironment,
    project,
  );
  const canRotate = hasRegistryCredentialCapability(
    capabilities,
    "registry-credential-bindings:rotate",
    application,
    selectedEnvironment,
    project,
  );
  const canDelete = hasRegistryCredentialCapability(
    capabilities,
    "registry-credential-bindings:delete",
    application,
    selectedEnvironment,
    project,
  );

  async function refreshList() {
    await queryClient.invalidateQueries({
      queryKey: [
        "registry-credential-bindings",
        application.id,
        selectedEnvironment.id,
      ],
    });
  }

  return (
    <div className="grid gap-4">
      <Card className="grid grid-cols-[minmax(220px,_1fr)_minmax(230px,_330px)_auto] items-end gap-5 py-4 px-5 [&>div:first-child]:flex [&>div:first-child]:items-center [&>div:first-child]:gap-3 [&>div:first-child]:self-center [&_strong]:block [&_strong]:text-xs [&_small]:block [&_small]:mt-1 [&_small]:text-ink-faint [&_small]:text-xs [&_.field]:gap-1.5 to-820:grid-cols-[1fr_1fr] to-820:[&>div:first-child]:col-[1_/_-1] to-580:grid-cols-[1fr] to-580:[&>div:first-child]:row-[auto] to-580:[&>div:first-child]:col-[auto]">
        <div>
          <span className="grid w-9 h-9 place-items-center rounded-[9px] text-mint-dark bg-mint-soft [&_svg]:w-[17px]">
            <Icon name="route" />
          </span>
          <span>
            <strong>Registry pull credentials</strong>
            <small>Host and username are readable; the password is write-only.</small>
          </span>
        </div>
        <Field label="App Environment">
          <Select
            aria-label="Registry credential environment"
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
          </Select>
        </Field>
        {canCreate ? (
          <Button
            type="button"
            disabled={Boolean(list.error)}
            onClick={() => setCreating((value) => !value)}
          >
            <Icon name="plus" /> New credential
          </Button>
        ) : null}
      </Card>

      {creating && canCreate && !list.error ? (
        <CreateRegistryCredentialForm
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
          title="Could not load registry credential metadata"
          onRetry={() => void list.refetch()}
        />
      ) : list.isPending ? (
        <Card>
          <Skeleton lines={6} />
        </Card>
      ) : list.data?.items.length ? (
        <div className="grid grid-cols-[minmax(250px,_0.7fr)_minmax(420px,_1.3fr)] items-start gap-4 to-1120:grid-cols-[1fr]">
          <Card className="overflow-hidden p-0 [&>div:last-child]:grid">
            <CardHeader bar>
              <div>
                <Eyebrow>Environment metadata</Eyebrow>
                <h3>Registry credential bindings</h3>
              </div>
              <PlaceholderBadge>
                {list.data.items.length} credentials
              </PlaceholderBadge>
            </CardHeader>
            <div>
              {list.data.items.map(
                (binding: RegistryCredentialBindingMetadata) => (
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
                      <small>Registry pull credential</small>
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
                ),
              )}
            </div>
          </Card>
          <div>
            {detail.error ? (
              <ErrorPanel
                error={detail.error}
                title="Could not load registry credential attestation"
                onRetry={() => void detail.refetch()}
              />
            ) : detail.isPending && selectedBindingId ? (
              <Card>
                <Skeleton lines={7} />
              </Card>
            ) : detail.data ? (
              <RegistryCredentialDetail
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
                title="Select a registry credential"
                description="Choose safe metadata to inspect public registry credential attestations."
                compact
              />
            )}
          </div>
        </div>
      ) : list.data && !creating ? (
        <EmptyState
          icon="route"
          title="No registry pull credentials"
          description="This environment has no registry credential metadata. The password is never listed."
          action={
            canCreate ? (
              <Button type="button" onClick={() => setCreating(true)}>
                Add write-only credential
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
