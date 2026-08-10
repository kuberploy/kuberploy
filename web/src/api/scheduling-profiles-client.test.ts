import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./client";

afterEach(() => vi.unstubAllGlobals());

describe("assigned scheduling profile client", () => {
  it("projects only tenant-safe bounded profile fields", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          items: [
            {
              profileId: "11111111-1111-4111-8111-111111111111",
              name: "on-demand",
              revision: 2,
              specDigest: `sha256:${"a".repeat(64)}`,
              assignmentsDigest: `sha256:${"b".repeat(64)}`,
              assignments: [
                {
                  scope: "project",
                  id: "22222222-2222-4222-8222-222222222222",
                },
              ],
              createdBy: "33333333-3333-4333-8333-333333333333",
              providerEndpoint: "https://private.provider.invalid",
              credentials: "secret",
              spec: {
                description: "Stable nodes",
                credentials: "nested-secret",
                pod: {
                  nodeSelector: { "kubernetes.io/arch": "amd64" },
                  preferredNodeAffinity: [
                    {
                      weight: 75,
                      requirements: [
                        {
                          key: "topology.kubernetes.io/zone",
                          operator: "In",
                          values: ["zone-a", "zone-b"],
                          providerEndpoint: "strip-me",
                        },
                      ],
                      labelSelector: { attacker: "other" },
                    },
                  ],
                  sameApplicationPodAntiAffinity: [
                    {
                      enforcement: "preferred",
                      topologyKey: "kubernetes.io/hostname",
                      weight: 40,
                      labelSelector: { attacker: "other" },
                      applicationId: "attacker",
                      namespaceSelector: {},
                    },
                  ],
                  priorityClassName: "normal",
                  providerEndpoint: "https://nested.invalid",
                },
              },
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const result = await api.assignedSchedulingProfiles("env/id");
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/environments/env%2Fid/scheduling-profiles",
    );
    expect(result.items[0]?.spec.pod.nodeSelector).toEqual({
      "kubernetes.io/arch": "amd64",
    });
    expect(result.items[0]?.spec.pod.preferredNodeAffinity).toEqual([
      {
        weight: 75,
        requirements: [
          {
            key: "topology.kubernetes.io/zone",
            operator: "In",
            values: ["zone-a", "zone-b"],
          },
        ],
      },
    ]);
    expect(result.items[0]?.spec.pod.sameApplicationPodAntiAffinity).toEqual([
      {
        enforcement: "preferred",
        topologyKey: "kubernetes.io/hostname",
        weight: 40,
      },
    ]);
    expect(JSON.stringify(result)).not.toMatch(
      /"assignments":|createdBy|providerEndpoint|credentials|labelSelector|applicationId|namespaceSelector|22222222|33333333/,
    );
  });

  it("sends closed platform mutation bodies and encodes exact profile paths", async () => {
    const entry = {
      profile: {
        id: "11111111-1111-4111-8111-111111111111",
        name: "on-demand",
        lifecycle: "active",
        currentRevision: 2,
        createdBy: "33333333-3333-4333-8333-333333333333",
        createdAt: "2026-08-09T00:00:00Z",
        providerEndpoint: "https://private.invalid",
      },
      revision: {
        profileId: "11111111-1111-4111-8111-111111111111",
        revision: 2,
        spec: { pod: { nodeSelector: { arch: "amd64" } } },
        specDigest: `sha256:${"a".repeat(64)}`,
        assignmentsDigest: `sha256:${"b".repeat(64)}`,
        createdBy: "33333333-3333-4333-8333-333333333333",
        assignments: [
          {
            scope: "environment",
            id: "22222222-2222-4222-8222-222222222222",
          },
        ],
        createdAt: "2026-08-09T00:00:00Z",
      },
    };
    const fetchMock = vi.fn().mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify(entry), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    await api.revisePlatformSchedulingProfile(
      "profile/id",
      {
        baseRevision: 1,
        spec: {
          pod: {
            nodeSelector: { arch: "amd64" },
            preferredNodeAffinity: [
              {
                weight: 75,
                requirements: [
                  {
                    key: "kubernetes.io/arch",
                    operator: "In",
                    values: ["amd64"],
                    labelSelector: "strip-me",
                  } as never,
                ],
                providerEndpoint: "strip-me",
              } as never,
            ],
            sameApplicationPodAntiAffinity: [
              {
                enforcement: "required",
                topologyKey: "kubernetes.io/hostname",
                labelSelector: "strip-me",
                applicationId: "strip-me",
              } as never,
            ],
          },
          credentials: "strip-me",
        } as never,
        assignments: [
          {
            scope: "environment",
            id: "22222222-2222-4222-8222-222222222222",
            providerEndpoint: "strip-me",
          } as never,
        ],
      },
      "idem-1",
    );
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/v1/platform/scheduling-profiles/profile%2Fid",
    );
    const options = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(options.method).toBe("PUT");
    expect(JSON.parse(String(options.body))).toEqual({
      baseRevision: 1,
      spec: {
        pod: {
          nodeSelector: { arch: "amd64" },
          preferredNodeAffinity: [
            {
              weight: 75,
              requirements: [
                {
                  key: "kubernetes.io/arch",
                  operator: "In",
                  values: ["amd64"],
                },
              ],
            },
          ],
          sameApplicationPodAntiAffinity: [
            {
              enforcement: "required",
              topologyKey: "kubernetes.io/hostname",
            },
          ],
        },
      },
      assignments: [
        {
          scope: "environment",
          id: "22222222-2222-4222-8222-222222222222",
        },
      ],
    });
    expect(
      JSON.stringify(
        await api.deactivatePlatformSchedulingProfile(
          "profile/id",
          2,
          "idem-2",
        ),
      ),
    ).not.toContain("providerEndpoint");
  });
});
