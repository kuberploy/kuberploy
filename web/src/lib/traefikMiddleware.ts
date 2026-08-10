export const traefikMiddlewareKinds = [
  "redirectScheme",
  "redirectRegex",
  "addPrefix",
  "stripPrefix",
  "stripPrefixRegex",
  "replacePath",
  "replacePathRegex",
  "headers",
  "rateLimit",
  "inFlightReq",
  "ipAllowList",
  "compress",
  "buffering",
  "retry",
  "basicAuth",
] as const;

export type TraefikMiddlewareKind = (typeof traefikMiddlewareKinds)[number];

export type HeaderEntry = { name: string; value: string };

export type RedirectSchemeConfig = {
  scheme: string;
  port?: string;
  permanent?: boolean;
};

export type RedirectRegexConfig = {
  regex: string;
  replacement: string;
  permanent?: boolean;
};

export type AddPrefixConfig = { prefix: string };
export type StripPrefixConfig = {
  prefixes: string[];
  forceSlash?: boolean;
};
export type StripPrefixRegexConfig = { regex: string[] };
export type ReplacePathConfig = { path: string };
export type ReplacePathRegexConfig = {
  regex: string;
  replacement: string;
};

export type HeadersConfig = {
  customRequestHeaders?: HeaderEntry[];
  customResponseHeaders?: HeaderEntry[];
  accessControlAllowCredentials?: boolean;
  accessControlAllowHeaders?: string[];
  accessControlAllowMethods?: string[];
  accessControlAllowOriginList?: string[];
  accessControlAllowOriginListRegex?: string[];
  accessControlExposeHeaders?: string[];
  accessControlMaxAge?: number;
  addVaryHeader?: boolean;
  allowedHosts?: string[];
  stsSeconds?: number;
  stsIncludeSubdomains?: boolean;
  stsPreload?: boolean;
  forceSTSHeader?: boolean;
  frameDeny?: boolean;
  customFrameOptionsValue?: string;
  contentTypeNosniff?: boolean;
  browserXssFilter?: boolean;
  customBrowserXSSValue?: string;
  contentSecurityPolicy?: string;
  contentSecurityPolicyReportOnly?: string;
  referrerPolicy?: string;
  permissionsPolicy?: string;
  isDevelopment?: boolean;
};

export type RateLimitConfig = {
  average: number;
  period?: string;
  burst?: number;
};

export type InFlightReqConfig = { amount: number };

export type IPStrategyConfig = {
  depth?: number;
  excludedIPs?: string[];
  ipv6Subnet?: number;
};

export type IPAllowListConfig = {
  sourceRange: string[];
  ipStrategy?: IPStrategyConfig;
};

export type CompressConfig = {
  excludedContentTypes?: string[];
  includedContentTypes?: string[];
  minResponseBodyBytes?: number;
  defaultEncoding?: string;
  encodings?: string[];
};

export type BufferingConfig = {
  maxRequestBodyBytes?: number;
  memRequestBodyBytes?: number;
  maxResponseBodyBytes?: number;
  memResponseBodyBytes?: number;
  retryExpression?: string;
};

export type RetryConfig = {
  attempts: number;
  initialInterval?: string;
};

export type MiddlewareProfileRef = {
  profileId: string;
  revision: number;
  specDigest: string;
  assignmentsDigest: string;
};

export type BasicAuthConfig = {
  secretBindingRef: {
    bindingId: string;
    name: string;
    key: "users";
    version: number;
  };
  removeHeader?: boolean;
  headerField?: string;
};

export type TraefikMiddlewareConfigByKind = {
  redirectScheme: RedirectSchemeConfig;
  redirectRegex: RedirectRegexConfig;
  addPrefix: AddPrefixConfig;
  stripPrefix: StripPrefixConfig;
  stripPrefixRegex: StripPrefixRegexConfig;
  replacePath: ReplacePathConfig;
  replacePathRegex: ReplacePathRegexConfig;
  headers: HeadersConfig;
  rateLimit: RateLimitConfig;
  inFlightReq: InFlightReqConfig;
  ipAllowList: IPAllowListConfig;
  compress: CompressConfig;
  buffering: BufferingConfig;
  retry: RetryConfig;
  basicAuth: BasicAuthConfig;
};

export type GuidedTraefikMiddleware = {
  [Kind in TraefikMiddlewareKind]: {
    id?: string;
    profileRef?: MiddlewareProfileRef;
    name: string;
    kind: Kind;
    config: TraefikMiddlewareConfigByKind[Kind];
  };
}[TraefikMiddlewareKind];

export type GuidedTraefikMiddlewareState = {
  definitions: GuidedTraefikMiddleware[];
  refs: string[];
  /**
   * Non-empty when valid AppConfig data cannot be represented without loss by
   * the bounded Guided editor. Callers must preserve the original YAML paths.
   */
  issue: string;
};

const dnsLabelPattern = /^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/;
const headerNamePattern = /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/;
const goDurationPattern = /^(?:[0-9]+(?:ns|us|µs|ms|s|m|h))+$/;
const forbiddenGuidedHeaders = new Set([
  "authorization",
  "connection",
  "content-length",
  "cookie",
  "host",
  "proxy-authorization",
  "proxy-connection",
  "set-cookie",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
  "x-forwarded-for",
  "x-forwarded-host",
  "x-forwarded-port",
  "x-forwarded-proto",
]);

const knownKeys: Record<TraefikMiddlewareKind, readonly string[]> = {
  redirectScheme: ["scheme", "port", "permanent"],
  redirectRegex: ["regex", "replacement", "permanent"],
  addPrefix: ["prefix"],
  stripPrefix: ["prefixes", "forceSlash"],
  stripPrefixRegex: ["regex"],
  replacePath: ["path"],
  replacePathRegex: ["regex", "replacement"],
  headers: [
    "customRequestHeaders",
    "customResponseHeaders",
    "accessControlAllowCredentials",
    "accessControlAllowHeaders",
    "accessControlAllowMethods",
    "accessControlAllowOriginList",
    "accessControlAllowOriginListRegex",
    "accessControlExposeHeaders",
    "accessControlMaxAge",
    "addVaryHeader",
    "allowedHosts",
    "stsSeconds",
    "stsIncludeSubdomains",
    "stsPreload",
    "forceSTSHeader",
    "frameDeny",
    "customFrameOptionsValue",
    "contentTypeNosniff",
    "browserXssFilter",
    "customBrowserXSSValue",
    "contentSecurityPolicy",
    "contentSecurityPolicyReportOnly",
    "referrerPolicy",
    "permissionsPolicy",
    "isDevelopment",
  ],
  rateLimit: ["average", "period", "burst"],
  inFlightReq: ["amount"],
  ipAllowList: ["sourceRange", "ipStrategy"],
  compress: [
    "excludedContentTypes",
    "includedContentTypes",
    "minResponseBodyBytes",
    "defaultEncoding",
    "encodings",
  ],
  buffering: [
    "maxRequestBodyBytes",
    "memRequestBodyBytes",
    "maxResponseBodyBytes",
    "memResponseBodyBytes",
    "retryExpression",
  ],
  retry: ["attempts", "initialInterval"],
  basicAuth: ["secretBindingRef", "removeHeader", "headerField"],
};

function isObject(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function assertKnownKeys(
  value: Record<string, unknown>,
  keys: readonly string[],
  label: string,
) {
  const unknown = Object.keys(value).find((key) => !keys.includes(key));
  if (unknown) {
    throw new Error(`${label}.${unknown} is only available in Advanced YAML`);
  }
}

function requiredString(value: unknown, label: string, maximum = 2048): string {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    value.length > maximum ||
    /[\u0000-\u001f\u007f\u2028\u2029]/.test(value)
  ) {
    throw new Error(`${label} must be a non-empty single-line string`);
  }
  return value;
}

function optionalString(
  value: unknown,
  label: string,
  maximum = 2048,
): string | undefined {
  if (value === undefined) return undefined;
  return requiredString(value, label, maximum);
}

function optionalBoolean(value: unknown, label: string): boolean | undefined {
  if (value === undefined) return undefined;
  if (typeof value !== "boolean") throw new Error(`${label} must be boolean`);
  return value;
}

function boundedInteger(
  value: unknown,
  label: string,
  minimum: number,
  maximum: number,
  required = false,
): number | undefined {
  if (value === undefined && !required) return undefined;
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value < minimum ||
    value > maximum
  ) {
    throw new Error(
      `${label} must be an integer from ${minimum} to ${maximum}`,
    );
  }
  return value;
}

function stringArray(
  value: unknown,
  label: string,
  options: { required?: boolean; maximum?: number } = {},
): string[] | undefined {
  if (value === undefined && !options.required) return undefined;
  if (!Array.isArray(value)) throw new Error(`${label} must be a string list`);
  if (options.required && value.length === 0)
    throw new Error(`${label} must not be empty`);
  const maximum = options.maximum ?? 64;
  if (value.length > maximum)
    throw new Error(`${label} must contain at most ${maximum} values`);
  const result = value.map((item, index) =>
    requiredString(item, `${label}[${index}]`),
  );
  if (new Set(result).size !== result.length)
    throw new Error(`${label} must not contain duplicates`);
  return result;
}

function headerEntries(
  value: unknown,
  label: string,
): HeaderEntry[] | undefined {
  if (value === undefined) return undefined;
  if (!isObject(value)) throw new Error(`${label} must be a header map`);
  const entries = Object.entries(value);
  if (entries.length > 64)
    throw new Error(`${label} must contain at most 64 headers`);
  return entries.map(([name, rawValue]) => {
    if (
      name.length > 128 ||
      !headerNamePattern.test(name) ||
      forbiddenGuidedHeaders.has(name.toLowerCase()) ||
      /(?:api[-_]?key|password|secret|token)/i.test(name)
    ) {
      throw new Error(`${label} contains a forbidden or invalid header name`);
    }
    const headerValue = requiredString(rawValue, `${label}.${name}`, 8192);
    return { name, value: headerValue };
  });
}

function isMiddlewareKind(value: string): value is TraefikMiddlewareKind {
  return (traefikMiddlewareKinds as readonly string[]).includes(value);
}

function validatePath(value: string, label: string) {
  requiredString(value, label);
  if (!value.startsWith("/") || value.length > 2048)
    throw new Error(
      `${label} must start with / and be at most 2048 characters`,
    );
}

function validateRE2(value: string, label: string) {
  requiredString(value, label);
  if (
    /\(\?(?:[=!]|<[=!])/.test(value) ||
    /\\(?:[1-9]|k<)/.test(value) ||
    /\(\?>/.test(value)
  ) {
    throw new Error(
      `${label} must use RE2 syntax without lookaround or backreferences`,
    );
  }
  try {
    new RegExp(value);
  } catch {
    throw new Error(`${label} must be a valid regular expression`);
  }
}

function validateDuration(value: string, label: string) {
  if (value.length > 64 || !goDurationPattern.test(value))
    throw new Error(
      `${label} must be a positive Go duration such as 1s or 250ms`,
    );
}

function isIPv4(value: string): boolean {
  const parts = value.split(".");
  return (
    parts.length === 4 &&
    parts.every(
      (part) => /^(?:0|[1-9][0-9]{0,2})$/.test(part) && Number(part) <= 255,
    )
  );
}

function isIPv6(value: string): boolean {
  if (!value || value.includes("%") || value.split("::").length > 2)
    return false;
  let normalized = value;
  const lastColon = normalized.lastIndexOf(":");
  const tail = normalized.slice(lastColon + 1);
  if (tail.includes(".")) {
    if (!isIPv4(tail) || lastColon < 0) return false;
    normalized = `${normalized.slice(0, lastColon)}:v4:v4`;
  }
  const compressed = normalized.includes("::");
  const [left = "", right = ""] = normalized.split("::");
  const leftParts = left ? left.split(":") : [];
  const rightParts = right ? right.split(":") : [];
  const validParts = [...leftParts, ...rightParts].every((part) =>
    /^(?:[0-9A-Fa-f]{1,4}|v4)$/.test(part),
  );
  if (!validParts) return false;
  const count = [...leftParts, ...rightParts].reduce(
    (total, part) => total + (part === "v4" ? 1 : 1),
    0,
  );
  return compressed ? count < 8 : count === 8;
}

function isIP(value: string): boolean {
  return isIPv4(value) || isIPv6(value);
}

function isCIDR(value: string): boolean {
  const slash = value.lastIndexOf("/");
  if (slash <= 0 || slash === value.length - 1) return false;
  const address = value.slice(0, slash);
  const rawPrefix = value.slice(slash + 1);
  if (!/^(?:0|[1-9][0-9]{0,2})$/.test(rawPrefix)) return false;
  const prefix = Number(rawPrefix);
  return isIPv4(address)
    ? prefix <= 32
    : isIPv6(address)
      ? prefix <= 128
      : false;
}

function validateHeaderEntryList(
  entries: HeaderEntry[] | undefined,
  label: string,
) {
  if (!entries) return;
  if (entries.length > 64)
    throw new Error(`${label} must contain at most 64 headers`);
  const seen = new Set<string>();
  for (const entry of entries) {
    const key = entry.name.toLowerCase();
    if (
      entry.name.length > 128 ||
      !headerNamePattern.test(entry.name) ||
      forbiddenGuidedHeaders.has(key) ||
      /(?:api[-_]?key|password|secret|token)/i.test(key)
    ) {
      throw new Error(`${label} contains a forbidden or invalid header name`);
    }
    if (seen.has(key))
      throw new Error(`${label} contains a duplicate header name`);
    seen.add(key);
    requiredString(entry.value, `${label}.${entry.name}`, 8192);
  }
}

function validateStringList(
  values: string[] | undefined,
  label: string,
  validate?: (value: string, label: string) => void,
  required = false,
) {
  if (!values) {
    if (required) throw new Error(`${label} must not be empty`);
    return;
  }
  if ((required && values.length === 0) || values.length > 64)
    throw new Error(
      required
        ? `${label} must contain between 1 and 64 values`
        : `${label} must contain at most 64 values`,
    );
  if (new Set(values).size !== values.length)
    throw new Error(`${label} must not contain duplicates`);
  values.forEach((value, index) => {
    requiredString(value, `${label}[${index}]`);
    validate?.(value, `${label}[${index}]`);
  });
}

function validateHostname(value: string, label: string) {
  if (
    value.length > 253 ||
    !/^(?:[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/.test(
      value.toLowerCase(),
    )
  ) {
    throw new Error(`${label} must be a hostname without a wildcard`);
  }
}

function validateOrigin(value: string, label: string) {
  if (value === "*" || value === "null") return;
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error(`${label} must be *, null, or an explicit HTTP(S) origin`);
  }
  if (
    !["http:", "https:"].includes(parsed.protocol) ||
    parsed.username ||
    parsed.password ||
    parsed.pathname !== "/" ||
    parsed.search ||
    parsed.hash
  ) {
    throw new Error(`${label} must be *, null, or an explicit HTTP(S) origin`);
  }
}

function validateContentType(value: string, label: string) {
  if (
    value.length > 255 ||
    !/^[A-Za-z0-9!#$&^_.+-]+\/[A-Za-z0-9!#$&^_.+*-]+$/.test(value)
  )
    throw new Error(`${label} must be a media type such as application/json`);
}

function validateEncoding(value: string, label: string) {
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$/.test(value))
    throw new Error(`${label} must be a response encoding token`);
}

function configForKind(
  kind: TraefikMiddlewareKind,
  raw: Record<string, unknown>,
): TraefikMiddlewareConfigByKind[TraefikMiddlewareKind] {
  assertKnownKeys(raw, knownKeys[kind], kind);
  switch (kind) {
    case "redirectScheme":
      return {
        scheme: requiredString(raw.scheme, `${kind}.scheme`, 32),
        port: optionalString(raw.port, `${kind}.port`, 5),
        permanent: optionalBoolean(raw.permanent, `${kind}.permanent`),
      };
    case "redirectRegex":
      return {
        regex: requiredString(raw.regex, `${kind}.regex`),
        replacement: requiredString(raw.replacement, `${kind}.replacement`),
        permanent: optionalBoolean(raw.permanent, `${kind}.permanent`),
      };
    case "addPrefix":
      return { prefix: requiredString(raw.prefix, `${kind}.prefix`) };
    case "stripPrefix":
      return {
        prefixes: stringArray(raw.prefixes, `${kind}.prefixes`, {
          required: true,
        })!,
        forceSlash: optionalBoolean(raw.forceSlash, `${kind}.forceSlash`),
      };
    case "stripPrefixRegex":
      return {
        regex: stringArray(raw.regex, `${kind}.regex`, { required: true })!,
      };
    case "replacePath":
      return { path: requiredString(raw.path, `${kind}.path`) };
    case "replacePathRegex":
      return {
        regex: requiredString(raw.regex, `${kind}.regex`),
        replacement: requiredString(raw.replacement, `${kind}.replacement`),
      };
    case "headers":
      return {
        customRequestHeaders: headerEntries(
          raw.customRequestHeaders,
          `${kind}.customRequestHeaders`,
        ),
        customResponseHeaders: headerEntries(
          raw.customResponseHeaders,
          `${kind}.customResponseHeaders`,
        ),
        accessControlAllowCredentials: optionalBoolean(
          raw.accessControlAllowCredentials,
          `${kind}.accessControlAllowCredentials`,
        ),
        accessControlAllowHeaders: stringArray(
          raw.accessControlAllowHeaders,
          `${kind}.accessControlAllowHeaders`,
        ),
        accessControlAllowMethods: stringArray(
          raw.accessControlAllowMethods,
          `${kind}.accessControlAllowMethods`,
        ),
        accessControlAllowOriginList: stringArray(
          raw.accessControlAllowOriginList,
          `${kind}.accessControlAllowOriginList`,
        ),
        accessControlAllowOriginListRegex: stringArray(
          raw.accessControlAllowOriginListRegex,
          `${kind}.accessControlAllowOriginListRegex`,
        ),
        accessControlExposeHeaders: stringArray(
          raw.accessControlExposeHeaders,
          `${kind}.accessControlExposeHeaders`,
        ),
        accessControlMaxAge: boundedInteger(
          raw.accessControlMaxAge,
          `${kind}.accessControlMaxAge`,
          0,
          86_400,
        ),
        addVaryHeader: optionalBoolean(
          raw.addVaryHeader,
          `${kind}.addVaryHeader`,
        ),
        allowedHosts: stringArray(raw.allowedHosts, `${kind}.allowedHosts`),
        stsSeconds: boundedInteger(
          raw.stsSeconds,
          `${kind}.stsSeconds`,
          0,
          63_072_000,
        ),
        stsIncludeSubdomains: optionalBoolean(
          raw.stsIncludeSubdomains,
          `${kind}.stsIncludeSubdomains`,
        ),
        stsPreload: optionalBoolean(raw.stsPreload, `${kind}.stsPreload`),
        forceSTSHeader: optionalBoolean(
          raw.forceSTSHeader,
          `${kind}.forceSTSHeader`,
        ),
        frameDeny: optionalBoolean(raw.frameDeny, `${kind}.frameDeny`),
        customFrameOptionsValue: optionalString(
          raw.customFrameOptionsValue,
          `${kind}.customFrameOptionsValue`,
        ),
        contentTypeNosniff: optionalBoolean(
          raw.contentTypeNosniff,
          `${kind}.contentTypeNosniff`,
        ),
        browserXssFilter: optionalBoolean(
          raw.browserXssFilter,
          `${kind}.browserXssFilter`,
        ),
        customBrowserXSSValue: optionalString(
          raw.customBrowserXSSValue,
          `${kind}.customBrowserXSSValue`,
        ),
        contentSecurityPolicy: optionalString(
          raw.contentSecurityPolicy,
          `${kind}.contentSecurityPolicy`,
          8192,
        ),
        contentSecurityPolicyReportOnly: optionalString(
          raw.contentSecurityPolicyReportOnly,
          `${kind}.contentSecurityPolicyReportOnly`,
          8192,
        ),
        referrerPolicy: optionalString(
          raw.referrerPolicy,
          `${kind}.referrerPolicy`,
        ),
        permissionsPolicy: optionalString(
          raw.permissionsPolicy,
          `${kind}.permissionsPolicy`,
          8192,
        ),
        isDevelopment: optionalBoolean(
          raw.isDevelopment,
          `${kind}.isDevelopment`,
        ),
      };
    case "rateLimit":
      return {
        average: boundedInteger(
          raw.average,
          `${kind}.average`,
          0,
          1_000_000,
          true,
        )!,
        period: optionalString(raw.period, `${kind}.period`, 64),
        burst: boundedInteger(raw.burst, `${kind}.burst`, 0, 1_000_000),
      };
    case "inFlightReq":
      return {
        amount: boundedInteger(
          raw.amount,
          `${kind}.amount`,
          1,
          1_000_000,
          true,
        )!,
      };
    case "ipAllowList": {
      const strategy = raw.ipStrategy;
      let ipStrategy: IPStrategyConfig | undefined;
      if (strategy !== undefined) {
        if (!isObject(strategy))
          throw new Error(`${kind}.ipStrategy must be an object`);
        assertKnownKeys(
          strategy,
          ["depth", "excludedIPs", "ipv6Subnet"],
          `${kind}.ipStrategy`,
        );
        ipStrategy = {
          depth: boundedInteger(
            strategy.depth,
            `${kind}.ipStrategy.depth`,
            0,
            100,
          ),
          excludedIPs: stringArray(
            strategy.excludedIPs,
            `${kind}.ipStrategy.excludedIPs`,
          ),
          ipv6Subnet: boundedInteger(
            strategy.ipv6Subnet,
            `${kind}.ipStrategy.ipv6Subnet`,
            0,
            128,
          ),
        };
      }
      return {
        sourceRange: stringArray(raw.sourceRange, `${kind}.sourceRange`, {
          required: true,
        })!,
        ipStrategy,
      };
    }
    case "compress":
      return {
        excludedContentTypes: stringArray(
          raw.excludedContentTypes,
          `${kind}.excludedContentTypes`,
        ),
        includedContentTypes: stringArray(
          raw.includedContentTypes,
          `${kind}.includedContentTypes`,
        ),
        minResponseBodyBytes: boundedInteger(
          raw.minResponseBodyBytes,
          `${kind}.minResponseBodyBytes`,
          0,
          1_073_741_824,
        ),
        defaultEncoding: optionalString(
          raw.defaultEncoding,
          `${kind}.defaultEncoding`,
          32,
        ),
        encodings: stringArray(raw.encodings, `${kind}.encodings`),
      };
    case "buffering":
      return {
        maxRequestBodyBytes: boundedInteger(
          raw.maxRequestBodyBytes,
          `${kind}.maxRequestBodyBytes`,
          0,
          1_073_741_824,
        ),
        memRequestBodyBytes: boundedInteger(
          raw.memRequestBodyBytes,
          `${kind}.memRequestBodyBytes`,
          0,
          1_073_741_824,
        ),
        maxResponseBodyBytes: boundedInteger(
          raw.maxResponseBodyBytes,
          `${kind}.maxResponseBodyBytes`,
          0,
          1_073_741_824,
        ),
        memResponseBodyBytes: boundedInteger(
          raw.memResponseBodyBytes,
          `${kind}.memResponseBodyBytes`,
          0,
          1_073_741_824,
        ),
        retryExpression: optionalString(
          raw.retryExpression,
          `${kind}.retryExpression`,
        ),
      };
    case "retry":
      return {
        attempts: boundedInteger(
          raw.attempts,
          `${kind}.attempts`,
          1,
          100,
          true,
        )!,
        initialInterval: optionalString(
          raw.initialInterval,
          `${kind}.initialInterval`,
          64,
        ),
      };
    case "basicAuth": {
      if (!isObject(raw.secretBindingRef))
        throw new Error(`${kind}.secretBindingRef must be an object`);
      assertKnownKeys(
        raw.secretBindingRef,
        ["bindingId", "name", "key", "version"],
        `${kind}.secretBindingRef`,
      );
      const key = requiredString(
        raw.secretBindingRef.key,
        `${kind}.secretBindingRef.key`,
        5,
      );
      if (key !== "users")
        throw new Error(`${kind}.secretBindingRef.key must be users`);
      return {
        secretBindingRef: {
          bindingId: requiredString(
            raw.secretBindingRef.bindingId,
            `${kind}.secretBindingRef.bindingId`,
            36,
          ),
          name: requiredString(
            raw.secretBindingRef.name,
            `${kind}.secretBindingRef.name`,
            63,
          ),
          key: "users",
          version: boundedInteger(
            raw.secretBindingRef.version,
            `${kind}.secretBindingRef.version`,
            1,
            Number.MAX_SAFE_INTEGER,
            true,
          )!,
        },
        removeHeader: optionalBoolean(raw.removeHeader, `${kind}.removeHeader`),
        headerField: optionalString(
          raw.headerField,
          `${kind}.headerField`,
          128,
        ),
      };
    }
  }
}

function validateMiddleware(
  middleware: GuidedTraefikMiddleware,
  index: number,
) {
  const label = `Middleware ${index + 1}`;
  if (!dnsLabelPattern.test(middleware.name))
    throw new Error(
      `${label} name must be a DNS label of at most 63 characters`,
    );
  switch (middleware.kind) {
    case "redirectScheme": {
      const config = middleware.config as RedirectSchemeConfig;
      requiredString(config.scheme, `${label} redirect scheme`, 32);
      if (config.scheme !== "http" && config.scheme !== "https")
        throw new Error(`${label} redirect scheme must be http or https`);
      if (
        config.port !== undefined &&
        (!/^[0-9]{1,5}$/.test(config.port) ||
          Number(config.port) > 65535 ||
          Number(config.port) < 1)
      )
        throw new Error(`${label} redirect port must be from 1 to 65535`);
      optionalString(config.port, `${label} redirect port`, 5);
      optionalBoolean(config.permanent, `${label} permanent redirect`);
      break;
    }
    case "redirectRegex": {
      const config = middleware.config as RedirectRegexConfig;
      validateRE2(config.regex, `${label} redirect regex`);
      requiredString(config.replacement, `${label} replacement target`);
      optionalBoolean(config.permanent, `${label} permanent redirect`);
      break;
    }
    case "addPrefix": {
      const config = middleware.config as AddPrefixConfig;
      validatePath(config.prefix, `${label} prefix`);
      break;
    }
    case "stripPrefix": {
      const config = middleware.config as StripPrefixConfig;
      validateStringList(
        config.prefixes,
        `${label} prefixes`,
        validatePath,
        true,
      );
      optionalBoolean(config.forceSlash, `${label} force slash`);
      break;
    }
    case "stripPrefixRegex": {
      const config = middleware.config as StripPrefixRegexConfig;
      validateStringList(
        config.regex,
        `${label} regex list`,
        validateRE2,
        true,
      );
      break;
    }
    case "replacePath": {
      const config = middleware.config as ReplacePathConfig;
      validatePath(config.path, `${label} replacement path`);
      break;
    }
    case "replacePathRegex": {
      const config = middleware.config as ReplacePathRegexConfig;
      validateRE2(config.regex, `${label} path regex`);
      requiredString(config.replacement, `${label} replacement target`);
      break;
    }
    case "headers": {
      const config = middleware.config as HeadersConfig;
      validateHeaderEntryList(
        config.customRequestHeaders,
        `${label} request headers`,
      );
      validateHeaderEntryList(
        config.customResponseHeaders,
        `${label} response headers`,
      );
      optionalBoolean(
        config.accessControlAllowCredentials,
        `${label} CORS allow credentials`,
      );
      validateStringList(
        config.accessControlAllowHeaders,
        `${label} CORS allow headers`,
        (value, itemLabel) => {
          if (value !== "*" && !headerNamePattern.test(value))
            throw new Error(`${itemLabel} must be * or a header name`);
        },
      );
      validateStringList(
        config.accessControlAllowMethods,
        `${label} CORS methods`,
        (value, itemLabel) => {
          if (!/^(?:\*|[A-Z][A-Z0-9!#$%&'*+.^_`|~-]{0,31})$/.test(value))
            throw new Error(
              `${itemLabel} must be * or an uppercase HTTP method`,
            );
        },
      );
      validateStringList(
        config.accessControlAllowOriginList,
        `${label} CORS origins`,
        validateOrigin,
      );
      if (
        config.accessControlAllowCredentials === true &&
        config.accessControlAllowOriginList?.includes("*")
      )
        throw new Error(
          `${label} cannot allow credentials with the wildcard CORS origin`,
        );
      validateStringList(
        config.accessControlAllowOriginListRegex,
        `${label} CORS origin regex`,
        validateRE2,
      );
      validateStringList(
        config.accessControlExposeHeaders,
        `${label} CORS expose headers`,
        (value, itemLabel) => {
          if (value !== "*" && !headerNamePattern.test(value))
            throw new Error(`${itemLabel} must be * or a header name`);
        },
      );
      boundedInteger(
        config.accessControlMaxAge,
        `${label} CORS max age`,
        0,
        86_400,
      );
      optionalBoolean(config.addVaryHeader, `${label} add Vary header`);
      validateStringList(
        config.allowedHosts,
        `${label} allowed hosts`,
        validateHostname,
      );
      boundedInteger(config.stsSeconds, `${label} HSTS seconds`, 0, 63_072_000);
      optionalBoolean(config.stsIncludeSubdomains, `${label} HSTS subdomains`);
      optionalBoolean(config.stsPreload, `${label} HSTS preload`);
      optionalBoolean(config.forceSTSHeader, `${label} force HSTS header`);
      optionalBoolean(config.frameDeny, `${label} frame deny`);
      optionalBoolean(
        config.contentTypeNosniff,
        `${label} content type nosniff`,
      );
      optionalBoolean(config.browserXssFilter, `${label} browser XSS filter`);
      optionalBoolean(config.isDevelopment, `${label} development mode`);
      optionalString(
        config.customFrameOptionsValue,
        `${label} custom frame options`,
      );
      optionalString(
        config.customBrowserXSSValue,
        `${label} custom browser XSS value`,
      );
      optionalString(
        config.contentSecurityPolicy,
        `${label} content security policy`,
        8192,
      );
      optionalString(
        config.contentSecurityPolicyReportOnly,
        `${label} report-only content security policy`,
        8192,
      );
      optionalString(config.referrerPolicy, `${label} referrer policy`);
      optionalString(
        config.permissionsPolicy,
        `${label} permissions policy`,
        8192,
      );
      if (config.frameDeny === true && Boolean(config.customFrameOptionsValue))
        throw new Error(
          `${label} cannot combine frame deny and custom frame options`,
        );
      break;
    }
    case "rateLimit": {
      const config = middleware.config as RateLimitConfig;
      boundedInteger(config.average, `${label} average`, 0, 1_000_000, true);
      if (config.period !== undefined) {
        requiredString(config.period, `${label} period`, 64);
        validateDuration(config.period, `${label} period`);
      }
      boundedInteger(config.burst, `${label} burst`, 0, 1_000_000);
      break;
    }
    case "inFlightReq": {
      const config = middleware.config as InFlightReqConfig;
      boundedInteger(config.amount, `${label} amount`, 1, 1_000_000, true);
      break;
    }
    case "ipAllowList": {
      const config = middleware.config as IPAllowListConfig;
      validateStringList(
        config.sourceRange,
        `${label} trusted source ranges`,
        (value, itemLabel) => {
          if (!isCIDR(value))
            throw new Error(`${itemLabel} must be an explicit CIDR`);
        },
        true,
      );
      if (config.ipStrategy) {
        boundedInteger(config.ipStrategy.depth, `${label} IP depth`, 0, 100);
        boundedInteger(
          config.ipStrategy.ipv6Subnet,
          `${label} IPv6 subnet`,
          0,
          128,
        );
        validateStringList(
          config.ipStrategy.excludedIPs,
          `${label} excluded trusted IPs`,
          (value, itemLabel) => {
            if (!isIP(value) && !isCIDR(value))
              throw new Error(`${itemLabel} must be an IP address or CIDR`);
          },
        );
      }
      break;
    }
    case "compress": {
      const config = middleware.config as CompressConfig;
      boundedInteger(
        config.minResponseBodyBytes,
        `${label} minimum response body bytes`,
        0,
        1_073_741_824,
      );
      validateStringList(
        config.excludedContentTypes,
        `${label} excluded content types`,
        validateContentType,
      );
      validateStringList(
        config.includedContentTypes,
        `${label} included content types`,
        validateContentType,
      );
      if (config.defaultEncoding !== undefined) {
        requiredString(config.defaultEncoding, `${label} default encoding`, 32);
        validateEncoding(config.defaultEncoding, `${label} default encoding`);
      }
      validateStringList(
        config.encodings,
        `${label} encodings`,
        validateEncoding,
      );
      break;
    }
    case "buffering": {
      const config = middleware.config as BufferingConfig;
      for (const [name, value] of Object.entries(config).filter(
        ([name]) => name !== "retryExpression",
      )) {
        boundedInteger(value, `${label} ${name}`, 0, 1_073_741_824);
      }
      if (config.retryExpression !== undefined)
        requiredString(config.retryExpression, `${label} retry expression`);
      break;
    }
    case "retry": {
      const config = middleware.config as RetryConfig;
      boundedInteger(config.attempts, `${label} attempts`, 1, 100, true);
      if (config.initialInterval !== undefined) {
        requiredString(config.initialInterval, `${label} initial interval`, 64);
        validateDuration(config.initialInterval, `${label} initial interval`);
      }
      break;
    }
    case "basicAuth": {
      const config = middleware.config as BasicAuthConfig;
      const ref = config.secretBindingRef;
      if (
        !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
          ref.bindingId,
        )
      )
        throw new Error(`${label} BasicAuth binding ID must be a UUID`);
      if (!dnsLabelPattern.test(ref.name))
        throw new Error(`${label} BasicAuth binding name must be a DNS label`);
      if (ref.key !== "users")
        throw new Error(`${label} BasicAuth binding key must be users`);
      boundedInteger(
        ref.version,
        `${label} BasicAuth binding version`,
        1,
        Number.MAX_SAFE_INTEGER,
        true,
      );
      optionalBoolean(
        config.removeHeader,
        `${label} remove authorization header`,
      );
      if (
        config.headerField !== undefined &&
        !headerNamePattern.test(config.headerField)
      )
        throw new Error(
          `${label} BasicAuth header field must be a header name`,
        );
      break;
    }
  }
}

export function validateGuidedTraefikMiddlewares(
  definitions: GuidedTraefikMiddleware[],
  refs: string[],
): string | null {
  try {
    if (definitions.length > 32)
      throw new Error("At most 32 middleware definitions are allowed");
    const names = new Set<string>();
    definitions.forEach((middleware, index) => {
      validateMiddleware(middleware, index);
      if (names.has(middleware.name))
        throw new Error(`Middleware name ${middleware.name} is duplicated`);
      names.add(middleware.name);
    });
    if (refs.length > 16)
      throw new Error(
        "The route chain accepts at most 16 middleware references",
      );
    const seenRefs = new Set<string>();
    refs.forEach((ref) => {
      if (!dnsLabelPattern.test(ref))
        throw new Error(
          `Route middleware reference ${ref || "(empty)"} is invalid`,
        );
      if (seenRefs.has(ref))
        throw new Error(`Route middleware reference ${ref} is duplicated`);
      if (!names.has(ref))
        throw new Error(`Route middleware reference ${ref} does not resolve`);
      seenRefs.add(ref);
    });
    return null;
  } catch (error) {
    return error instanceof Error
      ? error.message
      : "The middleware configuration is invalid";
  }
}

export function guidedTraefikMiddlewareState(
  rawDefinitions: unknown,
  rawRefs: unknown,
): GuidedTraefikMiddlewareState {
  try {
    if (rawDefinitions !== undefined && !Array.isArray(rawDefinitions))
      throw new Error("spec.middlewares is not a list");
    if (rawRefs !== undefined && !Array.isArray(rawRefs))
      throw new Error("route.middlewareRefs is not a list");
    const definitions = (rawDefinitions ?? []).map((raw, index) => {
      if (!isObject(raw))
        throw new Error(`middleware ${index + 1} is not an object`);
      assertKnownKeys(
        raw,
        ["id", "name", "profileRef", "spec"],
        `middleware ${index + 1}`,
      );
      const name = requiredString(raw.name, `middleware ${index + 1}.name`, 63);
      const id = optionalString(raw.id, `middleware ${index + 1}.id`, 64);
      let profileRef: MiddlewareProfileRef | undefined;
      if (raw.profileRef !== undefined) {
        if (!isObject(raw.profileRef))
          throw new Error(`middleware ${name}.profileRef must be an object`);
        assertKnownKeys(
          raw.profileRef,
          ["profileId", "revision", "specDigest", "assignmentsDigest"],
          `middleware ${name}.profileRef`,
        );
        const profileId = requiredString(
          raw.profileRef.profileId,
          `middleware ${name}.profileRef.profileId`,
          36,
        );
        const specDigest = requiredString(
          raw.profileRef.specDigest,
          `middleware ${name}.profileRef.specDigest`,
          71,
        );
        const assignmentsDigest = requiredString(
          raw.profileRef.assignmentsDigest,
          `middleware ${name}.profileRef.assignmentsDigest`,
          71,
        );
        if (
          !/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
            profileId,
          ) ||
          !/^sha256:[0-9a-f]{64}$/.test(specDigest) ||
          !/^sha256:[0-9a-f]{64}$/.test(assignmentsDigest)
        )
          throw new Error(`middleware ${name}.profileRef is invalid`);
        profileRef = {
          profileId,
          revision: boundedInteger(
            raw.profileRef.revision,
            `middleware ${name}.profileRef.revision`,
            1,
            Number.MAX_SAFE_INTEGER,
            true,
          )!,
          specDigest,
          assignmentsDigest,
        };
      }
      if (!isObject(raw.spec))
        throw new Error(`middleware ${name}.spec is not an object`);
      const specKeys = Object.keys(raw.spec);
      if (specKeys.length !== 1 || !isMiddlewareKind(specKeys[0] ?? ""))
        throw new Error(
          `middleware ${name} uses a family that Guided does not represent`,
        );
      const kind = specKeys[0] as TraefikMiddlewareKind;
      const rawConfig = raw.spec[kind];
      if (!isObject(rawConfig))
        throw new Error(`middleware ${name}.${kind} is not an object`);
      const config = configForKind(kind, rawConfig);
      return {
        ...(id ? { id } : {}),
        ...(profileRef ? { profileRef } : {}),
        name,
        kind,
        config,
      } as GuidedTraefikMiddleware;
    });
    const refs = (rawRefs ?? []).map((ref, index) =>
      requiredString(ref, `route.middlewareRefs[${index}]`, 63),
    );
    const validation = validateGuidedTraefikMiddlewares(definitions, refs);
    if (validation) throw new Error(validation);
    return { definitions, refs, issue: "" };
  } catch (error) {
    const detail =
      error instanceof Error ? error.message : "unsupported fields";
    return {
      definitions: [],
      refs: [],
      issue: `Guided middleware editing cannot represent this configuration safely: ${detail}. The original YAML is preserved; use Advanced YAML to inspect or change it.`,
    };
  }
}

function compactObject<T extends Record<string, unknown>>(value: T): T {
  return Object.fromEntries(
    Object.entries(value).filter(([, item]) => item !== undefined),
  ) as T;
}

function headersToValue(config: HeadersConfig): Record<string, unknown> {
  const map = (entries: HeaderEntry[] | undefined) =>
    entries === undefined
      ? undefined
      : Object.fromEntries(entries.map(({ name, value }) => [name, value]));
  return compactObject({
    ...config,
    customRequestHeaders: map(config.customRequestHeaders),
    customResponseHeaders: map(config.customResponseHeaders),
  });
}

export function guidedTraefikMiddlewaresToValue(
  definitions: GuidedTraefikMiddleware[],
): Array<Record<string, unknown>> {
  return definitions.map((middleware) => ({
    ...(middleware.id ? { id: middleware.id } : {}),
    ...(middleware.profileRef ? { profileRef: middleware.profileRef } : {}),
    name: middleware.name,
    spec: {
      [middleware.kind]:
        middleware.kind === "headers"
          ? headersToValue(middleware.config)
          : compactObject(
              middleware.config as unknown as Record<string, unknown>,
            ),
    },
  }));
}

export function defaultGuidedTraefikMiddleware(
  kind: TraefikMiddlewareKind,
  name: string,
): GuidedTraefikMiddleware {
  const config: TraefikMiddlewareConfigByKind[TraefikMiddlewareKind] = (() => {
    switch (kind) {
      case "redirectScheme":
        return { scheme: "https", permanent: true };
      case "redirectRegex":
        return {
          regex: "^https?://example\\.com/(.*)",
          replacement: "https://www.example.com/${1}",
        };
      case "addPrefix":
        return { prefix: "/api" };
      case "stripPrefix":
        return { prefixes: ["/api"] };
      case "stripPrefixRegex":
        return { regex: ["^/api/v[0-9]+"] };
      case "replacePath":
        return { path: "/" };
      case "replacePathRegex":
        return { regex: "^/api/(.*)", replacement: "/${1}" };
      case "headers":
        return {};
      case "rateLimit":
        return { average: 100, period: "1s", burst: 200 };
      case "inFlightReq":
        return { amount: 100 };
      case "ipAllowList":
        return { sourceRange: ["10.0.0.0/8"] };
      case "compress":
        return {};
      case "buffering":
        return { maxRequestBodyBytes: 10_485_760 };
      case "retry":
        return { attempts: 3, initialInterval: "100ms" };
      case "basicAuth":
        return {
          secretBindingRef: {
            bindingId: "",
            name: "",
            key: "users",
            version: 0,
          },
          removeHeader: true,
        };
    }
  })();
  return { name, kind, config } as GuidedTraefikMiddleware;
}
