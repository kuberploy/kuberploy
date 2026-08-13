import { parse, stringify } from "yaml";
import { Plus, Trash2 } from "lucide-react";
import type {
  TopologySpreadConstraint,
  WorkloadAffinity,
  WorkloadToleration,
} from "../api/types";
import type {
  PreferredNodeAffinityDraft,
  SchedulingRequirementDraft,
  SameApplicationPodAntiAffinityDraft,
} from "./SchedulingAffinityFields";
import { Button } from "./shadcn/button";
import { Input } from "./shadcn/input";

export type SchedulingEditorValue = {
  nodeSelectorYaml: string;
  affinityYaml: string;
  topologySpreadYaml: string;
  tolerationsYaml: string;
  priorityClassName: string;
};

type NodeSelectorRow = { key: string; value: string };
type RequiredNodeTerm = { requirements: SchedulingRequirementDraft[] };
type PodPreset = SameApplicationPodAntiAffinityDraft;
type TopologyDraft = Omit<TopologySpreadConstraint, "labelSelector">;

const requirementOperators: SchedulingRequirementDraft["operator"][] = [
  "In",
  "NotIn",
  "Exists",
  "DoesNotExist",
  "Gt",
  "Lt",
];
const tolerationEffects: WorkloadToleration["effect"][] = [
  "NoSchedule",
  "PreferNoSchedule",
  "NoExecute",
];

function requirementNeedsValues(
  operator: SchedulingRequirementDraft["operator"],
) {
  return ["In", "NotIn", "Gt", "Lt"].includes(operator);
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function list(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function fragment(value: unknown, empty: "{}" | "[]") {
  if (
    (Array.isArray(value) && value.length === 0) ||
    (!Array.isArray(value) && Object.keys(record(value)).length === 0)
  ) {
    return empty;
  }
  return stringify(value, { lineWidth: 0 }).trimEnd();
}

function parsed(value: string, fallback: unknown): unknown {
  try {
    return parse(value) ?? fallback;
  } catch {
    return fallback;
  }
}

function requirements(value: unknown): SchedulingRequirementDraft[] {
  return list(value).flatMap((item) => {
    const source = record(item);
    const operator = source.operator;
    if (
      typeof source.key !== "string" ||
      !requirementOperators.includes(
        operator as SchedulingRequirementDraft["operator"],
      )
    ) {
      return [];
    }
    return [
      {
        key: source.key,
        operator: operator as SchedulingRequirementDraft["operator"],
        ...(Array.isArray(source.values)
          ? {
              values: source.values.filter(
                (item): item is string => typeof item === "string",
              ),
            }
          : {}),
      },
    ];
  });
}

function podPresets(value: unknown): PodPreset[] {
  const source = record(value);
  return [
    ...list(source.requiredDuringSchedulingIgnoredDuringExecution).map(
      (item) => ({
        enforcement: "required" as const,
        topologyKey: String(record(item).topologyKey ?? ""),
      }),
    ),
    ...list(source.preferredDuringSchedulingIgnoredDuringExecution).map(
      (item) => {
        const preferred = record(item);
        return {
          enforcement: "preferred" as const,
          topologyKey: String(
            record(preferred.podAffinityTerm).topologyKey ?? "",
          ),
          weight: Number(preferred.weight ?? 100),
        };
      },
    ),
  ];
}

function podAffinity(presets: PodPreset[], applicationId: string) {
  const term = (topologyKey: string) => ({
    labelSelector: {
      matchLabels: { "kuberploy.io/application": applicationId },
    },
    topologyKey,
  });
  const required = presets
    .filter((item) => item.enforcement === "required")
    .map((item) => term(item.topologyKey));
  const preferred = presets
    .filter((item) => item.enforcement === "preferred")
    .map((item) => ({
      weight: item.weight ?? 100,
      podAffinityTerm: term(item.topologyKey),
    }));
  return {
    ...(required.length
      ? { requiredDuringSchedulingIgnoredDuringExecution: required }
      : {}),
    ...(preferred.length
      ? { preferredDuringSchedulingIgnoredDuringExecution: preferred }
      : {}),
  };
}

function applicationIdFromAffinity(affinity: WorkloadAffinity) {
  for (const kind of [affinity.podAffinity, affinity.podAntiAffinity]) {
    const required = kind?.requiredDuringSchedulingIgnoredDuringExecution ?? [];
    const preferred =
      kind?.preferredDuringSchedulingIgnoredDuringExecution ?? [];
    const terms = [
      ...required,
      ...preferred.map((item) => item.podAffinityTerm),
    ];
    for (const term of terms) {
      const id = term.labelSelector.matchLabels?.["kuberploy.io/application"];
      if (id) return id;
    }
  }
  return "";
}

function RequirementRows({
  value,
  onChange,
  disabled,
}: {
  value: SchedulingRequirementDraft[];
  onChange: (value: SchedulingRequirementDraft[]) => void;
  disabled: boolean;
}) {
  const update = (index: number, change: Partial<SchedulingRequirementDraft>) =>
    onChange(
      value.map((item, itemIndex) =>
        itemIndex === index ? { ...item, ...change } : item,
      ),
    );
  return (
    <div className="scheduling-editor__rows">
      {value.map((item, index) => {
        const needsValues = requirementNeedsValues(item.operator);
        return (
          <div className="scheduling-editor__requirement" key={index}>
            <label>
              <span>Label key</span>
              <Input
                aria-label={`Expression ${index + 1} label key`}
                value={item.key}
                disabled={disabled}
                placeholder="kubernetes.io/arch"
                onChange={(event) => update(index, { key: event.target.value })}
              />
            </label>
            <label>
              <span>Operator</span>
              <select
                aria-label={`Expression ${index + 1} operator`}
                value={item.operator}
                disabled={disabled}
                onChange={(event) => {
                  const operator = event.target
                    .value as SchedulingRequirementDraft["operator"];
                  update(index, {
                    operator,
                    values: requirementNeedsValues(operator)
                      ? (item.values ?? [""])
                      : undefined,
                  });
                }}
              >
                {requirementOperators.map((operator) => (
                  <option key={operator}>{operator}</option>
                ))}
              </select>
            </label>
            {needsValues ? (
              <label>
                <span>Values</span>
                <Input
                  aria-label={`Expression ${index + 1} values`}
                  value={(item.values ?? []).join(", ")}
                  disabled={disabled}
                  placeholder="amd64, arm64"
                  onChange={(event) =>
                    update(index, {
                      values: event.target.value
                        .split(",")
                        .map((part) => part.trim()),
                    })
                  }
                />
              </label>
            ) : (
              <span />
            )}
            <Button
              type="button"
              size="icon"
              variant="ghost"
              aria-label={`Remove expression ${index + 1}`}
              disabled={disabled || value.length === 1}
              onClick={() =>
                onChange(value.filter((_, itemIndex) => itemIndex !== index))
              }
            >
              <Trash2 />
            </Button>
          </div>
        );
      })}
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={disabled || value.length >= 32}
        onClick={() =>
          onChange([...value, { key: "", operator: "In", values: [""] }])
        }
      >
        <Plus /> Add expression
      </Button>
    </div>
  );
}

function PodPresetRows({
  title,
  value,
  onChange,
  disabled,
  canAdd,
}: {
  title: string;
  value: PodPreset[];
  onChange: (value: PodPreset[]) => void;
  disabled: boolean;
  canAdd: boolean;
}) {
  return (
    <div className="scheduling-editor__group">
      <div className="scheduling-editor__group-heading">
        <div>
          <strong>{title}</strong>
          <span>Always targets only this service.</span>
        </div>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={disabled || !canAdd || value.length >= 16}
          onClick={() =>
            onChange([
              ...value,
              {
                enforcement: "required",
                topologyKey: "kubernetes.io/hostname",
              },
            ])
          }
        >
          <Plus /> Add rule
        </Button>
      </div>
      {value.length === 0 ? (
        <p className="scheduling-editor__empty">No rules.</p>
      ) : null}
      {value.map((item, index) => (
        <div className="scheduling-editor__pod-row" key={index}>
          <label>
            <span>Enforcement</span>
            <select
              aria-label={`${title} ${index + 1} enforcement`}
              value={item.enforcement}
              disabled={disabled}
              onChange={(event) => {
                const enforcement = event.target
                  .value as PodPreset["enforcement"];
                onChange(
                  value.map((current, itemIndex) =>
                    itemIndex === index
                      ? {
                          ...current,
                          enforcement,
                          ...(enforcement === "preferred"
                            ? { weight: current.weight ?? 100 }
                            : { weight: undefined }),
                        }
                      : current,
                  ),
                );
              }}
            >
              <option value="required">Required</option>
              <option value="preferred">Preferred</option>
            </select>
          </label>
          <label>
            <span>Topology key</span>
            <Input
              aria-label={`${title} ${index + 1} topology key`}
              value={item.topologyKey}
              disabled={disabled}
              onChange={(event) =>
                onChange(
                  value.map((current, itemIndex) =>
                    itemIndex === index
                      ? { ...current, topologyKey: event.target.value }
                      : current,
                  ),
                )
              }
            />
          </label>
          {item.enforcement === "preferred" ? (
            <label>
              <span>Weight</span>
              <Input
                aria-label={`${title} ${index + 1} weight`}
                type="number"
                min={1}
                max={100}
                value={item.weight ?? 100}
                disabled={disabled}
                onChange={(event) =>
                  onChange(
                    value.map((current, itemIndex) =>
                      itemIndex === index
                        ? { ...current, weight: Number(event.target.value) }
                        : current,
                    ),
                  )
                }
              />
            </label>
          ) : (
            <span />
          )}
          <Button
            type="button"
            size="icon"
            variant="ghost"
            aria-label={`Remove ${title} ${index + 1}`}
            disabled={disabled}
            onClick={() =>
              onChange(value.filter((_, itemIndex) => itemIndex !== index))
            }
          >
            <Trash2 />
          </Button>
        </div>
      ))}
      {!canAdd ? (
        <p className="scheduling-editor__warning">
          Save service identity before adding pod placement rules.
        </p>
      ) : null}
    </div>
  );
}

export function SchedulingEditor({
  value,
  applicationId = "",
  onChange,
  disabled = false,
}: {
  value: SchedulingEditorValue;
  applicationId?: string;
  onChange: (value: SchedulingEditorValue) => void;
  disabled?: boolean;
}) {
  const nodeSelector = record(parsed(value.nodeSelectorYaml, {}));
  const affinity = record(parsed(value.affinityYaml, {})) as WorkloadAffinity;
  const nodeAffinity = record(affinity.nodeAffinity);
  const requiredNode = record(
    nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution,
  );
  const requiredTerms: RequiredNodeTerm[] = list(
    requiredNode.nodeSelectorTerms,
  ).map((item) => ({
    requirements: requirements(record(item).matchExpressions),
  }));
  const preferredTerms: PreferredNodeAffinityDraft[] = list(
    nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution,
  ).map((item) => {
    const source = record(item);
    return {
      weight: Number(source.weight ?? 50),
      requirements: requirements(record(source.preference).matchExpressions),
    };
  });
  const podAffinityDraft = podPresets(affinity.podAffinity);
  const podAntiAffinityDraft = podPresets(affinity.podAntiAffinity);
  const tolerations = list(parsed(value.tolerationsYaml, [])).map(
    (item) => record(item) as WorkloadToleration,
  );
  const topology = list(parsed(value.topologySpreadYaml, [])).map((item) => {
    const source = record(item);
    return {
      maxSkew: Number(source.maxSkew ?? 1),
      topologyKey: String(source.topologyKey ?? "kubernetes.io/hostname"),
      whenUnsatisfiable:
        source.whenUnsatisfiable === "ScheduleAnyway"
          ? ("ScheduleAnyway" as const)
          : ("DoNotSchedule" as const),
      ...(typeof source.minDomains === "number"
        ? { minDomains: source.minDomains }
        : {}),
      ...(source.nodeAffinityPolicy === "Ignore" ||
      source.nodeAffinityPolicy === "Honor"
        ? { nodeAffinityPolicy: source.nodeAffinityPolicy }
        : {}),
      ...(source.nodeTaintsPolicy === "Ignore" ||
      source.nodeTaintsPolicy === "Honor"
        ? { nodeTaintsPolicy: source.nodeTaintsPolicy }
        : {}),
    };
  });
  const exactApplicationId =
    applicationId || applicationIdFromAffinity(affinity);

  const update = (change: Partial<SchedulingEditorValue>) =>
    onChange({ ...value, ...change });
  const updateAffinity = (change: {
    required?: RequiredNodeTerm[];
    preferred?: PreferredNodeAffinityDraft[];
    pod?: PodPreset[];
    anti?: PodPreset[];
  }) => {
    const required = change.required ?? requiredTerms;
    const preferred = change.preferred ?? preferredTerms;
    const pod = change.pod ?? podAffinityDraft;
    const anti = change.anti ?? podAntiAffinityDraft;
    const next: WorkloadAffinity = {
      ...(required.length || preferred.length
        ? {
            nodeAffinity: {
              ...(required.length
                ? {
                    requiredDuringSchedulingIgnoredDuringExecution: {
                      nodeSelectorTerms: required.map((term) => ({
                        matchExpressions: term.requirements,
                      })),
                    },
                  }
                : {}),
              ...(preferred.length
                ? {
                    preferredDuringSchedulingIgnoredDuringExecution:
                      preferred.map((term) => ({
                        weight: term.weight,
                        preference: { matchExpressions: term.requirements },
                      })),
                  }
                : {}),
            },
          }
        : {}),
      ...(pod.length && exactApplicationId
        ? { podAffinity: podAffinity(pod, exactApplicationId) }
        : {}),
      ...(anti.length && exactApplicationId
        ? { podAntiAffinity: podAffinity(anti, exactApplicationId) }
        : {}),
    };
    update({ affinityYaml: fragment(next, "{}") });
  };

  return (
    <div className="scheduling-editor">
      <div className="scheduling-editor__group">
        <div className="scheduling-editor__group-heading">
          <div>
            <strong>Node selector</strong>
            <span>Exact labels every selected node must have.</span>
          </div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={disabled || Object.keys(nodeSelector).length >= 32}
            onClick={() => {
              const rows = [
                ...Object.entries(nodeSelector).map(([key, current]) => ({
                  key,
                  value: String(current),
                })),
                { key: "", value: "" },
              ];
              update({
                nodeSelectorYaml: fragment(
                  Object.fromEntries(rows.map((row) => [row.key, row.value])),
                  "{}",
                ),
              });
            }}
          >
            <Plus /> Add label
          </Button>
        </div>
        {Object.entries(nodeSelector).map(([key, current], index) => (
          <div className="scheduling-editor__key-value" key={index}>
            <label>
              <span>Label key</span>
              <Input
                aria-label={`Node selector ${index + 1} key`}
                value={key}
                disabled={disabled}
                onChange={(event) => {
                  const rows: NodeSelectorRow[] = Object.entries(
                    nodeSelector,
                  ).map(([rowKey, rowValue]) => ({
                    key: rowKey,
                    value: String(rowValue),
                  }));
                  rows[index] = { ...rows[index], key: event.target.value };
                  update({
                    nodeSelectorYaml: fragment(
                      Object.fromEntries(
                        rows.map((row) => [row.key, row.value]),
                      ),
                      "{}",
                    ),
                  });
                }}
              />
            </label>
            <label>
              <span>Label value</span>
              <Input
                aria-label={`Node selector ${index + 1} value`}
                value={String(current)}
                disabled={disabled}
                onChange={(event) => {
                  const rows: NodeSelectorRow[] = Object.entries(
                    nodeSelector,
                  ).map(([rowKey, rowValue]) => ({
                    key: rowKey,
                    value: String(rowValue),
                  }));
                  rows[index] = { ...rows[index], value: event.target.value };
                  update({
                    nodeSelectorYaml: fragment(
                      Object.fromEntries(
                        rows.map((row) => [row.key, row.value]),
                      ),
                      "{}",
                    ),
                  });
                }}
              />
            </label>
            <Button
              type="button"
              size="icon"
              variant="ghost"
              aria-label={`Remove node selector ${index + 1}`}
              disabled={disabled}
              onClick={() => {
                const next = Object.fromEntries(
                  Object.entries(nodeSelector).filter(
                    (_, itemIndex) => itemIndex !== index,
                  ),
                );
                update({ nodeSelectorYaml: fragment(next, "{}") });
              }}
            >
              <Trash2 />
            </Button>
          </div>
        ))}
        {Object.keys(nodeSelector).length === 0 ? (
          <p className="scheduling-editor__empty">No node labels required.</p>
        ) : null}
      </div>

      <div className="scheduling-editor__group">
        <div className="scheduling-editor__group-heading">
          <div>
            <strong>Required node affinity</strong>
            <span>
              Every expression in one term must match; any term may match.
            </span>
          </div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={disabled || requiredTerms.length >= 16}
            onClick={() =>
              updateAffinity({
                required: [
                  ...requiredTerms,
                  { requirements: [{ key: "", operator: "In", values: [""] }] },
                ],
              })
            }
          >
            <Plus /> Add term
          </Button>
        </div>
        {requiredTerms.map((term, index) => (
          <div className="scheduling-editor__term" key={index}>
            <div className="scheduling-editor__term-heading">
              <strong>Required term {index + 1}</strong>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                disabled={disabled}
                onClick={() =>
                  updateAffinity({
                    required: requiredTerms.filter(
                      (_, itemIndex) => itemIndex !== index,
                    ),
                  })
                }
              >
                <Trash2 /> Remove
              </Button>
            </div>
            <RequirementRows
              value={term.requirements}
              disabled={disabled}
              onChange={(requirementsValue) =>
                updateAffinity({
                  required: requiredTerms.map((current, itemIndex) =>
                    itemIndex === index
                      ? { requirements: requirementsValue }
                      : current,
                  ),
                })
              }
            />
          </div>
        ))}
        {requiredTerms.length === 0 ? (
          <p className="scheduling-editor__empty">No required node affinity.</p>
        ) : null}
      </div>

      <div className="scheduling-editor__group">
        <div className="scheduling-editor__group-heading">
          <div>
            <strong>Preferred node affinity</strong>
            <span>Higher weights influence placement without blocking it.</span>
          </div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={disabled || preferredTerms.length >= 16}
            onClick={() =>
              updateAffinity({
                preferred: [
                  ...preferredTerms,
                  {
                    weight: 50,
                    requirements: [{ key: "", operator: "In", values: [""] }],
                  },
                ],
              })
            }
          >
            <Plus /> Add term
          </Button>
        </div>
        {preferredTerms.map((term, index) => (
          <div className="scheduling-editor__term" key={index}>
            <div className="scheduling-editor__term-heading">
              <strong>Preferred term {index + 1}</strong>
              <label>
                <span>Weight</span>
                <Input
                  aria-label={`Preferred term ${index + 1} weight`}
                  type="number"
                  min={1}
                  max={100}
                  value={term.weight}
                  disabled={disabled}
                  onChange={(event) =>
                    updateAffinity({
                      preferred: preferredTerms.map((current, itemIndex) =>
                        itemIndex === index
                          ? { ...current, weight: Number(event.target.value) }
                          : current,
                      ),
                    })
                  }
                />
              </label>
              <Button
                type="button"
                size="sm"
                variant="ghost"
                disabled={disabled}
                onClick={() =>
                  updateAffinity({
                    preferred: preferredTerms.filter(
                      (_, itemIndex) => itemIndex !== index,
                    ),
                  })
                }
              >
                <Trash2 /> Remove
              </Button>
            </div>
            <RequirementRows
              value={term.requirements}
              disabled={disabled}
              onChange={(requirementsValue) =>
                updateAffinity({
                  preferred: preferredTerms.map((current, itemIndex) =>
                    itemIndex === index
                      ? { ...current, requirements: requirementsValue }
                      : current,
                  ),
                })
              }
            />
          </div>
        ))}
        {preferredTerms.length === 0 ? (
          <p className="scheduling-editor__empty">
            No preferred node affinity.
          </p>
        ) : null}
      </div>

      <PodPresetRows
        title="Same-service pod affinity"
        value={podAffinityDraft}
        onChange={(pod) => updateAffinity({ pod })}
        disabled={disabled}
        canAdd={Boolean(exactApplicationId)}
      />
      <PodPresetRows
        title="Same-service pod anti-affinity"
        value={podAntiAffinityDraft}
        onChange={(anti) => updateAffinity({ anti })}
        disabled={disabled}
        canAdd={Boolean(exactApplicationId)}
      />

      <div className="scheduling-editor__group">
        <div className="scheduling-editor__group-heading">
          <div>
            <strong>Tolerations</strong>
            <span>Allow this service onto matching tainted nodes.</span>
          </div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={disabled || tolerations.length >= 32}
            onClick={() =>
              update({
                tolerationsYaml: fragment(
                  [
                    ...tolerations,
                    {
                      key: "",
                      operator: "Equal",
                      value: "",
                      effect: "NoSchedule",
                    },
                  ],
                  "[]",
                ),
              })
            }
          >
            <Plus /> Add toleration
          </Button>
        </div>
        {tolerations.map((item, index) => (
          <div className="scheduling-editor__toleration" key={index}>
            <label>
              <span>Key</span>
              <Input
                aria-label={`Toleration ${index + 1} key`}
                value={item.key}
                disabled={disabled}
                onChange={(event) =>
                  update({
                    tolerationsYaml: fragment(
                      tolerations.map((current, itemIndex) =>
                        itemIndex === index
                          ? { ...current, key: event.target.value }
                          : current,
                      ),
                      "[]",
                    ),
                  })
                }
              />
            </label>
            <label>
              <span>Operator</span>
              <select
                aria-label={`Toleration ${index + 1} operator`}
                value={item.operator}
                disabled={disabled}
                onChange={(event) => {
                  const operator = event.target
                    .value as WorkloadToleration["operator"];
                  update({
                    tolerationsYaml: fragment(
                      tolerations.map((current, itemIndex) =>
                        itemIndex === index
                          ? {
                              ...current,
                              operator,
                              ...(operator === "Exists"
                                ? { value: undefined }
                                : { value: current.value ?? "" }),
                            }
                          : current,
                      ),
                      "[]",
                    ),
                  });
                }}
              >
                <option>Equal</option>
                <option>Exists</option>
              </select>
            </label>
            {item.operator === "Equal" ? (
              <label>
                <span>Value</span>
                <Input
                  aria-label={`Toleration ${index + 1} value`}
                  value={item.value ?? ""}
                  disabled={disabled}
                  onChange={(event) =>
                    update({
                      tolerationsYaml: fragment(
                        tolerations.map((current, itemIndex) =>
                          itemIndex === index
                            ? { ...current, value: event.target.value }
                            : current,
                        ),
                        "[]",
                      ),
                    })
                  }
                />
              </label>
            ) : (
              <span />
            )}
            <label>
              <span>Effect</span>
              <select
                aria-label={`Toleration ${index + 1} effect`}
                value={item.effect}
                disabled={disabled}
                onChange={(event) =>
                  update({
                    tolerationsYaml: fragment(
                      tolerations.map((current, itemIndex) =>
                        itemIndex === index
                          ? {
                              ...current,
                              effect: event.target
                                .value as WorkloadToleration["effect"],
                            }
                          : current,
                      ),
                      "[]",
                    ),
                  })
                }
              >
                {tolerationEffects.map((effect) => (
                  <option key={effect}>{effect}</option>
                ))}
              </select>
            </label>
            {item.effect === "NoExecute" ? (
              <label>
                <span>Seconds</span>
                <Input
                  aria-label={`Toleration ${index + 1} seconds`}
                  type="number"
                  min={0}
                  value={item.tolerationSeconds ?? ""}
                  disabled={disabled}
                  onChange={(event) =>
                    update({
                      tolerationsYaml: fragment(
                        tolerations.map((current, itemIndex) =>
                          itemIndex === index
                            ? {
                                ...current,
                                tolerationSeconds:
                                  event.target.value === ""
                                    ? undefined
                                    : Number(event.target.value),
                              }
                            : current,
                        ),
                        "[]",
                      ),
                    })
                  }
                />
              </label>
            ) : (
              <span />
            )}
            <Button
              type="button"
              size="icon"
              variant="ghost"
              aria-label={`Remove toleration ${index + 1}`}
              disabled={disabled}
              onClick={() =>
                update({
                  tolerationsYaml: fragment(
                    tolerations.filter((_, itemIndex) => itemIndex !== index),
                    "[]",
                  ),
                })
              }
            >
              <Trash2 />
            </Button>
          </div>
        ))}
        {tolerations.length === 0 ? (
          <p className="scheduling-editor__empty">No tolerations.</p>
        ) : null}
      </div>

      <div className="scheduling-editor__group">
        <div className="scheduling-editor__group-heading">
          <div>
            <strong>Topology spread</strong>
            <span>Spread only this service across topology domains.</span>
          </div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={disabled || !exactApplicationId || topology.length >= 16}
            onClick={() =>
              update({
                topologySpreadYaml: fragment(
                  [
                    ...topology,
                    {
                      maxSkew: 1,
                      topologyKey: "topology.kubernetes.io/zone",
                      whenUnsatisfiable: "DoNotSchedule",
                      labelSelector: {
                        matchLabels: {
                          "kuberploy.io/application": exactApplicationId,
                        },
                      },
                    },
                  ],
                  "[]",
                ),
              })
            }
          >
            <Plus /> Add constraint
          </Button>
        </div>
        {topology.map((item, index) => (
          <div className="scheduling-editor__topology" key={index}>
            <label>
              <span>Topology key</span>
              <Input
                aria-label={`Topology spread ${index + 1} key`}
                value={item.topologyKey}
                disabled={disabled}
                onChange={(event) =>
                  update({
                    topologySpreadYaml: fragment(
                      topology.map((current, itemIndex) => ({
                        ...current,
                        labelSelector: {
                          matchLabels: {
                            "kuberploy.io/application": exactApplicationId,
                          },
                        },
                        ...(itemIndex === index
                          ? { topologyKey: event.target.value }
                          : {}),
                      })),
                      "[]",
                    ),
                  })
                }
              />
            </label>
            <label>
              <span>Max skew</span>
              <Input
                aria-label={`Topology spread ${index + 1} max skew`}
                type="number"
                min={1}
                value={item.maxSkew}
                disabled={disabled}
                onChange={(event) =>
                  update({
                    topologySpreadYaml: fragment(
                      topology.map((current, itemIndex) => ({
                        ...current,
                        labelSelector: {
                          matchLabels: {
                            "kuberploy.io/application": exactApplicationId,
                          },
                        },
                        ...(itemIndex === index
                          ? { maxSkew: Number(event.target.value) }
                          : {}),
                      })),
                      "[]",
                    ),
                  })
                }
              />
            </label>
            <label>
              <span>Unsatisfiable</span>
              <select
                aria-label={`Topology spread ${index + 1} unsatisfiable`}
                value={item.whenUnsatisfiable}
                disabled={disabled}
                onChange={(event) =>
                  update({
                    topologySpreadYaml: fragment(
                      topology.map((current, itemIndex) => ({
                        ...current,
                        labelSelector: {
                          matchLabels: {
                            "kuberploy.io/application": exactApplicationId,
                          },
                        },
                        ...(itemIndex === index
                          ? {
                              whenUnsatisfiable: event.target
                                .value as TopologyDraft["whenUnsatisfiable"],
                            }
                          : {}),
                      })),
                      "[]",
                    ),
                  })
                }
              >
                <option>DoNotSchedule</option>
                <option>ScheduleAnyway</option>
              </select>
            </label>
            <label>
              <span>Min domains</span>
              <Input
                aria-label={`Topology spread ${index + 1} min domains`}
                type="number"
                min={1}
                value={item.minDomains ?? ""}
                disabled={disabled}
                onChange={(event) =>
                  update({
                    topologySpreadYaml: fragment(
                      topology.map((current, itemIndex) => ({
                        ...current,
                        labelSelector: {
                          matchLabels: {
                            "kuberploy.io/application": exactApplicationId,
                          },
                        },
                        ...(itemIndex === index
                          ? {
                              minDomains:
                                event.target.value === ""
                                  ? undefined
                                  : Number(event.target.value),
                            }
                          : {}),
                      })),
                      "[]",
                    ),
                  })
                }
              />
            </label>
            <label>
              <span>Node affinity policy</span>
              <select
                aria-label={`Topology spread ${index + 1} node affinity policy`}
                value={item.nodeAffinityPolicy ?? ""}
                disabled={disabled}
                onChange={(event) =>
                  update({
                    topologySpreadYaml: fragment(
                      topology.map((current, itemIndex) => ({
                        ...current,
                        labelSelector: {
                          matchLabels: {
                            "kuberploy.io/application": exactApplicationId,
                          },
                        },
                        ...(itemIndex === index
                          ? {
                              nodeAffinityPolicy:
                                event.target.value === ""
                                  ? undefined
                                  : (event.target
                                      .value as TopologyDraft["nodeAffinityPolicy"]),
                            }
                          : {}),
                      })),
                      "[]",
                    ),
                  })
                }
              >
                <option value="">Kubernetes default</option>
                <option>Honor</option>
                <option>Ignore</option>
              </select>
            </label>
            <label>
              <span>Node taints policy</span>
              <select
                aria-label={`Topology spread ${index + 1} node taints policy`}
                value={item.nodeTaintsPolicy ?? ""}
                disabled={disabled}
                onChange={(event) =>
                  update({
                    topologySpreadYaml: fragment(
                      topology.map((current, itemIndex) => ({
                        ...current,
                        labelSelector: {
                          matchLabels: {
                            "kuberploy.io/application": exactApplicationId,
                          },
                        },
                        ...(itemIndex === index
                          ? {
                              nodeTaintsPolicy:
                                event.target.value === ""
                                  ? undefined
                                  : (event.target
                                      .value as TopologyDraft["nodeTaintsPolicy"]),
                            }
                          : {}),
                      })),
                      "[]",
                    ),
                  })
                }
              >
                <option value="">Kubernetes default</option>
                <option>Honor</option>
                <option>Ignore</option>
              </select>
            </label>
            <Button
              type="button"
              size="icon"
              variant="ghost"
              aria-label={`Remove topology spread ${index + 1}`}
              disabled={disabled}
              onClick={() =>
                update({
                  topologySpreadYaml: fragment(
                    topology
                      .filter((_, itemIndex) => itemIndex !== index)
                      .map((current) => ({
                        ...current,
                        labelSelector: {
                          matchLabels: {
                            "kuberploy.io/application": exactApplicationId,
                          },
                        },
                      })),
                    "[]",
                  ),
                })
              }
            >
              <Trash2 />
            </Button>
          </div>
        ))}
        {topology.length === 0 ? (
          <p className="scheduling-editor__empty">No topology constraints.</p>
        ) : null}
      </div>

      <div className="scheduling-editor__group scheduling-editor__priority">
        <label>
          <strong>Priority class</strong>
          <span>
            Optional existing PriorityClass; Kubernetes system-* classes are
            reserved.
          </span>
          <Input
            aria-label="Priority class"
            value={value.priorityClassName}
            disabled={disabled}
            placeholder="workload-high"
            onChange={(event) =>
              update({ priorityClassName: event.target.value })
            }
          />
        </label>
      </div>
    </div>
  );
}
