import { describe, expect, it } from "vitest";
import { hasHelmApprovalManagementAccess } from "./helmApprovalAccess";

const session = {
  id: "u",
  displayName: "Admin",
  role: "platform-admin",
  authentication: { kind: "session" as const },
};
const grant = [
  {
    scopeType: "platform" as const,
    scopeId: "platform",
    actions: ["helm-approvals:manage"],
  },
];

describe("Helm approval access", () => {
  it("fails closed unless every exact gate holds", () => {
    expect(
      hasHelmApprovalManagementAccess(session, { helmApprovals: true }, grant),
    ).toBe(true);
    expect(
      hasHelmApprovalManagementAccess(session, { helmApprovals: false }, grant),
    ).toBe(false);
    expect(
      hasHelmApprovalManagementAccess(session, { helmApprovals: true }, [
        { ...grant[0], scopeType: "project" },
      ]),
    ).toBe(false);
    expect(
      hasHelmApprovalManagementAccess(
        {
          ...session,
          authentication: {
            kind: "service-account",
            serviceAccountId: "s",
            tokenId: "t",
            scopes: [],
            expiresAt: "x",
          },
        },
        { helmApprovals: true },
        grant,
      ),
    ).toBe(false);
  });
});
