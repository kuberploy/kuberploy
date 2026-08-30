import {
  Select,
  Button,
  ButtonRow,
  CardHeader,
  Field,
  FormGrid,
  useRowKeys,
} from "./ui";

export type SchedulingRequirementDraft = {
  key: string;
  operator: "In" | "NotIn" | "Exists" | "DoesNotExist" | "Gt" | "Lt";
  values?: string[];
};

export type PreferredNodeAffinityDraft = {
  weight: number;
  requirements: SchedulingRequirementDraft[];
};

export type SameApplicationPodAntiAffinityDraft = {
  enforcement: "required" | "preferred";
  topologyKey: string;
  weight?: number;
};

const operators: SchedulingRequirementDraft["operator"][] = [
  "In",
  "NotIn",
  "Exists",
  "DoesNotExist",
  "Gt",
  "Lt",
];

function needsValues(operator: SchedulingRequirementDraft["operator"]) {
  return ["In", "NotIn", "Gt", "Lt"].includes(operator);
}

function valuesText(values: string[] | undefined) {
  return (values ?? []).join(", ");
}

function parseValues(value: string) {
  return value
    .split(",")
    .map((item) => item.trim())
    .slice(0, 32);
}

function normalizedValues(values: string[] | undefined) {
  return (values ?? [])
    .map((item) => item.trim())
    .filter(Boolean)
    .slice(0, 32);
}

export function SchedulingAffinityFields({
  preferred,
  antiAffinity,
  onPreferredChange,
  onAntiAffinityChange,
}: {
  preferred: PreferredNodeAffinityDraft[];
  antiAffinity: SameApplicationPodAntiAffinityDraft[];
  onPreferredChange: (value: PreferredNodeAffinityDraft[]) => void;
  onAntiAffinityChange: (value: SameApplicationPodAntiAffinityDraft[]) => void;
}) {
  const updatePreferred = (
    termIndex: number,
    update: (term: PreferredNodeAffinityDraft) => PreferredNodeAffinityDraft,
  ) =>
    onPreferredChange(
      preferred.map((term, index) =>
        index === termIndex ? update(term) : term,
      ),
    );

  // Row identity that survives a removal; see useRowKeys.
  const presetKeys = useRowKeys(antiAffinity.length);

  return (
    <>
      <div className="col-[1/-1]">
        <CardHeader>
          <div>
            <h3>Preferred node affinity</h3>
            <p>
              Add up to 16 weighted terms. Every expression in a term must
              match; profile consumers only see this as read-only effective
              placement.
            </p>
          </div>
          <Button
            type="button"
            variant="secondary"
            disabled={preferred.length >= 16}
            onClick={() =>
              onPreferredChange([
                ...preferred,
                {
                  weight: 50,
                  requirements: [{ key: "", operator: "In", values: [""] }],
                },
              ])
            }
          >
            Add preferred term
          </Button>
        </CardHeader>
        {preferred.map((term, termIndex) => (
          <FormGrid as="fieldset" key={termIndex}>
            <legend>Preferred term {termIndex + 1}</legend>
            <Field label={`Preferred term ${termIndex + 1} weight`} required>
              <input
                type="number"
                min={1}
                max={100}
                required
                value={term.weight}
                onChange={(event) =>
                  updatePreferred(termIndex, (current) => ({
                    ...current,
                    weight: Number(event.target.value),
                  }))
                }
              />
            </Field>
            <ButtonRow>
              <Button
                type="button"
                variant="secondary"
                disabled={term.requirements.length >= 32}
                onClick={() =>
                  updatePreferred(termIndex, (current) => ({
                    ...current,
                    requirements: [
                      ...current.requirements,
                      { key: "", operator: "In", values: [""] },
                    ],
                  }))
                }
              >
                Add expression
              </Button>
              <Button
                type="button"
                variant="ghost"
                onClick={() =>
                  onPreferredChange(
                    preferred.filter((_, index) => index !== termIndex),
                  )
                }
              >
                Remove term
              </Button>
            </ButtonRow>
            {term.requirements.map((requirement, requirementIndex) => (
              <FormGrid key={requirementIndex} className="col-[1/-1]">
                <Field
                  label={`Term ${termIndex + 1} expression ${requirementIndex + 1} key`}
                  required
                >
                  <input
                    required
                    maxLength={253}
                    value={requirement.key}
                    onChange={(event) =>
                      updatePreferred(termIndex, (current) => ({
                        ...current,
                        requirements: current.requirements.map((item, index) =>
                          index === requirementIndex
                            ? { ...item, key: event.target.value }
                            : item,
                        ),
                      }))
                    }
                  />
                </Field>
                <Field
                  label={`Term ${termIndex + 1} expression ${requirementIndex + 1} operator`}
                  required
                >
                  <Select
                    value={requirement.operator}
                    onChange={(event) => {
                      const operator = event.target
                        .value as SchedulingRequirementDraft["operator"];
                      updatePreferred(termIndex, (current) => ({
                        ...current,
                        requirements: current.requirements.map((item, index) =>
                          index === requirementIndex
                            ? {
                                key: item.key,
                                operator,
                                ...(needsValues(operator)
                                  ? { values: item.values ?? [] }
                                  : {}),
                              }
                            : item,
                        ),
                      }));
                    }}
                  >
                    {operators.map((operator) => (
                      <option key={operator}>{operator}</option>
                    ))}
                  </Select>
                </Field>
                {needsValues(requirement.operator) ? (
                  <Field
                    label={`Term ${termIndex + 1} expression ${requirementIndex + 1} values`}
                    hint="Comma-separated; at most 32 values. Gt and Lt require one integer."
                    required
                  >
                    <input
                      required
                      value={valuesText(requirement.values)}
                      onChange={(event) =>
                        updatePreferred(termIndex, (current) => ({
                          ...current,
                          requirements: current.requirements.map(
                            (item, index) =>
                              index === requirementIndex
                                ? {
                                    ...item,
                                    values: parseValues(event.target.value),
                                  }
                                : item,
                          ),
                        }))
                      }
                      onBlur={() =>
                        updatePreferred(termIndex, (current) => ({
                          ...current,
                          requirements: current.requirements.map(
                            (item, index) =>
                              index === requirementIndex
                                ? {
                                    ...item,
                                    values: normalizedValues(item.values),
                                  }
                                : item,
                          ),
                        }))
                      }
                    />
                  </Field>
                ) : null}
                <Button
                  type="button"
                  variant="ghost"
                  disabled={term.requirements.length <= 1}
                  onClick={() =>
                    updatePreferred(termIndex, (current) => ({
                      ...current,
                      requirements: current.requirements.filter(
                        (_, index) => index !== requirementIndex,
                      ),
                    }))
                  }
                >
                  Remove expression {requirementIndex + 1}
                </Button>
              </FormGrid>
            ))}
          </FormGrid>
        ))}
      </div>

      <div className="col-[1/-1]">
        <CardHeader>
          <div>
            <h3>Same-App pod anti-affinity</h3>
            <p>
              Choose only enforcement and topology. Kuberploy derives the exact
              current App selector; no label selector or other App identity is
              accepted.
            </p>
          </div>
          <Button
            type="button"
            variant="secondary"
            disabled={antiAffinity.length >= 16}
            onClick={() =>
              onAntiAffinityChange([
                ...antiAffinity,
                {
                  enforcement: "required",
                  topologyKey: "kubernetes.io/hostname",
                },
              ])
            }
          >
            Add anti-affinity preset
          </Button>
        </CardHeader>
        {antiAffinity.map((preset, index) => (
          <FormGrid as="fieldset" key={presetKeys.keyAt(index)}>
            <legend>Anti-affinity preset {index + 1}</legend>
            <Field label={`Preset ${index + 1} enforcement`} required>
              <Select
                value={preset.enforcement}
                onChange={(event) => {
                  const enforcement = event.target.value as
                    "required" | "preferred";
                  onAntiAffinityChange(
                    antiAffinity.map((item, itemIndex) =>
                      itemIndex === index
                        ? {
                            enforcement,
                            topologyKey: item.topologyKey,
                            ...(enforcement === "preferred"
                              ? { weight: item.weight ?? 100 }
                              : {}),
                          }
                        : item,
                    ),
                  );
                }}
              >
                <option value="required">Required</option>
                <option value="preferred">Preferred</option>
              </Select>
            </Field>
            <Field label={`Preset ${index + 1} topology key`} required>
              <input
                required
                maxLength={253}
                value={preset.topologyKey}
                onChange={(event) =>
                  onAntiAffinityChange(
                    antiAffinity.map((item, itemIndex) =>
                      itemIndex === index
                        ? { ...item, topologyKey: event.target.value }
                        : item,
                    ),
                  )
                }
              />
            </Field>
            {preset.enforcement === "preferred" ? (
              <Field label={`Preset ${index + 1} weight`} required>
                <input
                  type="number"
                  min={1}
                  max={100}
                  required
                  value={preset.weight ?? 100}
                  onChange={(event) =>
                    onAntiAffinityChange(
                      antiAffinity.map((item, itemIndex) =>
                        itemIndex === index
                          ? { ...item, weight: Number(event.target.value) }
                          : item,
                      ),
                    )
                  }
                />
              </Field>
            ) : null}
            <Button
              type="button"
              variant="ghost"
              onClick={() => {
                presetKeys.removeAt(index);
                onAntiAffinityChange(
                  antiAffinity.filter((_, itemIndex) => itemIndex !== index),
                );
              }}
            >
              Remove preset {index + 1}
            </Button>
          </FormGrid>
        ))}
      </div>
    </>
  );
}
