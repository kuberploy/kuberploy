import { selectOption } from "../test/selectOption";
import { useState } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import {
  SchedulingAffinityFields,
  type PreferredNodeAffinityDraft,
  type SameApplicationPodAntiAffinityDraft,
} from "./SchedulingAffinityFields";

afterEach(cleanup);

function Harness({
  initialPreferred = [],
  initialAntiAffinity = [],
}: {
  initialPreferred?: PreferredNodeAffinityDraft[];
  initialAntiAffinity?: SameApplicationPodAntiAffinityDraft[];
}) {
  const [preferred, setPreferred] = useState(initialPreferred);
  const [antiAffinity, setAntiAffinity] = useState(initialAntiAffinity);
  return (
    <form>
      <SchedulingAffinityFields
        preferred={preferred}
        antiAffinity={antiAffinity}
        onPreferredChange={setPreferred}
        onAntiAffinityChange={setAntiAffinity}
      />
      <output aria-label="affinity draft">
        {JSON.stringify({ preferred, antiAffinity })}
      </output>
    </form>
  );
}

describe("structured scheduling affinity fields", () => {
  it("round-trips bounded multi-expression and closed anti-affinity drafts", () => {
    render(
      <Harness
        initialPreferred={[
          {
            weight: 75,
            requirements: [
              {
                key: "topology.kubernetes.io/zone",
                operator: "In",
                values: ["zone-a", "zone-b"],
              },
              { key: "kubernetes.io/arch", operator: "Exists" },
            ],
          },
        ]}
        initialAntiAffinity={[
          {
            enforcement: "preferred",
            topologyKey: "kubernetes.io/hostname",
            weight: 40,
          },
        ]}
      />,
    );
    expect(screen.getByLabelText(/^Preferred term 1 weight/)).toHaveValue(75);
    expect(screen.getByLabelText(/^Term 1 expression 1 key/)).toHaveValue(
      "topology.kubernetes.io/zone",
    );
    expect(screen.getByLabelText(/^Term 1 expression 1 values/)).toHaveValue(
      "zone-a, zone-b",
    );
    expect(screen.getByLabelText(/^Term 1 expression 2 operator/)).toHaveValue(
      "Exists",
    );
    expect(screen.getByLabelText(/^Preset 1 enforcement/)).toHaveValue(
      "preferred",
    );
    expect(screen.getByLabelText(/^Preset 1 weight/)).toHaveValue(40);
    expect(screen.getByLabelText("affinity draft")).toHaveTextContent(
      '"topologyKey":"kubernetes.io/hostname","weight":40',
    );
    expect(screen.queryByLabelText(/label selector/i)).not.toBeInTheDocument();
    expect(
      screen.queryByLabelText(/application identity/i),
    ).not.toBeInTheDocument();
  });

  it("adds structured terms and never exposes raw selector authority", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(
      screen.getByRole("button", { name: "Add preferred term" }),
    );
    await user.clear(screen.getByLabelText(/^Term 1 expression 1 key/));
    await user.type(
      screen.getByLabelText(/^Term 1 expression 1 key/),
      "kubernetes.io/arch",
    );
    await user.clear(screen.getByLabelText(/^Term 1 expression 1 values/));
    await user.type(
      screen.getByLabelText(/^Term 1 expression 1 values/),
      "amd64, arm64",
    );
    await user.click(
      screen.getByRole("button", { name: "Add anti-affinity preset" }),
    );
    await selectOption(
      screen.getByLabelText(/^Preset 1 enforcement/),
      "preferred",
    );
    expect(screen.getByLabelText("affinity draft")).toHaveTextContent(
      '"values":["amd64","arm64"]',
    );
    expect(screen.getByLabelText("affinity draft")).toHaveTextContent(
      '"enforcement":"preferred","topologyKey":"kubernetes.io/hostname","weight":100',
    );
    expect(screen.queryByText(/NodePool|NodeClass|taint mutation/i)).toBeNull();
  });
});
