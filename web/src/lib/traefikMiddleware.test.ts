import { describe, expect, it } from "vitest";
import {
  defaultGuidedTraefikMiddleware,
  guidedTraefikMiddlewaresToValue,
  guidedTraefikMiddlewareState,
  traefikMiddlewareKinds,
  validateGuidedTraefikMiddlewares,
  type GuidedTraefikMiddleware,
} from "./traefikMiddleware";

describe("bounded Traefik middleware model", () => {
  it("round-trips every allowlisted family without losing definition or route order", () => {
    const definitions = traefikMiddlewareKinds.map((kind, index) =>
      defaultGuidedTraefikMiddleware(kind, `middleware-${index + 1}`),
    );
    const basicAuth = definitions.find(({ kind }) => kind === "basicAuth");
    if (basicAuth?.kind === "basicAuth") {
      basicAuth.config.secretBindingRef = {
        bindingId: "11111111-1111-4111-8111-111111111111",
        name: "auth-users",
        key: "users",
        version: 3,
      };
    }
    const refs = [
      definitions[5]!.name,
      definitions[0]!.name,
      definitions[12]!.name,
    ];
    expect(validateGuidedTraefikMiddlewares(definitions, refs)).toBeNull();

    const raw = guidedTraefikMiddlewaresToValue(definitions);
    const reparsed = guidedTraefikMiddlewareState(raw, refs);
    expect(reparsed.issue).toBe("");
    expect(reparsed.definitions.map(({ kind }) => kind)).toEqual(
      traefikMiddlewareKinds,
    );
    expect(reparsed.refs).toEqual(refs);
    expect(guidedTraefikMiddlewaresToValue(reparsed.definitions)).toEqual(raw);
  });

  it("rejects duplicate names, duplicate refs, and unresolved refs", () => {
    const headers = defaultGuidedTraefikMiddleware("headers", "security");
    const retry = defaultGuidedTraefikMiddleware("retry", "security");
    expect(validateGuidedTraefikMiddlewares([headers, retry], [])).toMatch(
      /name security is duplicated/i,
    );
    expect(
      validateGuidedTraefikMiddlewares([headers], ["security", "security"]),
    ).toMatch(/reference security is duplicated/i);
    expect(validateGuidedTraefikMiddlewares([headers], ["missing"])).toMatch(
      /reference missing does not resolve/i,
    );
  });

  it("rejects invalid CIDRs and forwarded-IP trust inputs", () => {
    const allow = defaultGuidedTraefikMiddleware("ipAllowList", "office-only");
    const invalid = {
      ...allow,
      config: { sourceRange: ["10.0.0.0/99"] },
    } as GuidedTraefikMiddleware;
    expect(validateGuidedTraefikMiddlewares([invalid], [])).toMatch(
      /explicit CIDR/i,
    );

    const invalidProxy = {
      ...allow,
      config: {
        sourceRange: ["10.0.0.0/8"],
        ipStrategy: { excludedIPs: ["not-an-ip"] },
      },
    } as GuidedTraefikMiddleware;
    expect(validateGuidedTraefikMiddlewares([invalidProxy], [])).toMatch(
      /IP address or CIDR/i,
    );
  });

  it("rejects malformed or non-RE2 regular expressions", () => {
    const redirect = defaultGuidedTraefikMiddleware(
      "redirectRegex",
      "redirect",
    );
    expect(
      validateGuidedTraefikMiddlewares(
        [
          {
            ...redirect,
            config: { regex: "([", replacement: "https://example.com" },
          } as GuidedTraefikMiddleware,
        ],
        [],
      ),
    ).toMatch(/valid regular expression/i);
    expect(
      validateGuidedTraefikMiddlewares(
        [
          {
            ...redirect,
            config: {
              regex: "^(?=https://)",
              replacement: "https://example.com",
            },
          } as GuidedTraefikMiddleware,
        ],
        [],
      ),
    ).toMatch(/RE2 syntax/i);
  });

  it("rejects invalid, duplicate, routing, and secret-shaped header inputs", () => {
    const headers = defaultGuidedTraefikMiddleware("headers", "security");
    for (const [name, expected] of [
      ["Bad Header", /invalid header name/i],
      ["Host", /forbidden/i],
      ["X-API-Key", /forbidden/i],
    ] as const) {
      const candidate = {
        ...headers,
        config: {
          customRequestHeaders: [{ name, value: "literal" }],
        },
      } as GuidedTraefikMiddleware;
      expect(validateGuidedTraefikMiddlewares([candidate], [])).toMatch(
        expected,
      );
    }
    const duplicate = {
      ...headers,
      config: {
        customResponseHeaders: [
          { name: "X-Frame-Options", value: "DENY" },
          { name: "x-frame-options", value: "SAMEORIGIN" },
        ],
      },
    } as GuidedTraefikMiddleware;
    expect(validateGuidedTraefikMiddlewares([duplicate], [])).toMatch(
      /duplicate header name/i,
    );
    const injectedValue = {
      ...headers,
      config: {
        customResponseHeaders: [
          { name: "X-Safe-Header", value: "safe\r\nX-Injected: yes" },
        ],
      },
    } as GuidedTraefikMiddleware;
    expect(validateGuidedTraefikMiddlewares([injectedValue], [])).toMatch(
      /single-line string/i,
    );
  });

  it("classifies unknown but valid opaque fields as Advanced-only without echoing their values", () => {
    const state = guidedTraefikMiddlewareState(
      [
        {
          name: "advanced-headers",
          spec: {
            headers: {
              hostsProxyHeaders: ["X-Trusted-Proxy-Secret-Value"],
            },
          },
        },
      ],
      ["advanced-headers"],
    );
    expect(state.issue).toMatch(/only available in Advanced YAML/i);
    expect(state.issue).not.toContain("X-Trusted-Proxy-Secret-Value");
    expect(state.definitions).toEqual([]);
    expect(state.refs).toEqual([]);
  });

  it("preserves exact reusable refs and rejects BasicAuth plaintext or Secret names", () => {
    const profileRef = {
      profileId: "11111111-1111-4111-8111-111111111111",
      revision: 7,
      specDigest: `sha256:${"a".repeat(64)}`,
      assignmentsDigest: `sha256:${"b".repeat(64)}`,
    };
    const state = guidedTraefikMiddlewareState(
      [
        {
          name: "shared-security",
          profileRef,
          spec: { headers: { frameDeny: true } },
        },
      ],
      ["shared-security"],
    );
    expect(state.issue).toBe("");
    expect(guidedTraefikMiddlewaresToValue(state.definitions)).toEqual([
      {
        name: "shared-security",
        profileRef,
        spec: { headers: { frameDeny: true } },
      },
    ]);

    for (const forbidden of [
      { users: ["admin:$apr1$plaintext"] },
      { secretName: "caller-controlled-secret" },
      {
        secretBindingRef: {
          bindingId: profileRef.profileId,
          name: "auth-users",
          key: "users",
          version: 1,
        },
        targetSecretName: "caller-controlled-secret",
      },
    ]) {
      const rejected = guidedTraefikMiddlewareState(
        [{ name: "login", spec: { basicAuth: forbidden } }],
        [],
      );
      expect(rejected.issue).toMatch(/cannot represent|only available/i);
      expect(rejected.definitions).toEqual([]);
    }
  });
});
