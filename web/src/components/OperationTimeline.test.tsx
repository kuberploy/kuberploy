import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Operation } from "../api/types";
import { OperationTimeline } from "./OperationTimeline";

describe("OperationTimeline", () => {
  it("shows user-facing App labels instead of internal deployment kinds", () => {
    const operation: Operation = {
      id: "operation-12345678",
      kind: "deployment.git-write",
      status: "succeeded",
      state: "succeeded",
      targetType: "deployment",
      targetId: "deployment-1",
      requestId: "request-1",
      generation: 1,
      progress: [],
      createdAt: "2026-08-31T00:00:00Z",
      updatedAt: "2026-08-31T00:00:01Z",
    };

    render(<OperationTimeline operations={[operation]} />);

    expect(screen.getByText("Apply App change")).toBeInTheDocument();
    expect(screen.queryByText(/Deployment\.git write/)).not.toBeInTheDocument();
  });
});
