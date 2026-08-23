import { useEffect } from "react";
import { useFieldArray, useForm, useWatch } from "react-hook-form";
import {
  validateGuidedProbes,
  validateGuidedResourceOverrides,
  validateGuidedRuntimeProcess,
  type GuidedConfig,
  type GuidedPort,
  type GuidedProbe,
  type GuidedProbes,
  type GuidedRuntimeProcess,
} from "../lib/configDraft";
import { Button, Field } from "./ui";
import { Icon } from "./Icon";
import type { ExternalDNSCatalog, SSLIPHostnamePreview } from "../api/types";
import { externalDNSHostnameAllowed } from "../lib/externalDNSAccess";
import { TraefikMiddlewareEditor } from "./TraefikMiddlewareEditor";
import { RuntimeSecretReferencePicker } from "./RuntimeSecretReferencePicker";
import { CertificateReferencePicker } from "./CertificateReferencePicker";
import { CertificateIssuerPicker } from "./CertificateIssuerPicker";
import {
  SchedulingEditor,
  type SchedulingEditorValue,
} from "./SchedulingEditor";

const probePhases = [
  {
    key: "startup",
    label: "Startup",
    description: "Protect slow-starting containers before other checks run.",
  },
  {
    key: "readiness",
    label: "Readiness",
    description: "Remove an unready Pod from Service traffic.",
  },
  {
    key: "liveness",
    label: "Liveness",
    description: "Restart a container that can no longer recover.",
  },
] as const;

function optionalInputNumber(value: string): number | undefined {
  if (value === "") return undefined;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

export function RuntimeProcessEditor({
  value,
  onChange,
}: {
  value: GuidedRuntimeProcess;
  onChange: (value: GuidedRuntimeProcess) => void;
}) {
  const validationError = validateGuidedRuntimeProcess(value);
  return (
    <div className="runtime-process-editor">
      {validationError ? (
        <div className="yaml-diagnostic" role="alert">
          {validationError}
        </div>
      ) : null}
      <div className="runtime-process-grid">
        <Field
          label="Working directory"
          hint="Optional absolute container path, for example /app."
        >
          <input
            aria-label="Container working directory"
            placeholder="/app"
            value={value.workingDirectory ?? ""}
            onChange={(event) =>
              onChange({ ...value, workingDirectory: event.target.value })
            }
          />
        </Field>
        <Field
          label="Container command (YAML list)"
          hint="Up to 64 exact argv entries overriding image ENTRYPOINT; [] keeps the default."
        >
          <textarea
            aria-label="Container command (YAML list)"
            rows={5}
            maxLength={262_144}
            spellCheck={false}
            value={value.commandYaml}
            onChange={(event) =>
              onChange({ ...value, commandYaml: event.target.value })
            }
          />
        </Field>
        <Field
          label="Container arguments (YAML list)"
          hint="Up to 128 exact argv entries overriding image CMD; [] keeps the default."
        >
          <textarea
            aria-label="Container arguments (YAML list)"
            rows={5}
            maxLength={262_144}
            spellCheck={false}
            value={value.argsYaml}
            onChange={(event) =>
              onChange({ ...value, argsYaml: event.target.value })
            }
          />
        </Field>
        <Field
          label="Termination grace period"
          hint="Optional integer from 1 to 3600 seconds."
        >
          <input
            aria-label="Termination grace period"
            type="number"
            min={1}
            max={3600}
            step={1}
            value={value.terminationGracePeriodSeconds ?? ""}
            onChange={(event) =>
              onChange({
                ...value,
                terminationGracePeriodSeconds: optionalInputNumber(
                  event.target.value,
                ),
              })
            }
          />
        </Field>
      </div>
      <p className="runtime-process-editor__hint">
        Each YAML item is one literal container argument. Values are never split
        or interpreted as a shell command; command and arguments share a 65536
        byte limit.
      </p>
    </div>
  );
}

export function HealthProbeEditor({
  value,
  onChange,
  configuredPorts,
}: {
  value: GuidedProbes;
  onChange: (value: GuidedProbes) => void;
  configuredPorts?: GuidedPort[];
}) {
  const validationError = validateGuidedProbes(value, configuredPorts);
  const update = (phase: keyof GuidedProbes, change: Partial<GuidedProbe>) =>
    onChange({
      ...value,
      [phase]: { ...value[phase], ...change },
    });

  return (
    <div className="health-probe-editor">
      {validationError ? (
        <div className="yaml-diagnostic" role="alert">
          {validationError}
        </div>
      ) : null}
      <div className="health-probe-grid">
        {probePhases.map(({ key, label, description }) => {
          const probe = value[key];
          return (
            <fieldset className="health-probe-card" key={key}>
              <legend>{label}</legend>
              <p>{description}</p>
              <Field label={`${label} check`}>
                <select
                  aria-label={`${label} check`}
                  value={probe.mode}
                  onChange={(event) =>
                    update(key, {
                      mode: event.target.value as GuidedProbe["mode"],
                    })
                  }
                >
                  <option value="disabled">Disabled</option>
                  <option value="httpGet">HTTP request</option>
                  <option value="tcpSocket">TCP connection</option>
                  <option value="exec">Exec command</option>
                </select>
              </Field>
              {probe.mode === "httpGet" ? (
                <div className="health-probe-action">
                  <Field label={`${label} HTTP path`}>
                    <input
                      aria-label={`${label} HTTP path`}
                      required
                      maxLength={2048}
                      value={probe.httpPath}
                      onChange={(event) =>
                        update(key, { httpPath: event.target.value })
                      }
                    />
                  </Field>
                  <Field
                    label={`${label} port`}
                    hint="Configured TCP port name or number."
                  >
                    <input
                      aria-label={`${label} port`}
                      required
                      maxLength={15}
                      value={probe.port}
                      onChange={(event) =>
                        update(key, { port: event.target.value })
                      }
                    />
                  </Field>
                  <Field label={`${label} HTTP scheme`}>
                    <select
                      aria-label={`${label} HTTP scheme`}
                      value={probe.httpScheme}
                      onChange={(event) =>
                        update(key, {
                          httpScheme: event.target
                            .value as GuidedProbe["httpScheme"],
                        })
                      }
                    >
                      <option value="">Default (HTTP)</option>
                      <option value="HTTP">HTTP</option>
                      <option value="HTTPS">HTTPS</option>
                    </select>
                  </Field>
                </div>
              ) : null}
              {probe.mode === "tcpSocket" ? (
                <div className="health-probe-action">
                  <Field
                    label={`${label} port`}
                    hint="Configured TCP port name or number."
                  >
                    <input
                      aria-label={`${label} port`}
                      required
                      maxLength={15}
                      value={probe.port}
                      onChange={(event) =>
                        update(key, { port: event.target.value })
                      }
                    />
                  </Field>
                </div>
              ) : null}
              {probe.mode === "exec" ? (
                <div className="health-probe-action">
                  <Field
                    label={`${label} exec arguments (YAML list)`}
                    hint="One exact command argument per YAML item; shell parsing is never implied."
                  >
                    <textarea
                      aria-label={`${label} exec arguments (YAML list)`}
                      rows={5}
                      spellCheck={false}
                      value={probe.execCommandYaml}
                      onChange={(event) =>
                        update(key, { execCommandYaml: event.target.value })
                      }
                    />
                  </Field>
                </div>
              ) : null}
              {probe.mode !== "disabled" ? (
                <div className="health-probe-timing">
                  <Field label={`${label} initial delay`} hint="0–3600 s">
                    <input
                      aria-label={`${label} initial delay`}
                      type="number"
                      min={0}
                      max={3600}
                      value={probe.initialDelaySeconds ?? ""}
                      onChange={(event) =>
                        update(key, {
                          initialDelaySeconds: optionalInputNumber(
                            event.target.value,
                          ),
                        })
                      }
                    />
                  </Field>
                  <Field label={`${label} period`} hint="1–300 s">
                    <input
                      aria-label={`${label} period`}
                      type="number"
                      min={1}
                      max={300}
                      value={probe.periodSeconds ?? ""}
                      onChange={(event) =>
                        update(key, {
                          periodSeconds: optionalInputNumber(
                            event.target.value,
                          ),
                        })
                      }
                    />
                  </Field>
                  <Field label={`${label} timeout`} hint="1–300 s">
                    <input
                      aria-label={`${label} timeout`}
                      type="number"
                      min={1}
                      max={300}
                      value={probe.timeoutSeconds ?? ""}
                      onChange={(event) =>
                        update(key, {
                          timeoutSeconds: optionalInputNumber(
                            event.target.value,
                          ),
                        })
                      }
                    />
                  </Field>
                  <Field
                    label={`${label} success threshold`}
                    hint={key === "readiness" ? "1–100" : "Must be 1"}
                  >
                    <input
                      aria-label={`${label} success threshold`}
                      type="number"
                      min={1}
                      max={key === "readiness" ? 100 : 1}
                      value={probe.successThreshold ?? ""}
                      onChange={(event) =>
                        update(key, {
                          successThreshold: optionalInputNumber(
                            event.target.value,
                          ),
                        })
                      }
                    />
                  </Field>
                  <Field label={`${label} failure threshold`} hint="1–100">
                    <input
                      aria-label={`${label} failure threshold`}
                      type="number"
                      min={1}
                      max={100}
                      value={probe.failureThreshold ?? ""}
                      onChange={(event) =>
                        update(key, {
                          failureThreshold: optionalInputNumber(
                            event.target.value,
                          ),
                        })
                      }
                    />
                  </Field>
                </div>
              ) : (
                <div className="inline-empty">
                  No {label.toLowerCase()} check.
                </div>
              )}
            </fieldset>
          );
        })}
      </div>
      <p className="health-probe-editor__hint">
        Empty timing fields use Kubernetes defaults. Every port must match a
        configured TCP container port.
      </p>
    </div>
  );
}

export function GuidedConfigForm({
  initial,
  onChange,
  externalDNSCatalog,
  externalDNSCatalogPending = false,
  externalDNSCatalogError,
  externalDNSRuntimeEnabled = false,
  runtimeSecretApplicationId,
  runtimeSecretEnvironmentId,
  runtimeSecretReferencesEnabled = false,
  runtimeSecretReferencesUnavailableReason,
  certificateReferencesEnabled = false,
  certificateReferencesUnavailableReason,
  certificateIssuersEnabled = false,
  certificateIssuersUnavailableReason,
  sslipHostnameEnabled = false,
  sslipHostnamePreview,
  sslipHostnamePending = false,
  sslipHostnameError,
  readOnly = false,
  middlewareEditingUnavailableReason,
  reusableMiddlewareProfilesEnabled = false,
}: {
  initial: GuidedConfig;
  onChange: (value: GuidedConfig) => void;
  externalDNSCatalog?: ExternalDNSCatalog;
  externalDNSCatalogPending?: boolean;
  externalDNSCatalogError?: string;
  externalDNSRuntimeEnabled?: boolean;
  runtimeSecretApplicationId?: string;
  runtimeSecretEnvironmentId?: string;
  runtimeSecretReferencesEnabled?: boolean;
  runtimeSecretReferencesUnavailableReason?: string;
  certificateReferencesEnabled?: boolean;
  certificateReferencesUnavailableReason?: string;
  certificateIssuersEnabled?: boolean;
  certificateIssuersUnavailableReason?: string;
  sslipHostnameEnabled?: boolean;
  sslipHostnamePreview?: SSLIPHostnamePreview;
  sslipHostnamePending?: boolean;
  sslipHostnameError?: string;
  readOnly?: boolean;
  middlewareEditingUnavailableReason?: string;
  reusableMiddlewareProfilesEnabled?: boolean;
}) {
  const form = useForm<GuidedConfig>({ defaultValues: initial });
  const ports = useFieldArray({ control: form.control, name: "ports" });
  const variables = useFieldArray({ control: form.control, name: "variables" });
  const secretVariables = useFieldArray({
    control: form.control,
    name: "secretVariables",
  });
  const secretVariableValues = useWatch({
    control: form.control,
    name: "secretVariables",
  });
  const workloadType = useWatch({
    control: form.control,
    name: "workloadType",
  });
  const tlsMode = useWatch({ control: form.control, name: "tlsMode" });
  const issuerRef = useWatch({ control: form.control, name: "issuerRef" });
  const dnsMode = useWatch({ control: form.control, name: "dnsMode" });
  const host = useWatch({ control: form.control, name: "host" });
  const certificateRef = useWatch({
    control: form.control,
    name: "certificateRef",
  });
  const dnsIntegrationRef = useWatch({
    control: form.control,
    name: "dnsIntegrationRef",
  });
  const probes = useWatch({ control: form.control, name: "probes" });
  const commandYaml = useWatch({ control: form.control, name: "commandYaml" });
  const argsYaml = useWatch({ control: form.control, name: "argsYaml" });
  const terminationGracePeriodSeconds = useWatch({
    control: form.control,
    name: "terminationGracePeriodSeconds",
  });
  const configuredPorts = useWatch({ control: form.control, name: "ports" });
  const scheduling = useWatch({
    control: form.control,
    name: [
      "nodeSelectorYaml",
      "affinityYaml",
      "topologySpreadYaml",
      "tolerationsYaml",
      "priorityClassName",
    ],
  });
  const middlewares = useWatch({
    control: form.control,
    name: "middlewares",
  });
  const middlewareRefs = useWatch({
    control: form.control,
    name: "middlewareRefs",
  });
  const resourceOverrides = useWatch({
    control: form.control,
    name: "resourceOverrides",
  });
  const resourceOverrideError = validateGuidedResourceOverrides(
    resourceOverrides ?? initial.resourceOverrides,
  );
  const commit = () => queueMicrotask(() => onChange(form.getValues()));
  const updateScheduling = (value: SchedulingEditorValue) => {
    form.setValue("nodeSelectorYaml", value.nodeSelectorYaml, {
      shouldDirty: true,
    });
    form.setValue("affinityYaml", value.affinityYaml, { shouldDirty: true });
    form.setValue("topologySpreadYaml", value.topologySpreadYaml, {
      shouldDirty: true,
    });
    form.setValue("tolerationsYaml", value.tolerationsYaml, {
      shouldDirty: true,
    });
    form.setValue("priorityClassName", value.priorityClassName, {
      shouldDirty: true,
    });
    commit();
  };
  useEffect(() => {
    if (
      readOnly ||
      dnsMode !== "sslip" ||
      !sslipHostnamePreview?.hostname ||
      form.getValues("host") === sslipHostnamePreview.hostname
    ) {
      return;
    }
    form.setValue("host", sslipHostnamePreview.hostname, {
      shouldDirty: true,
    });
    form.setValue("dnsIntegrationRef", "", { shouldDirty: true });
    commit();
  }, [dnsMode, form, readOnly, sslipHostnamePreview?.hostname]);
  const dnsIntegrations = externalDNSCatalog?.items ?? [];
  const selectedDNSIntegration = dnsIntegrations.find(
    (integration) => integration.slug === dnsIntegrationRef,
  );
  const automaticDNSRuntimeReady =
    externalDNSRuntimeEnabled &&
    externalDNSCatalog?.runtimeAvailable === true &&
    dnsIntegrations.some(
      (integration) => integration.runtimeAvailable === true,
    );
  const selectedDNSIntegrationReady =
    automaticDNSRuntimeReady &&
    selectedDNSIntegration?.runtimeAvailable === true;
  const dnsIntegrationError =
    dnsMode !== "externalDns" || externalDNSCatalogPending
      ? undefined
      : externalDNSCatalogError
        ? externalDNSCatalogError
        : !selectedDNSIntegration
          ? "Select an integration authorized for this application and environment."
          : selectedDNSIntegration.runtimeAvailable !== true
            ? "The selected External DNS integration revision is not freshly observed ready."
            : host &&
                !externalDNSHostnameAllowed(
                  host,
                  selectedDNSIntegration.allowedDomainSuffixes,
                )
              ? `Hostname must be inside: ${selectedDNSIntegration.allowedDomainSuffixes.join(", ")}.`
              : undefined;
  const automaticDNSUnavailable =
    !externalDNSCatalogPending &&
    (!automaticDNSRuntimeReady ||
      Boolean(externalDNSCatalogError) ||
      dnsIntegrations.length === 0);

  return (
    <fieldset className="guided-config" disabled={readOnly}>
      <legend className="sr-only">Guided application configuration</legend>
      <section className="config-section">
        <div className="config-section__heading">
          <span className="config-section__icon">
            <Icon name="apps" />
          </span>
          <div>
            <h3>Runtime</h3>
            <p>Common Deployment and Service controls.</p>
          </div>
        </div>
        <div className="form-grid form-grid--three">
          <Field label="Replicas">
            <input
              type="number"
              min={1}
              max={20}
              {...form.register("replicas", {
                valueAsNumber: true,
                onChange: commit,
              })}
            />
          </Field>
          <Field label="Workload type">
            <select
              {...form.register("workloadType", {
                onChange: (event) => {
                  const stateful = event.target.value === "StatefulSet";
                  form.setValue("strategyType", "RollingUpdate", {
                    shouldDirty: true,
                  });
                  if (stateful)
                    form.setValue("podManagementPolicy", "OrderedReady", {
                      shouldDirty: true,
                    });
                  commit();
                },
              })}
            >
              <option value="Deployment">Deployment</option>
              <option value="StatefulSet">StatefulSet</option>
            </select>
          </Field>
          <Field
            label={`${workloadType === "StatefulSet" ? "StatefulSet" : "Deployment"} strategy`}
            hint="Rolling update is the default. Other strategies replace Pods according to workload type."
          >
            <select {...form.register("strategyType", { onChange: commit })}>
              <option value="RollingUpdate">Rolling update</option>
              {workloadType === "StatefulSet" ? (
                <option value="OnDelete">On delete</option>
              ) : (
                <option value="Recreate">Recreate</option>
              )}
            </select>
          </Field>
          {workloadType === "StatefulSet" ? (
            <Field label="Pod management policy">
              <select
                {...form.register("podManagementPolicy", { onChange: commit })}
              >
                <option value="OrderedReady">Ordered ready</option>
                <option value="Parallel">Parallel</option>
              </select>
            </Field>
          ) : null}
          <Field label="CPU request" hint="Default 50m">
            <input
              placeholder="50m"
              {...form.register("cpuRequest", { onChange: commit })}
            />
          </Field>
          <Field label="Memory request" hint="Default 100Mi">
            <input
              placeholder="100Mi"
              {...form.register("memoryRequest", { onChange: commit })}
            />
          </Field>
          <Field label="CPU limit" hint="Optional; cannot be below request">
            <input
              placeholder="500m"
              {...form.register("cpuLimit", { onChange: commit })}
            />
          </Field>
          <Field label="Memory limit" hint="Optional; cannot be below request">
            <input
              placeholder="512Mi"
              {...form.register("memoryLimit", { onChange: commit })}
            />
          </Field>
        </div>
        <RuntimeProcessEditor
          value={{
            commandYaml: commandYaml ?? initial.commandYaml,
            argsYaml: argsYaml ?? initial.argsYaml,
            workingDirectory: form.getValues("workingDirectory"),
            terminationGracePeriodSeconds,
          }}
          onChange={(value) => {
            form.setValue("commandYaml", value.commandYaml, {
              shouldDirty: true,
            });
            form.setValue("argsYaml", value.argsYaml, { shouldDirty: true });
            form.setValue("workingDirectory", value.workingDirectory ?? "", {
              shouldDirty: true,
            });
            form.setValue(
              "terminationGracePeriodSeconds",
              value.terminationGracePeriodSeconds,
              { shouldDirty: true },
            );
            commit();
          }}
        />
        <div className="config-section__heading config-section__heading--action">
          <div>
            <h3>Ports</h3>
            <p>Named container and Service ports.</p>
          </div>
          <Button
            type="button"
            variant="secondary"
            onClick={() => {
              ports.append({
                name: `port-${ports.fields.length + 1}`,
                containerPort: 3000,
                protocol: "TCP",
              });
              commit();
            }}
          >
            <Icon name="plus" /> Add port
          </Button>
        </div>
        <div className="variable-list">
          {ports.fields.map((field, index) => (
            <div className="variable-row" key={field.id}>
              <Field label={index === 0 ? "Name" : ""}>
                <input
                  aria-label={`Port ${index + 1} name`}
                  {...form.register(`ports.${index}.name`, {
                    onChange: commit,
                  })}
                />
              </Field>
              <Field label={index === 0 ? "Container" : ""}>
                <input
                  aria-label={`Port ${index + 1} container port`}
                  type="number"
                  min={1}
                  max={65535}
                  {...form.register(`ports.${index}.containerPort`, {
                    valueAsNumber: true,
                    onChange: commit,
                  })}
                />
              </Field>
              <Field label={index === 0 ? "Service (optional)" : ""}>
                <input
                  aria-label={`Port ${index + 1} service port`}
                  type="number"
                  min={1}
                  max={65535}
                  {...form.register(`ports.${index}.servicePort`, {
                    setValueAs: (value) =>
                      value === "" ? undefined : Number(value),
                    onChange: commit,
                  })}
                />
              </Field>
              <Field label={index === 0 ? "Protocol" : ""}>
                <select
                  aria-label={`Port ${index + 1} protocol`}
                  {...form.register(`ports.${index}.protocol`, {
                    onChange: commit,
                  })}
                >
                  <option value="TCP">TCP</option>
                  <option value="UDP">UDP</option>
                </select>
              </Field>
              <button
                type="button"
                className="icon-button"
                disabled={ports.fields.length === 1}
                onClick={() => {
                  ports.remove(index);
                  commit();
                }}
                aria-label={`Remove port ${index + 1}`}
              >
                <Icon name="close" />
              </button>
            </div>
          ))}
        </div>
      </section>

      <section className="config-section">
        <div className="config-section__heading">
          <span className="config-section__icon">
            <Icon name="check" />
          </span>
          <div>
            <h3>Health checks</h3>
            <p>
              Startup, traffic readiness, and recovery checks rendered as
              Kubernetes probes.
            </p>
          </div>
        </div>
        <HealthProbeEditor
          value={probes ?? initial.probes}
          configuredPorts={configuredPorts ?? initial.ports}
          onChange={(value) => {
            form.setValue("probes", value, { shouldDirty: true });
            commit();
          }}
        />
      </section>

      <section className="config-section">
        <div className="config-section__heading config-section__heading--action">
          <span className="config-section__icon">
            <Icon name="terminal" />
          </span>
          <div>
            <h3>Runtime environment values</h3>
            <p>
              Used only by the deployed service. Visible in Git and rendered as
              explicit ConfigMap references; never passed to image builds.
            </p>
          </div>
          <Button
            type="button"
            variant="secondary"
            onClick={() => {
              variables.append({ name: "", value: "" });
              commit();
            }}
          >
            <Icon name="plus" /> Add
          </Button>
        </div>
        {variables.fields.length ? (
          <div className="variable-list">
            {variables.fields.map((field, index) => (
              <div className="variable-row" key={field.id}>
                <Field label={index === 0 ? "Name" : ""}>
                  <input
                    aria-label={`Config variable ${index + 1} name`}
                    placeholder="LOG_LEVEL"
                    {...form.register(`variables.${index}.name`, {
                      onChange: commit,
                    })}
                  />
                </Field>
                <Field label={index === 0 ? "Value" : ""}>
                  <input
                    aria-label={`Config variable ${index + 1} value`}
                    placeholder="info"
                    {...form.register(`variables.${index}.value`, {
                      onChange: commit,
                    })}
                  />
                </Field>
                <button
                  type="button"
                  className="icon-button"
                  onClick={() => {
                    variables.remove(index);
                    commit();
                  }}
                  aria-label={`Remove config variable ${index + 1}`}
                >
                  <Icon name="close" />
                </button>
              </div>
            ))}
          </div>
        ) : (
          <div className="inline-empty">
            No ordinary values. Sensitive values use an existing write-only
            secret binding.
          </div>
        )}
      </section>

      <section className="config-section">
        <div className="config-section__heading config-section__heading--action">
          <span className="config-section__icon">
            <Icon name="settings" />
          </span>
          <div>
            <h3>Secret references</h3>
            <p>
              Select an existing write-only binding and its exact active
              version. Secret plaintext is never part of this document.
            </p>
          </div>
          <Button
            type="button"
            variant="secondary"
            disabled={
              !runtimeSecretReferencesEnabled ||
              !runtimeSecretApplicationId ||
              !runtimeSecretEnvironmentId
            }
            onClick={() => {
              secretVariables.append({
                name: "",
                bindingId: "",
                bindingName: "",
                key: "",
                version: 0,
              });
              commit();
            }}
          >
            <Icon name="plus" /> Add reference
          </Button>
        </div>
        {secretVariables.fields.map((field, index) => (
          <div className="variable-row" key={field.id}>
            <Field label={index === 0 ? "Environment name" : ""}>
              <input
                aria-label={`Secret variable ${index + 1} name`}
                placeholder="DATABASE_PASSWORD"
                {...form.register(`secretVariables.${index}.name`, {
                  onChange: () => {
                    form.setValue(`secretVariables.${index}.key`, "", {
                      shouldDirty: true,
                    });
                    commit();
                  },
                })}
              />
            </Field>
            <RuntimeSecretReferencePicker
              index={index}
              applicationId={runtimeSecretApplicationId}
              environmentId={runtimeSecretEnvironmentId}
              environmentName={secretVariableValues?.[index]?.name ?? ""}
              value={{
                bindingId: secretVariableValues?.[index]?.bindingId ?? "",
                bindingName: secretVariableValues?.[index]?.bindingName ?? "",
                key: secretVariableValues?.[index]?.key ?? "",
                version: secretVariableValues?.[index]?.version ?? 0,
              }}
              enabled={runtimeSecretReferencesEnabled}
              readOnly={readOnly}
              unavailableReason={runtimeSecretReferencesUnavailableReason}
              onChange={(reference) => {
                form.setValue(
                  `secretVariables.${index}`,
                  {
                    name: form.getValues(`secretVariables.${index}.name`),
                    ...reference,
                  },
                  { shouldDirty: true },
                );
                commit();
              }}
            />
            <button
              type="button"
              className="icon-button"
              onClick={() => {
                secretVariables.remove(index);
                commit();
              }}
              aria-label={`Remove secret variable ${index + 1}`}
            >
              <Icon name="close" />
            </button>
          </div>
        ))}
      </section>

      <section className="config-section">
        <div className="config-section__heading config-section__heading--action">
          <span className="config-section__icon">
            <Icon name="layers" />
          </span>
          <div>
            <h3>Scheduling for this service</h3>
            <p>
              Choose how this app is placed on Karpenter or fixed node pools.
              Other services keep their own independent selection.
            </p>
          </div>
        </div>
        <SchedulingEditor
          value={{
            nodeSelectorYaml: scheduling[0] ?? "{}",
            affinityYaml: scheduling[1] ?? "{}",
            topologySpreadYaml: scheduling[2] ?? "[]",
            tolerationsYaml: scheduling[3] ?? "[]",
            priorityClassName: scheduling[4] ?? "",
          }}
          applicationId={runtimeSecretApplicationId}
          disabled={readOnly}
          onChange={updateScheduling}
        />
        <p className="field-hint">
          Placement is stored directly with this app. Pod affinity,
          anti-affinity, and topology selectors may target only this exact
          application. Kuberploy never edits Nodes, taints, or provisioners.
        </p>
      </section>

      <section className="config-section">
        <div className="config-section__heading">
          <span className="config-section__icon">
            <Icon name="route" />
          </span>
          <div>
            <h3>Public route</h3>
            <p>
              Traefik exposure, TLS, DNS, and ordered middleware are one
              Git-backed route.
            </p>
          </div>
        </div>
        <div className="form-grid">
          <Field
            label="Hostname"
            hint={
              dnsMode === "sslip"
                ? "Read-only server-derived sslip.io hostname. No caller IP or free-form sslip hostname is accepted."
                : "Leave empty for an internal-only Service."
            }
          >
            <input
              aria-label="Hostname"
              placeholder="hello.example.com"
              readOnly={dnsMode === "sslip"}
              aria-readonly={dnsMode === "sslip"}
              {...form.register("host", { onChange: commit })}
            />
          </Field>
          <Field label="Path">
            <input
              placeholder="/"
              {...form.register("path", { onChange: commit })}
            />
          </Field>
        </div>
        {host || sslipHostnameEnabled ? (
          <>
            <div className="form-subsection">
              <span className="field__label">TLS mode</span>
              <div className="choice-cards choice-cards--three">
                <label>
                  <input
                    type="radio"
                    value="httpOnly"
                    {...form.register("tlsMode", { onChange: commit })}
                  />
                  <span>
                    <strong>HTTP only</strong>
                    <small>No certificate or redirect.</small>
                  </span>
                </label>
                <label>
                  <input
                    type="radio"
                    value="letsencrypt"
                    {...form.register("tlsMode", { onChange: commit })}
                  />
                  <span>
                    <strong>Let's Encrypt</strong>
                    <small>Admin-approved HTTP-01 or DNS-01.</small>
                  </span>
                </label>
                <label>
                  <input
                    type="radio"
                    value="customCertificate"
                    {...form.register("tlsMode", { onChange: commit })}
                  />
                  <span>
                    <strong>Custom certificate</strong>
                    <small>Use a scoped certificate.</small>
                  </span>
                </label>
              </div>
            </div>
            {tlsMode === "letsencrypt" ? (
              <div className="form-grid">
                <CertificateIssuerPicker
                  applicationId={runtimeSecretApplicationId}
                  environmentId={runtimeSecretEnvironmentId}
                  hostname={
                    dnsMode === "sslip" ? sslipHostnamePreview?.hostname : host
                  }
                  value={issuerRef}
                  enabled={certificateIssuersEnabled}
                  disabled={readOnly}
                  unavailableReason={certificateIssuersUnavailableReason}
                  onChange={(issuer) => {
                    form.setValue("issuerRef", issuer, { shouldDirty: true });
                    commit();
                  }}
                />
                <Field label="HTTP redirect">
                  <label className="switch">
                    <input
                      type="checkbox"
                      {...form.register("redirectHttp", { onChange: commit })}
                    />
                    <span />
                    Redirect port 80 to HTTPS
                  </label>
                </Field>
              </div>
            ) : null}
            {tlsMode === "customCertificate" ? (
              <CertificateReferencePicker
                applicationId={runtimeSecretApplicationId}
                environmentId={runtimeSecretEnvironmentId}
                value={certificateRef}
                enabled={certificateReferencesEnabled}
                disabled={readOnly}
                unavailableReason={certificateReferencesUnavailableReason}
                onChange={(reference) => {
                  form.setValue("certificateRef", reference, {
                    shouldDirty: true,
                  });
                  commit();
                }}
              />
            ) : null}

            <div className="form-subsection">
              <span className="field__label">DNS management</span>
              <div className="choice-cards choice-cards--three">
                <label>
                  <input
                    type="radio"
                    value="manual"
                    {...form.register("dnsMode", { onChange: commit })}
                  />
                  <span>
                    <strong>Manual DNS</strong>
                    <small>Show the exact required record.</small>
                  </span>
                </label>
                <label>
                  <input
                    type="radio"
                    value="externalDns"
                    disabled={
                      automaticDNSUnavailable && dnsMode !== "externalDns"
                    }
                    {...form.register("dnsMode", { onChange: commit })}
                  />
                  <span>
                    <strong>Automatic DNS</strong>
                    <small>Use an allowed external-dns integration.</small>
                  </span>
                </label>
                <label>
                  <input
                    type="radio"
                    value="sslip"
                    disabled={
                      (!sslipHostnamePreview ||
                        sslipHostnamePending ||
                        Boolean(sslipHostnameError)) &&
                      dnsMode !== "sslip"
                    }
                    {...form.register("dnsMode", {
                      onChange: (event) => {
                        if (
                          event.target.value === "sslip" &&
                          sslipHostnamePreview?.hostname
                        ) {
                          form.setValue("host", sslipHostnamePreview.hostname, {
                            shouldDirty: true,
                          });
                          form.setValue("dnsIntegrationRef", "", {
                            shouldDirty: true,
                          });
                        }
                        commit();
                      },
                    })}
                  />
                  <span>
                    <strong>Free sslip.io hostname</strong>
                    <small>Test/convenience only · server-derived.</small>
                  </span>
                </label>
              </div>
            </div>
            {dnsMode === "sslip" ? (
              <div className="notice notice--warning" role="status">
                <div>
                  <strong>
                    {sslipHostnamePreview
                      ? sslipHostnamePreview.hostname
                      : "sslip.io hostname unavailable"}
                  </strong>
                  <p>
                    A public ingress IP and exact fresh edge runtime readiness
                    are required. Dynamic ALB/load-balancer hostnames are not
                    eligible unless the operator provides a verified static
                    public IP observation. No External DNS integration is used.
                  </p>
                  {sslipHostnamePreview ? (
                    <small>
                      Source: {sslipHostnamePreview.source} · observed{" "}
                      {sslipHostnamePreview.observedAt}
                    </small>
                  ) : sslipHostnamePending ? (
                    <small>Loading the exact server-derived preview…</small>
                  ) : (
                    <small>
                      {sslipHostnameError ??
                        "This mode fails closed until a fresh exact preview is available."}
                    </small>
                  )}
                </div>
              </div>
            ) : null}
            {dnsMode === "externalDns" ? (
              <div className="form-grid">
                <Field
                  label="DNS integration"
                  error={dnsIntegrationError}
                  hint={
                    externalDNSCatalogPending
                      ? "Loading the authorized environment catalog…"
                      : "Only exact profiles authorized for this application and environment are selectable."
                  }
                >
                  <select
                    disabled={
                      externalDNSCatalogPending ||
                      !automaticDNSRuntimeReady ||
                      selectedDNSIntegration?.runtimeAvailable === false
                    }
                    {...form.register("dnsIntegrationRef", {
                      onChange: commit,
                    })}
                  >
                    {dnsIntegrationRef && !selectedDNSIntegration ? (
                      <option value={dnsIntegrationRef} disabled>
                        {dnsIntegrationRef} (unavailable)
                      </option>
                    ) : null}
                    <option value="">
                      {externalDNSCatalogPending
                        ? "Loading integrations…"
                        : dnsIntegrations.length === 0
                          ? "No authorized integrations"
                          : "Select an integration"}
                    </option>
                    {dnsIntegrations.map((integration) => (
                      <option
                        value={integration.slug}
                        key={integration.id}
                        disabled={!integration.runtimeAvailable}
                      >
                        {integration.name} · {integration.providerKind} ·{" "}
                        {integration.allowedDomainSuffixes.join(", ")}
                      </option>
                    ))}
                  </select>
                </Field>
                <Field label="TTL (seconds)">
                  <input
                    type="number"
                    min={30}
                    {...form.register("dnsTtl", {
                      valueAsNumber: true,
                      onChange: commit,
                    })}
                  />
                </Field>
                <div
                  className={`notice ${
                    selectedDNSIntegrationReady
                      ? "notice--success"
                      : "notice--warning"
                  }`}
                >
                  <div>
                    <strong>
                      {!selectedDNSIntegration
                        ? "Select an External DNS integration"
                        : selectedDNSIntegrationReady
                          ? "External DNS revision is ready"
                          : "External DNS runtime is not ready"}
                    </strong>
                    <p>
                      {!selectedDNSIntegration
                        ? "Choose one authorized integration before previewing or saving automatic DNS."
                        : selectedDNSIntegrationReady
                          ? "The selected revision is protected-Git materialized and freshly observed. Preview and save revalidate this exact slug, hostname, and runtime boundary."
                          : "Existing configuration remains visible, but a new automatic-DNS selection cannot be previewed or saved until the exact integration revision is freshly observed ready."}
                    </p>
                  </div>
                </div>
              </div>
            ) : null}
          </>
        ) : null}
      </section>
      <TraefikMiddlewareEditor
        definitions={middlewares ?? initial.middlewares}
        refs={middlewareRefs ?? initial.middlewareRefs}
        issue={initial.middlewareGuidedIssue}
        routeEnabled={Boolean(host)}
        readOnly={readOnly}
        editingUnavailableReason={middlewareEditingUnavailableReason}
        applicationId={runtimeSecretApplicationId}
        environmentId={runtimeSecretEnvironmentId}
        reusableProfilesEnabled={reusableMiddlewareProfilesEnabled}
        onChange={({ definitions, refs }) => {
          form.setValue("middlewares", definitions, { shouldDirty: true });
          form.setValue("middlewareRefs", refs, { shouldDirty: true });
          commit();
        }}
      />
      <details className="service-settings-advanced">
        <summary>
          <span>
            <strong>Advanced Kubernetes YAML overrides</strong>
            <small>
              Deployment, Service, Ingress, and ServiceAccount merge patches
            </small>
          </span>
          <Icon name="chevron" />
        </summary>
        <div className="service-settings-advanced__content">
          <div className="notice notice--warning" role="status">
            <div>
              <strong>Advanced YAML wins over matching Guided fields</strong>
              <p>
                Enter a partial Kubernetes resource object. Kuberploy merges it
                after Guided configuration, while retaining resource identity,
                App selectors, the immutable App image, and workload isolation.
              </p>
            </div>
          </div>
          {resourceOverrideError ? (
            <div className="yaml-diagnostic" role="alert">
              {resourceOverrideError}
            </div>
          ) : null}
          <div className="form-grid">
            <Field
              label="Deployment override YAML"
              hint="Partial metadata/spec mapping; use {} for no override."
            >
              <textarea
                aria-label="Deployment override YAML"
                rows={10}
                spellCheck={false}
                {...form.register("resourceOverrides.deploymentYaml", {
                  onChange: commit,
                })}
              />
            </Field>
            <Field
              label="Service override YAML"
              hint="Partial metadata/spec mapping; use {} for no override."
            >
              <textarea
                aria-label="Service override YAML"
                rows={10}
                spellCheck={false}
                {...form.register("resourceOverrides.serviceYaml", {
                  onChange: commit,
                })}
              />
            </Field>
            <Field
              label="Ingress override YAML"
              hint="Applied to each generated primary Ingress."
            >
              <textarea
                aria-label="Ingress override YAML"
                rows={10}
                spellCheck={false}
                {...form.register("resourceOverrides.ingressYaml", {
                  onChange: commit,
                })}
              />
            </Field>
            <Field
              label="ServiceAccount override YAML"
              hint="Supports metadata annotations such as an AWS IAM role ARN."
            >
              <textarea
                aria-label="ServiceAccount override YAML"
                rows={10}
                spellCheck={false}
                {...form.register("resourceOverrides.serviceAccountYaml", {
                  onChange: commit,
                })}
              />
            </Field>
          </div>
        </div>
      </details>
    </fieldset>
  );
}
