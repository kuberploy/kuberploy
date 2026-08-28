import { parse, stringify } from "yaml";
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
import { Select, Button, Field, useRowKeys } from "./ui";
import { Icon } from "./Icon";

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
  const rowKeys = useRowKeys(value.length);
  const update = (index: number, change: Partial<SchedulingRequirementDraft>) =>
    onChange(
      value.map((item, itemIndex) =>
        itemIndex === index ? { ...item, ...change } : item,
      ),
    );
  return (
    <div className="grid gap-2">
      {value.map((item, index) => {
        const needsValues = requirementNeedsValues(item.operator);
        return (
          <div
            className="grid items-end gap-2 grid-cols-[minmax(140px,_1.2fr)_minmax(110px,_0.7fr)_minmax(150px,_1fr)_auto] to-860:grid-cols-[repeat(2,_minmax(0,_1fr))_auto] to-580:grid-cols-[1fr]"
            key={rowKeys.keyAt(index)}
          >
            <label>
              <span>Label key</span>
              <input
                aria-label={`Expression ${index + 1} label key`}
                value={item.key}
                disabled={disabled}
                placeholder="kubernetes.io/arch"
                onChange={(event) => update(index, { key: event.target.value })}
              />
            </label>
            <Field label="Operator">
              <Select
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
              </Select>
            </Field>
            {needsValues ? (
              <label>
                <span>Values</span>
                <input
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
            <button
              type="button"
              className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
              aria-label={`Remove expression ${index + 1}`}
              disabled={disabled || value.length === 1}
              onClick={() => {
                rowKeys.removeAt(index);
                onChange(value.filter((_, itemIndex) => itemIndex !== index));
              }}
            >
              <Icon name="trash" />
            </button>
          </div>
        );
      })}
      <Button
        type="button"
        variant="secondary"
        disabled={disabled || value.length >= 32}
        onClick={() =>
          onChange([...value, { key: "", operator: "In", values: [""] }])
        }
      >
        <Icon name="plus" /> Add expression
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
  const rowKeys = useRowKeys(value.length);
  return (
    <div className="min-w-0 p-4 border border-line rounded-[10px] bg-surface">
      <div className="flex items-center justify-between gap-3 mb-3 [&>div]:grid [&>div]:gap-1 [&_strong]:text-ink [&_strong]:text-meta [&_span]:text-ink-faint [&_span]:text-xs [&_span]:font-semibold to-580:items-start to-580:flex-col">
        <div>
          <strong>{title}</strong>
          <span>Always targets only this service.</span>
        </div>
        <Button
          type="button"
          variant="secondary"
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
          <Icon name="plus" /> Add rule
        </Button>
      </div>
      {value.length === 0 ? (
        <p className="!m-0 py-2 px-3 rounded-lg !text-[var(--amber-ink)] bg-surface-soft">
          No rules.
        </p>
      ) : null}
      {value.map((item, index) => (
        <div
          className="grid items-end gap-2 grid-cols-[minmax(120px,_0.6fr)_minmax(180px,_1fr)_minmax(90px,_0.35fr)_auto] to-860:grid-cols-[repeat(2,_minmax(0,_1fr))_auto] to-580:grid-cols-[1fr]"
          key={rowKeys.keyAt(index)}
        >
          <Field label="Enforcement">
            <Select
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
            </Select>
          </Field>
          <label>
            <span>Topology key</span>
            <input
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
              <input
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
          <button
            type="button"
            className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
            aria-label={`Remove ${title} ${index + 1}`}
            disabled={disabled}
            onClick={() => {
              rowKeys.removeAt(index);
              onChange(value.filter((_, itemIndex) => itemIndex !== index));
            }}
          >
            <Icon name="trash" />
          </button>
        </div>
      ))}
      {!canAdd ? (
        <p className="!m-0 py-2 px-3 rounded-lg !text-[var(--amber-ink)] bg-surface-soft">
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
      ...(source.whenUnsatisfiable !== "ScheduleAnyway" &&
      typeof source.minDomains === "number"
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
  // Every editable list below gets row identity that survives a removal; see
  // useRowKeys. Keying these by index moves a row's DOM node onto its
  // successor when a row in the middle is deleted.
  const nodeSelectorKeys = useRowKeys(Object.keys(nodeSelector).length);
  const requiredTermKeys = useRowKeys(requiredTerms.length);
  const preferredTermKeys = useRowKeys(preferredTerms.length);
  const tolerationKeys = useRowKeys(tolerations.length);
  const topologyKeys = useRowKeys(topology.length);

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
    <div className="grid gap-3 [&_label_>_span]:text-ink-faint [&_label_>_span]:text-xs [&_label_>_span]:font-semibold [&_.icon-button]:w-10 [&_.icon-button]:h-11 [&_label]:grid [&_label]:min-w-0 [&_label]:gap-1.5 [&_[role='combobox']]:h-8 [&_[role='combobox']]:min-h-8 [&_[role='combobox']]:px-2">
      <div className="min-w-0 p-4 border border-line rounded-[10px] bg-surface">
        <div className="flex items-center justify-between gap-3 mb-3 [&>div]:grid [&>div]:gap-1 [&_strong]:text-ink [&_strong]:text-meta [&_span]:text-ink-faint [&_span]:text-xs [&_span]:font-semibold to-580:items-start to-580:flex-col">
          <div>
            <strong>Node selector</strong>
            <span>Exact labels every selected node must have.</span>
          </div>
          <Button
            type="button"
            variant="secondary"
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
            <Icon name="plus" /> Add label
          </Button>
        </div>
        {Object.entries(nodeSelector).map(([key, current], index) => (
          <div
            className="grid items-end gap-2 grid-cols-[minmax(0,_1fr)_minmax(0,_1fr)_auto] to-580:grid-cols-[1fr]"
            key={nodeSelectorKeys.keyAt(index)}
          >
            <label>
              <span>Label key</span>
              <input
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
              <input
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
            <button
              type="button"
              className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
              aria-label={`Remove node selector ${index + 1}`}
              disabled={disabled}
              onClick={() => {
                nodeSelectorKeys.removeAt(index);
                const next = Object.fromEntries(
                  Object.entries(nodeSelector).filter(
                    (_, itemIndex) => itemIndex !== index,
                  ),
                );
                update({ nodeSelectorYaml: fragment(next, "{}") });
              }}
            >
              <Icon name="trash" />
            </button>
          </div>
        ))}
        {Object.keys(nodeSelector).length === 0 ? (
          <p className="!m-0 py-2 px-3 rounded-lg !text-[var(--amber-ink)] bg-surface-soft">
            No node labels required.
          </p>
        ) : null}
      </div>

      <div className="min-w-0 p-4 border border-line rounded-[10px] bg-surface">
        <div className="flex items-center justify-between gap-3 mb-3 [&>div]:grid [&>div]:gap-1 [&_strong]:text-ink [&_strong]:text-meta [&_span]:text-ink-faint [&_span]:text-xs [&_span]:font-semibold to-580:items-start to-580:flex-col">
          <div>
            <strong>Required node affinity</strong>
            <span>
              Every expression in one term must match; any term may match.
            </span>
          </div>
          <Button
            type="button"
            variant="secondary"
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
            <Icon name="plus" /> Add term
          </Button>
        </div>
        {requiredTerms.map((term, index) => (
          <div
            className="mt-2 p-3 border border-line rounded-[9px] bg-surface-soft"
            key={requiredTermKeys.keyAt(index)}
          >
            <div className="flex items-center justify-between gap-3 mb-3 [&_strong]:text-ink [&_strong]:text-meta [&_label_span]:text-ink-faint [&_label_span]:text-xs [&_label_span]:font-semibold [&_label]:grid [&_label]:min-w-0 [&_label]:gap-1.5 [&_label]:grid-cols-[auto_82px] [&_label]:items-center [&_label]:ml-[auto] to-580:items-start to-580:flex-col to-580:[&_label]:ml-0">
              <strong>Required term {index + 1}</strong>
              <Button
                type="button"
                variant="ghost"
                disabled={disabled}
                onClick={() => {
                  requiredTermKeys.removeAt(index);
                  updateAffinity({
                    required: requiredTerms.filter(
                      (_, itemIndex) => itemIndex !== index,
                    ),
                  });
                }}
              >
                <Icon name="trash" /> Remove
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
          <p className="!m-0 py-2 px-3 rounded-lg !text-[var(--amber-ink)] bg-surface-soft">
            No required node affinity.
          </p>
        ) : null}
      </div>

      <div className="min-w-0 p-4 border border-line rounded-[10px] bg-surface">
        <div className="flex items-center justify-between gap-3 mb-3 [&>div]:grid [&>div]:gap-1 [&_strong]:text-ink [&_strong]:text-meta [&_span]:text-ink-faint [&_span]:text-xs [&_span]:font-semibold to-580:items-start to-580:flex-col">
          <div>
            <strong>Preferred node affinity</strong>
            <span>Higher weights influence placement without blocking it.</span>
          </div>
          <Button
            type="button"
            variant="secondary"
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
            <Icon name="plus" /> Add term
          </Button>
        </div>
        {preferredTerms.map((term, index) => (
          <div
            className="mt-2 p-3 border border-line rounded-[9px] bg-surface-soft"
            key={preferredTermKeys.keyAt(index)}
          >
            <div className="flex items-center justify-between gap-3 mb-3 [&_strong]:text-ink [&_strong]:text-meta [&_label_span]:text-ink-faint [&_label_span]:text-xs [&_label_span]:font-semibold [&_label]:grid [&_label]:min-w-0 [&_label]:gap-1.5 [&_label]:grid-cols-[auto_82px] [&_label]:items-center [&_label]:ml-[auto] to-580:items-start to-580:flex-col to-580:[&_label]:ml-0">
              <strong>Preferred term {index + 1}</strong>
              <label>
                <span>Weight</span>
                <input
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
                variant="ghost"
                disabled={disabled}
                onClick={() => {
                  preferredTermKeys.removeAt(index);
                  updateAffinity({
                    preferred: preferredTerms.filter(
                      (_, itemIndex) => itemIndex !== index,
                    ),
                  });
                }}
              >
                <Icon name="trash" /> Remove
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
          <p className="!m-0 py-2 px-3 rounded-lg !text-[var(--amber-ink)] bg-surface-soft">
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

      <div className="min-w-0 p-4 border border-line rounded-[10px] bg-surface">
        <div className="flex items-center justify-between gap-3 mb-3 [&>div]:grid [&>div]:gap-1 [&_strong]:text-ink [&_strong]:text-meta [&_span]:text-ink-faint [&_span]:text-xs [&_span]:font-semibold to-580:items-start to-580:flex-col">
          <div>
            <strong>Tolerations</strong>
            <span>Allow this service onto matching tainted nodes.</span>
          </div>
          <Button
            type="button"
            variant="secondary"
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
            <Icon name="plus" /> Add toleration
          </Button>
        </div>
        {tolerations.map((item, index) => (
          <div
            className="grid items-end gap-2 grid-cols-[minmax(130px,_1fr)_minmax(100px,_0.55fr)_minmax(110px,_0.8fr)_minmax(130px,_0.7fr)_minmax(90px,_0.35fr)_auto] to-1240:grid-cols-[repeat(3,_minmax(0,_1fr))_auto] to-860:grid-cols-[repeat(2,_minmax(0,_1fr))_auto] to-580:grid-cols-[1fr]"
            key={tolerationKeys.keyAt(index)}
          >
            <label>
              <span>Key</span>
              <input
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
            <Field label="Operator">
              <Select
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
              </Select>
            </Field>
            {item.operator === "Equal" ? (
              <label>
                <span>Value</span>
                <input
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
            <Field label="Effect">
              <Select
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
              </Select>
            </Field>
            {item.effect === "NoExecute" ? (
              <label>
                <span>Seconds</span>
                <input
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
            <button
              type="button"
              className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
              aria-label={`Remove toleration ${index + 1}`}
              disabled={disabled}
              onClick={() => {
                tolerationKeys.removeAt(index);
                update({
                  tolerationsYaml: fragment(
                    tolerations.filter((_, itemIndex) => itemIndex !== index),
                    "[]",
                  ),
                });
              }}
            >
              <Icon name="trash" />
            </button>
          </div>
        ))}
        {tolerations.length === 0 ? (
          <p className="!m-0 py-2 px-3 rounded-lg !text-[var(--amber-ink)] bg-surface-soft">
            No tolerations.
          </p>
        ) : null}
      </div>

      <div className="min-w-0 p-4 border border-line rounded-[10px] bg-surface">
        <div className="flex items-center justify-between gap-3 mb-3 [&>div]:grid [&>div]:gap-1 [&_strong]:text-ink [&_strong]:text-meta [&_span]:text-ink-faint [&_span]:text-xs [&_span]:font-semibold to-580:items-start to-580:flex-col">
          <div>
            <strong>Topology spread</strong>
            <span>Spread only this service across topology domains.</span>
          </div>
          <Button
            type="button"
            variant="secondary"
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
            <Icon name="plus" /> Add constraint
          </Button>
        </div>
        {topology.map((item, index) => (
          <div
            className="grid items-end gap-2 grid-cols-[minmax(180px,_1fr)_minmax(90px,_0.35fr)_minmax(145px,_0.6fr)_minmax(90px,_0.35fr)_minmax(150px,_0.7fr)_minmax(150px,_0.7fr)_auto] to-1240:grid-cols-[repeat(3,_minmax(0,_1fr))_auto] to-860:grid-cols-[repeat(2,_minmax(0,_1fr))_auto] to-580:grid-cols-[1fr]"
            key={topologyKeys.keyAt(index)}
          >
            <label>
              <span>Topology key</span>
              <input
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
              <input
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
            <Field label="Unsatisfiable">
              <Select
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
                              minDomains:
                                event.target.value === "DoNotSchedule"
                                  ? current.minDomains
                                  : undefined,
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
              </Select>
            </Field>
            <label>
              <span>Min domains</span>
              <input
                aria-label={`Topology spread ${index + 1} min domains`}
                type="number"
                min={1}
                value={item.minDomains ?? ""}
                disabled={
                  disabled || item.whenUnsatisfiable !== "DoNotSchedule"
                }
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
            <Field label="Node affinity policy">
              <Select
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
              </Select>
            </Field>
            <Field label="Node taints policy">
              <Select
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
              </Select>
            </Field>
            <button
              type="button"
              className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
              aria-label={`Remove topology spread ${index + 1}`}
              disabled={disabled}
              onClick={() => {
                topologyKeys.removeAt(index);
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
                });
              }}
            >
              <Icon name="trash" />
            </button>
          </div>
        ))}
        {topology.length === 0 ? (
          <p className="!m-0 py-2 px-3 rounded-lg !text-[var(--amber-ink)] bg-surface-soft">
            No topology constraints.
          </p>
        ) : null}
      </div>

      <div className="min-w-0 p-4 border border-line rounded-[10px] bg-surface [&_label]:grid [&_label]:gap-1 [&_label]:max-w-[480px] [&_strong]:text-ink [&_strong]:text-meta [&_span]:text-ink-faint [&_span]:text-xs [&_span]:font-semibold">
        <label>
          <strong>Priority class</strong>
          <span>
            Optional existing PriorityClass; Kubernetes system-* classes are
            reserved.
          </span>
          <input
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
