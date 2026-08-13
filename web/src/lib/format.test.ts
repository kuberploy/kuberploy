import { describe, expect, it } from "vitest";
import { canonicalBranchRef, gitRefLabel } from "./format";

describe("Git ref formatting", () => {
  it("shows readable branch and tag names", () => {
    expect(gitRefLabel("refs/heads/main")).toBe("main");
    expect(gitRefLabel("refs/heads/release/next")).toBe("release/next");
    expect(gitRefLabel("refs/tags/v1.0.0")).toBe("v1.0.0");
  });

  it("restores the canonical branch ref used by the API", () => {
    expect(canonicalBranchRef("main")).toBe("refs/heads/main");
    expect(canonicalBranchRef("refs/heads/main")).toBe("refs/heads/main");
  });
});
