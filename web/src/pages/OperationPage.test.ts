import { describe, expect, it } from "vitest";
import { operationStageTitle, operationTitle } from "./OperationPage";

describe("App operation labels", () => {
  it("hides internal deployment and worker names", () => {
    expect(operationTitle("deployment.git-write")).toBe("Apply App change");
    expect(operationTitle("deployment.config-draft-save")).toBe(
      "Save App draft",
    );
    expect(operationTitle("deployment.clone-draft")).toBe("Clone App draft");
    expect(operationStageTitle("git-write")).toBe("Save desired state");
    expect(operationStageTitle("reconciling")).toBe("Synchronize App");
    expect(operationStageTitle("healthy")).toBe("Verify App health");
  });
});
