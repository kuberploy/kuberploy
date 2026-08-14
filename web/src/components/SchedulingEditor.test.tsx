import { useState } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { parse } from "yaml";
import { afterEach, describe, expect, it } from "vitest";
import {
  SchedulingEditor,
  type SchedulingEditorValue,
} from "./SchedulingEditor";

afterEach(cleanup);

const applicationId = "11111111-1111-4111-8111-111111111111";

function Harness({ initial }: { initial?: Partial<SchedulingEditorValue> }) {
  const [value, setValue] = useState<SchedulingEditorValue>({
    nodeSelectorYaml: "{}",
    affinityYaml: "{}",
    topologySpreadYaml: "[]",
    tolerationsYaml: "[]",
    priorityClassName: "",
    ...initial,
  });
  return (
    <>
      <SchedulingEditor
        value={value}
        applicationId={applicationId}
        onChange={setValue}
      />
      <output aria-label="scheduling value">{JSON.stringify(value)}</output>
    </>
  );
}

describe("per-service scheduling editor", () => {
  it("shows structured controls without raw YAML or selector authority", () => {
    render(
      <Harness
        initial={{
          nodeSelectorYaml: "kubernetes.io/arch: amd64",
          affinityYaml: `nodeAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    nodeSelectorTerms:
      - matchExpressions:
          - key: karpenter.sh/capacity-type
            operator: In
            values: [on-demand]
podAntiAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    - topologyKey: kubernetes.io/hostname
      labelSelector:
        matchLabels:
          kuberploy.io/application: ${applicationId}`,
          tolerationsYaml: `- key: dedicated
  operator: Equal
  value: application
  effect: NoSchedule`,
        }}
      />,
    );

    expect(screen.getByLabelText("Node selector 1 key")).toHaveValue(
      "kubernetes.io/arch",
    );
    expect(screen.getByLabelText("Expression 1 label key")).toHaveValue(
      "karpenter.sh/capacity-type",
    );
    expect(
      screen.getByLabelText("Same-service pod anti-affinity 1 topology key"),
    ).toHaveValue("kubernetes.io/hostname");
    expect(screen.getByLabelText("Toleration 1 value")).toHaveValue(
      "application",
    );
    expect(screen.queryByText(/YAML map|YAML list/i)).not.toBeInTheDocument();
    expect(
      screen.queryByLabelText(/application selector|namespace/i),
    ).toBeNull();
    expect(screen.getByText(/system-\* classes are reserved/i)).toBeVisible();
  });

  it("derives exact application selectors for pod and topology rules", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.click(screen.getAllByRole("button", { name: "Add rule" })[0]);
    await user.click(screen.getByRole("button", { name: "Add constraint" }));

    const serialized = JSON.parse(
      screen.getByLabelText("scheduling value").textContent ?? "{}",
    ) as SchedulingEditorValue;
    const affinity = parse(serialized.affinityYaml);
    const topology = parse(serialized.topologySpreadYaml);
    expect(
      affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[0]
        .labelSelector,
    ).toEqual({
      matchLabels: { "kuberploy.io/application": applicationId },
    });
    expect(topology[0].labelSelector).toEqual({
      matchLabels: { "kuberploy.io/application": applicationId },
    });
  });

  it("removes minDomains when topology spread becomes soft", async () => {
    const user = userEvent.setup();
    render(
      <Harness
        initial={{
          topologySpreadYaml: `- maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: DoNotSchedule
  minDomains: 2
  labelSelector:
    matchLabels:
      kuberploy.io/application: ${applicationId}`,
        }}
      />,
    );

    const minDomains = screen.getByLabelText("Topology spread 1 min domains");
    expect(minDomains).toBeEnabled();
    await user.selectOptions(
      screen.getByLabelText("Topology spread 1 unsatisfiable"),
      "ScheduleAnyway",
    );
    expect(minDomains).toBeDisabled();

    const serialized = JSON.parse(
      screen.getByLabelText("scheduling value").textContent ?? "{}",
    ) as SchedulingEditorValue;
    expect(parse(serialized.topologySpreadYaml)[0]).not.toHaveProperty(
      "minDomains",
    );
  });

  it("adds and removes direct node placement values", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Add label" }));
    await user.type(screen.getByLabelText("Node selector 1 key"), "pool");
    await user.type(screen.getByLabelText("Node selector 1 value"), "worker");
    expect(screen.getByLabelText("scheduling value")).toHaveTextContent(
      "pool: worker",
    );
    await user.click(
      screen.getByRole("button", { name: "Remove node selector 1" }),
    );
    expect(screen.getByLabelText("scheduling value")).toHaveTextContent(
      'nodeSelectorYaml":"{}',
    );
  });
});
