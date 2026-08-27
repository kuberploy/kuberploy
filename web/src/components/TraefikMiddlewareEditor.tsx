import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import {
  defaultGuidedTraefikMiddleware,
  guidedTraefikMiddlewareState,
  traefikMiddlewareKinds,
  validateGuidedTraefikMiddlewares,
  type BufferingConfig,
  type BasicAuthConfig,
  type CompressConfig,
  type GuidedTraefikMiddleware,
  type HeaderEntry,
  type HeadersConfig,
  type IPAllowListConfig,
  type RedirectRegexConfig,
  type RedirectSchemeConfig,
  type ReplacePathRegexConfig,
  type RetryConfig,
  type StripPrefixConfig,
  type TraefikMiddlewareKind,
} from "../lib/traefikMiddleware";
import {
  Button,
  Field,
  FieldLabel,
  FormGrid,
  Notice,
  PlaceholderBadge,
  useRowKeys,
} from "./ui";
import { Icon } from "./Icon";
import { BasicAuthBindingPicker } from "./BasicAuthBindingPicker";

const kindLabels: Record<TraefikMiddlewareKind, string> = {
  redirectScheme: "Redirect scheme",
  redirectRegex: "Redirect regex",
  addPrefix: "Add prefix",
  stripPrefix: "Strip prefix",
  stripPrefixRegex: "Strip prefix regex",
  replacePath: "Replace path",
  replacePathRegex: "Replace path regex",
  headers: "Headers & CORS",
  rateLimit: "Rate limit",
  inFlightReq: "In-flight requests",
  ipAllowList: "IP allow list",
  compress: "Compression",
  buffering: "Buffering",
  retry: "Retry",
  basicAuth: "Basic authentication",
};

const kindSlugs: Record<TraefikMiddlewareKind, string> = {
  redirectScheme: "redirect-scheme",
  redirectRegex: "redirect-regex",
  addPrefix: "add-prefix",
  stripPrefix: "strip-prefix",
  stripPrefixRegex: "strip-prefix-regex",
  replacePath: "replace-path",
  replacePathRegex: "replace-path-regex",
  headers: "headers",
  rateLimit: "rate-limit",
  inFlightReq: "in-flight",
  ipAllowList: "ip-allow-list",
  compress: "compress",
  buffering: "buffering",
  retry: "retry",
  basicAuth: "basic-auth",
};

function optionalNumber(value: string): number | undefined {
  return value === "" ? undefined : Number(value);
}

function numberInputValue(value: number | undefined): number | "" {
  return value !== undefined && Number.isFinite(value) ? value : "";
}

function BooleanSetting({
  label,
  value,
  onChange,
}: {
  label: string;
  value?: boolean;
  onChange: (value: boolean | undefined) => void;
}) {
  return (
    <Field label={label} hint="Default leaves the Traefik field unset.">
      <select
        aria-label={label}
        value={value === undefined ? "" : String(value)}
        onChange={(event) =>
          onChange(
            event.target.value === ""
              ? undefined
              : event.target.value === "true",
          )
        }
      >
        <option value="">Traefik default</option>
        <option value="true">Enabled</option>
        <option value="false">Disabled</option>
      </select>
    </Field>
  );
}

function StringListEditor({
  label,
  values = [],
  placeholder,
  risk,
  onChange,
}: {
  label: string;
  values?: string[];
  placeholder: string;
  risk?: string;
  onChange: (values: string[]) => void;
}) {
  // Row identity that survives a removal; see useRowKeys.
  const rowKeys = useRowKeys(values.length);
  return (
    <div className="grid content-start gap-2 [&>[data-slot='button']]:w-max to-760:[&>[data-slot='button']]:justify-self-start">
      <FieldLabel>{label}</FieldLabel>
      {risk ? (
        <span className="text-tone-warn text-xs leading-[1.45]">{risk}</span>
      ) : null}
      <div className="grid gap-1.5">
        {values.map((value, index) => (
          <div
            className="grid items-center gap-1.5 grid-cols-[minmax(0,_1fr)_32px]"
            key={rowKeys.keyAt(index)}
          >
            <input
              aria-label={`${label} ${index + 1}`}
              maxLength={2048}
              placeholder={placeholder}
              value={value}
              onChange={(event) =>
                onChange(
                  values.map((item, itemIndex) =>
                    itemIndex === index ? event.target.value : item,
                  ),
                )
              }
            />
            <button
              type="button"
              className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
              aria-label={`Remove ${label} ${index + 1}`}
              onClick={() => {
                rowKeys.removeAt(index);
                onChange(values.filter((_, itemIndex) => itemIndex !== index));
              }}
            >
              <Icon name="close" />
            </button>
          </div>
        ))}
      </div>
      <Button
        type="button"
        variant="secondary"
        disabled={values.length >= 64}
        onClick={() => onChange([...values, ""])}
      >
        <Icon name="plus" /> Add value
      </Button>
    </div>
  );
}

function HeaderMapEditor({
  label,
  values = [],
  onChange,
}: {
  label: string;
  values?: HeaderEntry[];
  onChange: (values: HeaderEntry[]) => void;
}) {
  // Row identity that survives a removal; see useRowKeys.
  const headerKeys = useRowKeys(values.length);
  return (
    <div className="grid content-start gap-2 [&>[data-slot='button']]:w-max to-760:[&>[data-slot='button']]:justify-self-start">
      <FieldLabel>{label}</FieldLabel>
      <span className="text-tone-warn text-xs leading-[1.45]">
        Header names and literal values are committed to Git. Credential,
        cookie, forwarding, routing, and hop-by-hop headers are blocked here;
        use a runtime secret inside the application instead.
      </span>
      <div className="grid gap-1.5">
        {values.map((entry, index) => (
          <div
            className="grid items-center gap-1.5 grid-cols-[minmax(130px,_0.65fr)_minmax(180px,_1fr)_32px] to-760:grid-cols-[1fr]"
            key={headerKeys.keyAt(index)}
          >
            <input
              aria-label={`${label} ${index + 1} name`}
              maxLength={128}
              placeholder="X-Content-Type-Options"
              value={entry.name}
              onChange={(event) =>
                onChange(
                  values.map((item, itemIndex) =>
                    itemIndex === index
                      ? { ...item, name: event.target.value }
                      : item,
                  ),
                )
              }
            />
            <input
              aria-label={`${label} ${index + 1} value`}
              maxLength={8192}
              placeholder="nosniff"
              value={entry.value}
              onChange={(event) =>
                onChange(
                  values.map((item, itemIndex) =>
                    itemIndex === index
                      ? { ...item, value: event.target.value }
                      : item,
                  ),
                )
              }
            />
            <button
              type="button"
              className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
              aria-label={`Remove ${label} ${index + 1}`}
              onClick={() => {
                headerKeys.removeAt(index);
                onChange(values.filter((_, itemIndex) => itemIndex !== index));
              }}
            >
              <Icon name="close" />
            </button>
          </div>
        ))}
      </div>
      <Button
        type="button"
        variant="secondary"
        disabled={values.length >= 64}
        onClick={() => onChange([...values, { name: "", value: "" }])}
      >
        <Icon name="plus" /> Add header
      </Button>
    </div>
  );
}

function HeadersEditor({
  value,
  onChange,
}: {
  value: HeadersConfig;
  onChange: (value: HeadersConfig) => void;
}) {
  const update = (change: Partial<HeadersConfig>) =>
    onChange({ ...value, ...change });
  return (
    <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3 grid gap-4">
      <HeaderMapEditor
        label="Literal request headers"
        values={value.customRequestHeaders}
        onChange={(customRequestHeaders) => update({ customRequestHeaders })}
      />
      <HeaderMapEditor
        label="Literal response headers"
        values={value.customResponseHeaders}
        onChange={(customResponseHeaders) => update({ customResponseHeaders })}
      />
      <div className="[&_.middleware-list-field]:p-0 grid gap-3 pt-4 border-t border-t-line [&_h5]:m-0 [&_h5]:text-ink [&_h5]:text-meta">
        <h5>CORS policy</h5>
        <FormGrid columns={3}>
          <BooleanSetting
            label="Allow credentials"
            value={value.accessControlAllowCredentials}
            onChange={(accessControlAllowCredentials) =>
              update({ accessControlAllowCredentials })
            }
          />
          <BooleanSetting
            label="Add Vary: Origin"
            value={value.addVaryHeader}
            onChange={(addVaryHeader) => update({ addVaryHeader })}
          />
          <Field label="CORS max age" hint="0–86400 seconds; blank is unset.">
            <input
              aria-label="CORS max age"
              type="number"
              min={0}
              max={86_400}
              value={numberInputValue(value.accessControlMaxAge)}
              onChange={(event) =>
                update({
                  accessControlMaxAge: optionalNumber(event.target.value),
                })
              }
            />
          </Field>
        </FormGrid>
        <div className="grid grid-cols-[repeat(2,_minmax(0,_1fr))] gap-3 to-760:grid-cols-[1fr]">
          <StringListEditor
            label="Allowed origins"
            values={value.accessControlAllowOriginList}
            placeholder="https://app.example.com"
            risk="Each origin is an explicit trust decision. Use * only for intentionally public responses."
            onChange={(accessControlAllowOriginList) =>
              update({ accessControlAllowOriginList })
            }
          />
          <StringListEditor
            label="Allowed origin RE2 patterns"
            values={value.accessControlAllowOriginListRegex}
            placeholder="^https://.*\\.example\\.com$"
            risk="Patterns expand the trusted browser origins; preview them carefully."
            onChange={(accessControlAllowOriginListRegex) =>
              update({ accessControlAllowOriginListRegex })
            }
          />
          <StringListEditor
            label="Allowed methods"
            values={value.accessControlAllowMethods}
            placeholder="GET"
            onChange={(accessControlAllowMethods) =>
              update({ accessControlAllowMethods })
            }
          />
          <StringListEditor
            label="Allowed request headers"
            values={value.accessControlAllowHeaders}
            placeholder="Content-Type"
            onChange={(accessControlAllowHeaders) =>
              update({ accessControlAllowHeaders })
            }
          />
          <StringListEditor
            label="Exposed response headers"
            values={value.accessControlExposeHeaders}
            placeholder="X-Request-Id"
            onChange={(accessControlExposeHeaders) =>
              update({ accessControlExposeHeaders })
            }
          />
        </div>
      </div>
      <div className="[&_.middleware-list-field]:p-0 grid gap-3 pt-4 border-t border-t-line [&_h5]:m-0 [&_h5]:text-ink [&_h5]:text-meta">
        <h5>Browser security headers</h5>
        <StringListEditor
          label="Allowed hosts"
          values={value.allowedHosts}
          placeholder="api.example.com"
          risk="Requests with any other Host header are rejected. Wildcards are not accepted in Guided mode."
          onChange={(allowedHosts) => update({ allowedHosts })}
        />
        <FormGrid columns={3}>
          <Field label="HSTS seconds" hint="0–63072000; blank is unset.">
            <input
              aria-label="HSTS seconds"
              type="number"
              min={0}
              max={63_072_000}
              value={numberInputValue(value.stsSeconds)}
              onChange={(event) =>
                update({ stsSeconds: optionalNumber(event.target.value) })
              }
            />
          </Field>
          <BooleanSetting
            label="HSTS subdomains"
            value={value.stsIncludeSubdomains}
            onChange={(stsIncludeSubdomains) =>
              update({ stsIncludeSubdomains })
            }
          />
          <BooleanSetting
            label="HSTS preload"
            value={value.stsPreload}
            onChange={(stsPreload) => update({ stsPreload })}
          />
          <BooleanSetting
            label="Force HSTS on HTTP"
            value={value.forceSTSHeader}
            onChange={(forceSTSHeader) => update({ forceSTSHeader })}
          />
          <BooleanSetting
            label="Deny framing"
            value={value.frameDeny}
            onChange={(frameDeny) => update({ frameDeny })}
          />
          <BooleanSetting
            label="Content type nosniff"
            value={value.contentTypeNosniff}
            onChange={(contentTypeNosniff) => update({ contentTypeNosniff })}
          />
          <BooleanSetting
            label="Legacy XSS filter"
            value={value.browserXssFilter}
            onChange={(browserXssFilter) => update({ browserXssFilter })}
          />
          <BooleanSetting
            label="Development mode"
            value={value.isDevelopment}
            onChange={(isDevelopment) => update({ isDevelopment })}
          />
        </FormGrid>
        <FormGrid>
          <Field
            label="Custom frame options"
            hint="Cannot be combined with Deny framing."
          >
            <input
              aria-label="Custom frame options"
              maxLength={2048}
              placeholder="SAMEORIGIN"
              value={value.customFrameOptionsValue ?? ""}
              onChange={(event) =>
                update({
                  customFrameOptionsValue: event.target.value || undefined,
                })
              }
            />
          </Field>
          <Field label="Custom legacy XSS value">
            <input
              aria-label="Custom legacy XSS value"
              maxLength={2048}
              placeholder="1; mode=block"
              value={value.customBrowserXSSValue ?? ""}
              onChange={(event) =>
                update({
                  customBrowserXSSValue: event.target.value || undefined,
                })
              }
            />
          </Field>
          <Field label="Content-Security-Policy">
            <input
              aria-label="Content-Security-Policy"
              maxLength={8192}
              placeholder="default-src 'self'"
              value={value.contentSecurityPolicy ?? ""}
              onChange={(event) =>
                update({
                  contentSecurityPolicy: event.target.value || undefined,
                })
              }
            />
          </Field>
          <Field label="CSP report-only">
            <input
              aria-label="CSP report-only"
              maxLength={8192}
              placeholder="default-src 'self'"
              value={value.contentSecurityPolicyReportOnly ?? ""}
              onChange={(event) =>
                update({
                  contentSecurityPolicyReportOnly:
                    event.target.value || undefined,
                })
              }
            />
          </Field>
          <Field label="Referrer policy">
            <input
              aria-label="Referrer policy"
              maxLength={2048}
              placeholder="strict-origin-when-cross-origin"
              value={value.referrerPolicy ?? ""}
              onChange={(event) =>
                update({ referrerPolicy: event.target.value || undefined })
              }
            />
          </Field>
          <Field label="Permissions policy">
            <input
              aria-label="Permissions policy"
              maxLength={8192}
              placeholder="camera=(), microphone=()"
              value={value.permissionsPolicy ?? ""}
              onChange={(event) =>
                update({ permissionsPolicy: event.target.value || undefined })
              }
            />
          </Field>
        </FormGrid>
      </div>
    </div>
  );
}

function MiddlewareFamilyEditor({
  middleware,
  applicationId,
  environmentId,
  onChange,
}: {
  middleware: GuidedTraefikMiddleware;
  applicationId?: string;
  environmentId?: string;
  onChange: (middleware: GuidedTraefikMiddleware) => void;
}) {
  const updateConfig = (config: GuidedTraefikMiddleware["config"]) =>
    onChange({ ...middleware, config } as GuidedTraefikMiddleware);
  switch (middleware.kind) {
    case "redirectScheme": {
      const value = middleware.config as RedirectSchemeConfig;
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3 to-580:grid-cols-[1fr]">
          <Field
            label="Redirect scheme"
            hint="Explicit redirect target scheme, usually https."
          >
            <select
              aria-label={`${middleware.name} redirect scheme`}
              value={value.scheme}
              onChange={(event) =>
                updateConfig({ ...value, scheme: event.target.value })
              }
            >
              <option value="https">HTTPS</option>
              <option value="http">HTTP</option>
            </select>
          </Field>
          <Field
            label="Redirect port"
            hint="Optional explicit target port, 1–65535."
          >
            <input
              aria-label={`${middleware.name} redirect port`}
              inputMode="numeric"
              maxLength={5}
              value={value.port ?? ""}
              onChange={(event) =>
                updateConfig({
                  ...value,
                  port: event.target.value || undefined,
                })
              }
            />
          </Field>
          <BooleanSetting
            label="Permanent redirect"
            value={value.permanent}
            onChange={(permanent) => updateConfig({ ...value, permanent })}
          />
        </div>
      );
    }
    case "redirectRegex": {
      const value = middleware.config as RedirectRegexConfig;
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3 to-580:grid-cols-[1fr]">
          <Field
            label="Source URL RE2"
            hint="Bounded RE2 pattern; no lookaround or backreferences."
          >
            <input
              aria-label={`${middleware.name} redirect regex`}
              maxLength={2048}
              value={value.regex}
              onChange={(event) =>
                updateConfig({ ...value, regex: event.target.value })
              }
            />
          </Field>
          <Field
            label="Replacement URL"
            hint="Explicit redirect destination. Use ${1} for a capture group."
          >
            <input
              aria-label={`${middleware.name} redirect replacement`}
              maxLength={2048}
              value={value.replacement}
              onChange={(event) =>
                updateConfig({ ...value, replacement: event.target.value })
              }
            />
          </Field>
          <BooleanSetting
            label="Permanent redirect"
            value={value.permanent}
            onChange={(permanent) => updateConfig({ ...value, permanent })}
          />
        </div>
      );
    }
    case "addPrefix":
      return (
        <Field label="Prefix" hint="Exact path prefix beginning with /.">
          <input
            aria-label={`${middleware.name} prefix`}
            maxLength={2048}
            value={middleware.config.prefix}
            onChange={(event) => updateConfig({ prefix: event.target.value })}
          />
        </Field>
      );
    case "stripPrefix": {
      const value = middleware.config as StripPrefixConfig;
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3">
          <StringListEditor
            label="Prefixes"
            values={value.prefixes}
            placeholder="/api"
            onChange={(prefixes) => updateConfig({ ...value, prefixes })}
          />
          <BooleanSetting
            label="Force slash"
            value={value.forceSlash}
            onChange={(forceSlash) => updateConfig({ ...value, forceSlash })}
          />
        </div>
      );
    }
    case "stripPrefixRegex":
      return (
        <StringListEditor
          label="Prefix RE2 patterns"
          values={middleware.config.regex}
          placeholder="^/api/v[0-9]+"
          onChange={(regex) => updateConfig({ regex })}
        />
      );
    case "replacePath":
      return (
        <Field
          label="Replacement path"
          hint="Exact target path beginning with /."
        >
          <input
            aria-label={`${middleware.name} replacement path`}
            maxLength={2048}
            value={middleware.config.path}
            onChange={(event) => updateConfig({ path: event.target.value })}
          />
        </Field>
      );
    case "replacePathRegex": {
      const value = middleware.config as ReplacePathRegexConfig;
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3 to-580:grid-cols-[1fr]">
          <Field label="Path RE2">
            <input
              aria-label={`${middleware.name} path regex`}
              maxLength={2048}
              value={value.regex}
              onChange={(event) =>
                updateConfig({ ...value, regex: event.target.value })
              }
            />
          </Field>
          <Field label="Replacement path" hint="Use ${1} for a capture group.">
            <input
              aria-label={`${middleware.name} path replacement`}
              maxLength={2048}
              value={value.replacement}
              onChange={(event) =>
                updateConfig({ ...value, replacement: event.target.value })
              }
            />
          </Field>
        </div>
      );
    }
    case "headers":
      return (
        <HeadersEditor
          value={middleware.config as HeadersConfig}
          onChange={updateConfig}
        />
      );
    case "rateLimit":
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3 to-580:grid-cols-[1fr]">
          <Field label="Average requests" hint="0–1000000 per period.">
            <input
              aria-label={`${middleware.name} average requests`}
              type="number"
              min={0}
              max={1_000_000}
              value={numberInputValue(middleware.config.average)}
              onChange={(event) =>
                updateConfig({
                  ...middleware.config,
                  average: Number(event.target.value),
                })
              }
            />
          </Field>
          <Field label="Period" hint="Go duration, for example 1s.">
            <input
              aria-label={`${middleware.name} rate period`}
              maxLength={64}
              value={middleware.config.period ?? ""}
              onChange={(event) =>
                updateConfig({
                  ...middleware.config,
                  period: event.target.value || undefined,
                })
              }
            />
          </Field>
          <Field label="Burst" hint="Optional 0–1000000.">
            <input
              aria-label={`${middleware.name} rate burst`}
              type="number"
              min={0}
              max={1_000_000}
              value={numberInputValue(middleware.config.burst)}
              onChange={(event) =>
                updateConfig({
                  ...middleware.config,
                  burst: optionalNumber(event.target.value),
                })
              }
            />
          </Field>
        </div>
      );
    case "inFlightReq":
      return (
        <Field label="Maximum in-flight requests" hint="1–1000000.">
          <input
            aria-label={`${middleware.name} in-flight amount`}
            type="number"
            min={1}
            max={1_000_000}
            value={numberInputValue(middleware.config.amount)}
            onChange={(event) =>
              updateConfig({ amount: Number(event.target.value) })
            }
          />
        </Field>
      );
    case "ipAllowList": {
      const value = middleware.config as IPAllowListConfig;
      const strategy = value.ipStrategy ?? {};
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3">
          <StringListEditor
            label="Trusted source CIDRs"
            values={value.sourceRange}
            placeholder="203.0.113.0/24"
            risk="Only these client networks can reach the route. Each entry must include an explicit prefix length."
            onChange={(sourceRange) => updateConfig({ ...value, sourceRange })}
          />
          <FormGrid columns={3}>
            <Field
              label="Forwarded IP depth"
              hint="0–100. Set only with a trusted proxy topology."
            >
              <input
                aria-label={`${middleware.name} forwarded IP depth`}
                type="number"
                min={0}
                max={100}
                value={numberInputValue(strategy.depth)}
                onChange={(event) =>
                  updateConfig({
                    ...value,
                    ipStrategy: {
                      ...strategy,
                      depth: optionalNumber(event.target.value),
                    },
                  })
                }
              />
            </Field>
            <Field label="IPv6 subnet" hint="0–128; blank is unset.">
              <input
                aria-label={`${middleware.name} IPv6 subnet`}
                type="number"
                min={0}
                max={128}
                value={numberInputValue(strategy.ipv6Subnet)}
                onChange={(event) =>
                  updateConfig({
                    ...value,
                    ipStrategy: {
                      ...strategy,
                      ipv6Subnet: optionalNumber(event.target.value),
                    },
                  })
                }
              />
            </Field>
          </FormGrid>
          <StringListEditor
            label="Excluded trusted proxy IPs"
            values={strategy.excludedIPs}
            placeholder="10.0.0.10"
            risk="These addresses are skipped while resolving the client IP. Configure only known proxies."
            onChange={(excludedIPs) =>
              updateConfig({
                ...value,
                ipStrategy: { ...strategy, excludedIPs },
              })
            }
          />
        </div>
      );
    }
    case "compress": {
      const value = middleware.config as CompressConfig;
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3">
          <FormGrid>
            <Field label="Minimum response bytes" hint="0–1073741824.">
              <input
                aria-label={`${middleware.name} minimum response bytes`}
                type="number"
                min={0}
                max={1_073_741_824}
                value={numberInputValue(value.minResponseBodyBytes)}
                onChange={(event) =>
                  updateConfig({
                    ...value,
                    minResponseBodyBytes: optionalNumber(event.target.value),
                  })
                }
              />
            </Field>
            <Field label="Default encoding" hint="For example gzip or br.">
              <input
                aria-label={`${middleware.name} default encoding`}
                maxLength={32}
                value={value.defaultEncoding ?? ""}
                onChange={(event) =>
                  updateConfig({
                    ...value,
                    defaultEncoding: event.target.value || undefined,
                  })
                }
              />
            </Field>
          </FormGrid>
          <div className="grid grid-cols-[repeat(2,_minmax(0,_1fr))] gap-3 to-760:grid-cols-[1fr]">
            <StringListEditor
              label="Included content types"
              values={value.includedContentTypes}
              placeholder="application/json"
              onChange={(includedContentTypes) =>
                updateConfig({ ...value, includedContentTypes })
              }
            />
            <StringListEditor
              label="Excluded content types"
              values={value.excludedContentTypes}
              placeholder="text/event-stream"
              onChange={(excludedContentTypes) =>
                updateConfig({ ...value, excludedContentTypes })
              }
            />
            <StringListEditor
              label="Allowed encodings"
              values={value.encodings}
              placeholder="gzip"
              onChange={(encodings) => updateConfig({ ...value, encodings })}
            />
          </div>
        </div>
      );
    }
    case "buffering": {
      const value = middleware.config as BufferingConfig;
      const byteField = (
        name: keyof Omit<BufferingConfig, "retryExpression">,
        label: string,
      ) => (
        <Field label={label} hint="0–1073741824 bytes; blank is unset.">
          <input
            aria-label={`${middleware.name} ${label}`}
            type="number"
            min={0}
            max={1_073_741_824}
            value={numberInputValue(value[name])}
            onChange={(event) =>
              updateConfig({
                ...value,
                [name]: optionalNumber(event.target.value),
              })
            }
          />
        </Field>
      );
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3">
          <FormGrid>
            {byteField("maxRequestBodyBytes", "Maximum request body")}
            {byteField("memRequestBodyBytes", "In-memory request body")}
            {byteField("maxResponseBodyBytes", "Maximum response body")}
            {byteField("memResponseBodyBytes", "In-memory response body")}
          </FormGrid>
          <Field
            label="Retry expression"
            hint="Explicit Traefik retry expression; no shell or template expansion."
          >
            <input
              aria-label={`${middleware.name} buffering retry expression`}
              maxLength={2048}
              value={value.retryExpression ?? ""}
              onChange={(event) =>
                updateConfig({
                  ...value,
                  retryExpression: event.target.value || undefined,
                })
              }
            />
          </Field>
        </div>
      );
    }
    case "retry": {
      const value = middleware.config as RetryConfig;
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3 to-580:grid-cols-[1fr]">
          <Field label="Attempts" hint="1–100.">
            <input
              aria-label={`${middleware.name} retry attempts`}
              type="number"
              min={1}
              max={100}
              value={numberInputValue(value.attempts)}
              onChange={(event) =>
                updateConfig({ ...value, attempts: Number(event.target.value) })
              }
            />
          </Field>
          <Field
            label="Initial interval"
            hint="Go duration, for example 100ms."
          >
            <input
              aria-label={`${middleware.name} initial retry interval`}
              maxLength={64}
              value={value.initialInterval ?? ""}
              onChange={(event) =>
                updateConfig({
                  ...value,
                  initialInterval: event.target.value || undefined,
                })
              }
            />
          </Field>
        </div>
      );
    }
    case "basicAuth": {
      const value = middleware.config as BasicAuthConfig;
      return (
        <div className="p-4 [&_.middleware-list-field]:p-0 [&>.field_+_.field]:mt-3 [&>.middleware-list-field_+_.field]:mt-3 [&>.middleware-list-field_+_.middleware-list-field]:mt-3">
          <Notice tone="warning">
            <div>
              <strong>Write-only runtime secret</strong>
              <p>
                Select an exact ready runtime-secret binding. User credentials
                and Kubernetes Secret names are never stored in AppConfig.
              </p>
            </div>
          </Notice>
          <BasicAuthBindingPicker
            applicationId={applicationId}
            environmentId={environmentId}
            value={value.secretBindingRef}
            onChange={(secretBindingRef) =>
              updateConfig({ ...value, secretBindingRef })
            }
          />
          <BooleanSetting
            label="Remove Authorization header"
            value={value.removeHeader}
            onChange={(removeHeader) =>
              updateConfig({ ...value, removeHeader })
            }
          />
          <Field label="Authenticated user header" hint="Optional header name.">
            <input
              aria-label={`${middleware.name} BasicAuth header field`}
              maxLength={128}
              value={value.headerField ?? ""}
              onChange={(event) =>
                updateConfig({
                  ...value,
                  headerField: event.target.value || undefined,
                })
              }
            />
          </Field>
        </div>
      );
    }
  }
}

function ReusableProfileAttacher({
  applicationId,
  environmentId,
  definitions,
  onAttach,
}: {
  applicationId: string;
  environmentId: string;
  definitions: GuidedTraefikMiddleware[];
  onAttach: (definition: GuidedTraefikMiddleware) => void;
}) {
  const [selectedProfile, setSelectedProfile] = useState("");
  const profiles = useQuery({
    queryKey: ["assigned-middleware-profiles", environmentId, applicationId],
    queryFn: () => api.assignedMiddlewareProfiles(environmentId, applicationId),
    retry: false,
  });
  return (
    <div className="grid grid-cols-[minmax(220px,_360px)_auto] items-end gap-3 [&>[data-slot='button']]:justify-self-start to-760:grid-cols-[1fr]">
      <Field
        label="Reusable middleware profile"
        hint="Only exact active revisions assigned to this application and environment are shown."
      >
        <select
          aria-label="Reusable middleware profile"
          value={selectedProfile}
          disabled={profiles.isPending || Boolean(profiles.error)}
          onChange={(event) => setSelectedProfile(event.target.value)}
        >
          <option value="">Select assigned profile</option>
          {profiles.data?.items.map((profile) => (
            <option
              key={`${profile.profileId}@${profile.revision}`}
              value={`${profile.profileId}@${profile.revision}`}
            >
              {profile.name} · revision {profile.revision}
            </option>
          ))}
        </select>
      </Field>
      <Button
        type="button"
        variant="secondary"
        disabled={!selectedProfile || definitions.length >= 32}
        onClick={() => {
          const profile = profiles.data?.items.find(
            (candidate) =>
              `${candidate.profileId}@${candidate.revision}` ===
              selectedProfile,
          );
          if (!profile) return;
          const profileKind = Object.keys(profile.spec)[0];
          if (
            !profileKind ||
            !traefikMiddlewareKinds.includes(
              profileKind as TraefikMiddlewareKind,
            )
          )
            return;
          const base = kindSlugs[profileKind as TraefikMiddlewareKind];
          const used = new Set(definitions.map(({ name }) => name));
          let name = base;
          for (let suffix = 2; used.has(name) && suffix <= 99; suffix += 1)
            name = `${base}-${suffix}`;
          const parsed = guidedTraefikMiddlewareState(
            [
              {
                name,
                profileRef: {
                  profileId: profile.profileId,
                  revision: profile.revision,
                  specDigest: profile.specDigest,
                  assignmentsDigest: profile.assignmentsDigest,
                },
                spec: profile.spec,
              },
            ],
            [],
          );
          const definition = parsed.definitions[0];
          if (!definition) return;
          onAttach(definition);
          setSelectedProfile("");
        }}
      >
        <Icon name="plus" /> Attach exact revision
      </Button>
    </div>
  );
}

export function TraefikMiddlewareEditor({
  definitions,
  refs,
  issue,
  routeEnabled,
  readOnly = false,
  editingUnavailableReason,
  applicationId,
  environmentId,
  reusableProfilesEnabled = false,
  onChange,
}: {
  definitions: GuidedTraefikMiddleware[];
  refs: string[];
  issue: string;
  routeEnabled: boolean;
  readOnly?: boolean;
  editingUnavailableReason?: string;
  applicationId?: string;
  environmentId?: string;
  reusableProfilesEnabled?: boolean;
  onChange: (value: {
    definitions: GuidedTraefikMiddleware[];
    refs: string[];
  }) => void;
}) {
  const [newKind, setNewKind] = useState<TraefikMiddlewareKind>("headers");
  // A definition has no id until it is saved and a chain entry is a bare,
  // user-editable name, so neither can key its row. Identity is minted per row
  // and moved with the row; see useRowKeys.
  const definitionKeys = useRowKeys(definitions.length);
  const chainKeys = useRowKeys(refs.length);

  // Chain rows are keyed by position because a chain entry is a bare name with
  // no stable id, so a reorder rewrites the row the operator is standing on
  // instead of moving it. Keyboard focus would stay on that position and end up
  // driving whichever middleware landed there -- press Enter twice and the
  // second press moves a different one. Put focus back on the control for the
  // middleware that actually moved.
  const chainControls = useRef(new Map<string, HTMLButtonElement>());
  const pendingChainFocus = useRef<string | null>(null);
  useEffect(() => {
    const wanted = pendingChainFocus.current;
    if (!wanted) return;
    pendingChainFocus.current = null;
    const target = chainControls.current.get(wanted);
    if (!target) return;
    // At the end of the chain the button that made the move is disabled in its
    // new position; hand focus to the opposite direction rather than to nothing.
    if (target.disabled) {
      const [direction, ref] = wanted.split(":");
      const opposite = chainControls.current.get(
        `${direction === "up" ? "down" : "up"}:${ref}`,
      );
      if (opposite && !opposite.disabled) opposite.focus();
      return;
    }
    target.focus();
  });
  const registerChainControl =
    (key: string) => (node: HTMLButtonElement | null) => {
      if (node) chainControls.current.set(key, node);
      else chainControls.current.delete(key);
    };

  const validationError = useMemo(
    () => (issue ? null : validateGuidedTraefikMiddlewares(definitions, refs)),
    [definitions, issue, refs],
  );
  const unavailable = issue || editingUnavailableReason;
  const controlsDisabled = readOnly || Boolean(unavailable);

  const nextName = (kind: TraefikMiddlewareKind) => {
    const base = kindSlugs[kind];
    const used = new Set(definitions.map(({ name }) => name));
    if (!used.has(base)) return base;
    for (let suffix = 2; suffix <= 99; suffix += 1) {
      const candidate = `${base}-${suffix}`;
      if (!used.has(candidate)) return candidate;
    }
    return `${base}-new`;
  };

  const updateDefinition = (
    index: number,
    middleware: GuidedTraefikMiddleware,
  ) => {
    const previous = definitions[index];
    onChange({
      definitions: definitions.map((item, itemIndex) =>
        itemIndex === index ? middleware : item,
      ),
      refs:
        previous && previous.name !== middleware.name
          ? refs.map((ref) => (ref === previous.name ? middleware.name : ref))
          : refs,
    });
  };

  const move = <T,>(values: T[], from: number, to: number): T[] => {
    if (to < 0 || to >= values.length) return values;
    const result = [...values];
    const [item] = result.splice(from, 1);
    if (item !== undefined) result.splice(to, 0, item);
    return result;
  };

  return (
    <section className="py-6 px-0 border-b border-b-line last:border-b-0 [&_h3]:m-0 [&_h3]:text-ink [&_h3]:text-sm [&_h3]:font-semibold [&_h3]:tracking-[-0.01em] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-faint [&_p]:text-xs [&>[data-slot='notice']]:mb-4">
      <div className="grid items-center gap-3 mb-4 grid-cols-[32px_minmax(0,_1fr)_auto] to-580:grid-cols-[32px_1fr] to-580:[&_[data-slot='button']]:col-[2] to-580:[&_[data-slot='button']]:w-max">
        <span className="grid w-8 h-8 place-items-center rounded-[9px] text-mint-dark bg-mint-soft [&_svg]:w-[15px]">
          <Icon name="route" />
        </span>
        <div>
          <h3>Traefik middleware</h3>
          <p>
            Application-scoped, ordered definitions and an exact route chain. No
            cross-namespace, secret, ForwardAuth, plugin, or chain references
            are accepted in Guided mode.
          </p>
        </div>
        {readOnly ? <PlaceholderBadge>Read-only</PlaceholderBadge> : null}
      </div>

      {unavailable ? (
        <Notice tone="warning" role="status">
          <div>
            <strong>Guided middleware editing is unavailable</strong>
            <p>{unavailable}</p>
          </div>
          <PlaceholderBadge>YAML preserved</PlaceholderBadge>
        </Notice>
      ) : null}
      {validationError ? (
        <Notice tone="error" role="alert">
          <div>
            <strong>Middleware draft needs attention</strong>
            <p>{validationError}</p>
          </div>
        </Notice>
      ) : null}

      <fieldset
        className="min-w-0 m-0 p-0 border-0 disabled:opacity-100 [&:disabled_input]:text-ink-soft [&:disabled_input]:cursor-not-allowed [&:disabled_input]:bg-surface-soft [&:disabled_select]:text-ink-soft [&:disabled_select]:cursor-not-allowed [&:disabled_select]:bg-surface-soft [&:disabled_textarea]:text-ink-soft [&:disabled_textarea]:cursor-not-allowed [&:disabled_textarea]:bg-surface-soft"
        disabled={controlsDisabled}
      >
        <legend className="sr-only">Traefik middleware controls</legend>
        <div className="grid grid-cols-[minmax(220px,_360px)_auto] items-end gap-3 [&>[data-slot='button']]:justify-self-start to-760:grid-cols-[1fr]">
          <Field label="Middleware family">
            <select
              aria-label="New middleware family"
              value={newKind}
              onChange={(event) =>
                setNewKind(event.target.value as TraefikMiddlewareKind)
              }
            >
              {traefikMiddlewareKinds.map((kind) => (
                <option value={kind} key={kind}>
                  {kindLabels[kind]}
                </option>
              ))}
            </select>
          </Field>
          <Button
            type="button"
            variant="secondary"
            disabled={definitions.length >= 32}
            onClick={() =>
              onChange({
                definitions: [
                  ...definitions,
                  defaultGuidedTraefikMiddleware(newKind, nextName(newKind)),
                ],
                refs,
              })
            }
          >
            <Icon name="plus" /> Add middleware
          </Button>
        </div>
        {reusableProfilesEnabled && applicationId && environmentId ? (
          <ReusableProfileAttacher
            applicationId={applicationId}
            environmentId={environmentId}
            definitions={definitions}
            onAttach={(definition) =>
              onChange({
                definitions: [...definitions, definition],
                refs:
                  routeEnabled && refs.length < 16
                    ? [...refs, definition.name]
                    : refs,
              })
            }
          />
        ) : null}

        {definitions.length ? (
          <div className="grid gap-3 mt-4">
            {definitions.map((middleware, index) => (
              <article
                className="overflow-hidden border border-line rounded-[10px] bg-surface-soft [&>.field]:p-4 [&>.middleware-list-field]:p-4"
                key={definitionKeys.keyAt(index)}
              >
                <header className="grid grid-cols-[28px_minmax(160px,_0.8fr)_minmax(180px,_0.7fr)_auto] items-end gap-3 p-3 border-b border-b-line bg-surface to-760:grid-cols-[1fr]">
                  <span className="grid w-[26px] h-[26px] place-items-center self-center border border-line rounded-[7px] text-ink-soft bg-surface-soft text-meta font-semibold to-760:hidden">
                    {index + 1}
                  </span>
                  <Field label="DNS-label name">
                    <input
                      aria-label={`Middleware ${index + 1} name`}
                      maxLength={63}
                      value={middleware.name}
                      onChange={(event) =>
                        updateDefinition(index, {
                          ...middleware,
                          name: event.target.value,
                        } as GuidedTraefikMiddleware)
                      }
                    />
                  </Field>
                  <Field label="Family">
                    <select
                      aria-label={`Middleware ${index + 1} family`}
                      value={middleware.kind}
                      disabled={Boolean(middleware.profileRef)}
                      onChange={(event) => {
                        const kind = event.target
                          .value as TraefikMiddlewareKind;
                        const replacement = defaultGuidedTraefikMiddleware(
                          kind,
                          middleware.name,
                        );
                        updateDefinition(index, {
                          ...replacement,
                          ...(middleware.id ? { id: middleware.id } : {}),
                        } as GuidedTraefikMiddleware);
                      }}
                    >
                      {traefikMiddlewareKinds.map((kind) => (
                        <option value={kind} key={kind}>
                          {kindLabels[kind]}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <div className="flex items-center gap-1.5 pb-1 to-760:justify-self-start">
                    <button
                      type="button"
                      className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
                      disabled={index === 0}
                      aria-label={`Move middleware ${middleware.name} up`}
                      onClick={() =>
                        onChange({
                          definitions: move(definitions, index, index - 1),
                          refs,
                        })
                      }
                    >
                      ↑
                    </button>
                    <button
                      type="button"
                      className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
                      disabled={index === definitions.length - 1}
                      aria-label={`Move middleware ${middleware.name} down`}
                      onClick={() =>
                        onChange({
                          definitions: move(definitions, index, index + 1),
                          refs,
                        })
                      }
                    >
                      ↓
                    </button>
                    <button
                      type="button"
                      className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
                      aria-label={`Remove middleware ${middleware.name}`}
                      onClick={() => {
                        definitionKeys.removeAt(index);
                        for (
                          let position = refs.length - 1;
                          position >= 0;
                          position -= 1
                        ) {
                          if (refs[position] === middleware.name)
                            chainKeys.removeAt(position);
                        }
                        onChange({
                          definitions: definitions.filter(
                            (_, itemIndex) => itemIndex !== index,
                          ),
                          refs: refs.filter((ref) => ref !== middleware.name),
                        });
                      }}
                    >
                      <Icon name="close" />
                    </button>
                  </div>
                </header>
                {middleware.profileRef ? (
                  <Notice tone="info">
                    <div>
                      <strong>Reusable exact revision</strong>
                      <p>
                        Revision {middleware.profileRef.revision} is immutable.
                        Detach it to make an independent inline copy before
                        editing.
                      </p>
                    </div>
                    <Button
                      type="button"
                      variant="secondary"
                      onClick={() => {
                        const { profileRef: _profileRef, ...inline } =
                          middleware;
                        updateDefinition(
                          index,
                          inline as GuidedTraefikMiddleware,
                        );
                      }}
                    >
                      Detach as inline copy
                    </Button>
                  </Notice>
                ) : (
                  <MiddlewareFamilyEditor
                    middleware={middleware}
                    applicationId={applicationId}
                    environmentId={environmentId}
                    onChange={(value) => updateDefinition(index, value)}
                  />
                )}
              </article>
            ))}
          </div>
        ) : (
          <div className="p-4 border border-dashed border-[var(--line)] rounded-lg text-ink-faint bg-surface-soft text-meta text-center">
            No middleware definitions. Add a bounded Traefik family when the
            application needs route-specific behavior.
          </div>
        )}

        <div className="[&_h4]:m-0 [&_h4]:text-ink [&_h4]:text-meta grid gap-3 mt-5 p-4 border border-line rounded-[10px] bg-surface">
          <div className="flex items-center justify-between gap-3 [&_p]:mt-1 to-760:items-start to-760:flex-col">
            <div>
              <h4>Route chain</h4>
              <p>
                Execution order is top to bottom. Every reference must resolve
                to exactly one definition above.
              </p>
            </div>
            <Button
              type="button"
              variant="secondary"
              disabled={
                !routeEnabled ||
                refs.length >= 16 ||
                definitions.every(({ name }) => refs.includes(name))
              }
              onClick={() => {
                const next = definitions.find(
                  ({ name }) => !refs.includes(name),
                );
                if (next) onChange({ definitions, refs: [...refs, next.name] });
              }}
            >
              <Icon name="plus" /> Add to route
            </Button>
          </div>
          {!routeEnabled ? (
            <div className="p-4 border border-dashed border-[var(--line)] rounded-lg text-ink-faint bg-surface-soft text-meta text-center">
              Add a public hostname before assigning a route chain. Definitions
              remain application-scoped and are not exposed by themselves.
            </div>
          ) : refs.length ? (
            <div className="grid gap-1.5">
              {refs.map((ref, index) => (
                <div
                  className="[&>span]:grid [&>span]:w-[26px] [&>span]:h-[26px] [&>span]:place-items-center [&>span]:self-center [&>span]:border [&>span]:border-line [&>span]:rounded-[7px] [&>span]:text-ink-soft [&>span]:bg-surface-soft [&>span]:text-meta [&>span]:font-semibold grid items-center gap-1.5 grid-cols-[28px_minmax(180px,_1fr)_32px_32px_32px] to-760:grid-cols-[28px_minmax(0,_1fr)_32px_32px_32px]"
                  key={chainKeys.keyAt(index)}
                >
                  <span>{index + 1}</span>
                  <select
                    aria-label={`Route middleware ${index + 1}`}
                    value={ref}
                    onChange={(event) =>
                      onChange({
                        definitions,
                        refs: refs.map((item, itemIndex) =>
                          itemIndex === index ? event.target.value : item,
                        ),
                      })
                    }
                  >
                    {!definitions.some(({ name }) => name === ref) ? (
                      <option value={ref}>{ref} (unresolved)</option>
                    ) : null}
                    {/* A name is user-editable and is not unique until the
                        validator says so, so it cannot be the key. */}
                    {definitions.map(({ name }, definitionIndex) => (
                      <option value={name} key={`${name}-${definitionIndex}`}>
                        {name}
                      </option>
                    ))}
                  </select>
                  <button
                    type="button"
                    className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
                    ref={registerChainControl(`up:${ref}`)}
                    disabled={index === 0}
                    aria-label={`Move route middleware ${ref} up`}
                    onClick={() => {
                      pendingChainFocus.current = `up:${ref}`;
                      chainKeys.moveRow(index, index - 1);
                      onChange({
                        definitions,
                        refs: move(refs, index, index - 1),
                      });
                    }}
                  >
                    ↑
                  </button>
                  <button
                    type="button"
                    className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
                    ref={registerChainControl(`down:${ref}`)}
                    disabled={index === refs.length - 1}
                    aria-label={`Move route middleware ${ref} down`}
                    onClick={() => {
                      pendingChainFocus.current = `down:${ref}`;
                      chainKeys.moveRow(index, index + 1);
                      onChange({
                        definitions,
                        refs: move(refs, index, index + 1),
                      });
                    }}
                  >
                    ↓
                  </button>
                  <button
                    type="button"
                    className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
                    aria-label={`Remove route middleware ${ref}`}
                    onClick={() => {
                      chainKeys.removeAt(index);
                      onChange({
                        definitions,
                        refs: refs.filter(
                          (_, itemIndex) => itemIndex !== index,
                        ),
                      });
                    }}
                  >
                    <Icon name="close" />
                  </button>
                </div>
              ))}
            </div>
          ) : (
            <div className="p-4 border border-dashed border-[var(--line)] rounded-lg text-ink-faint bg-surface-soft text-meta text-center">
              This public route has no middleware chain.
            </div>
          )}
        </div>
      </fieldset>
    </section>
  );
}
