# Git-backed ordinary variables

`variables` is the strict compiler for project and environment
`variables.yaml` dependencies. A document has one closed shape:

```yaml
apiVersion: variables.kuberploy.io/v1alpha1
kind: VariableSet
values:
  LOG_LEVEL: info
  FEATURE_FLAG: "true"
```

Only ordinary string values belong here. YAML aliases, anchors, merge keys,
custom tags, duplicate keys, implicit booleans/numbers, unknown fields, invalid
environment names, and oversized documents fail closed. Values that resemble
another YAML scalar type must be quoted so Git review and runtime rendering
cannot disagree.

Resolution is deterministic and name-sorted. Project values are applied first,
environment values override them, and application `runtime.env` entries are
most specific. The effective result retains each overridden source for preview.
An application may replace an inherited ordinary value with an authorized
secret-binding reference, but a parent `VariableSet` never contains secret
material.

The server derives exactly two optional dependency paths in precedence order:
`tenants/<project-id>/variables.yaml`, then the environment binding's
`<prefix>/variables.yaml`. It records explicit presence or absence for both.
The Git projection indexer validates present documents, stores their parsed
form or bounded diagnostics, and includes exact dependency presence and blob
IDs in the application's strong ETag. A parent change therefore invalidates an
outstanding preview/save and is also part of pinned automatic-deployment
provenance.

Config read, preview, validation, and save resolve the fresh indexed bundle and
return both dependency state and the effective ordinary values or opaque
secret-binding references. Invalid parent content cannot replace the prior
active projection generation. Raw VariableSet documents remain reviewable Git
input; inherited values are not copied into `app.yaml`.

## Human Git management

The Variables page reads the exact project and environment snapshots for one
environment binding and displays their raw YAML, derived path, indexed revision
and ETag. Project scope is intentionally tied to that concrete environment Git
authority in the MVP. A caller supplies only the selected `project` or
`environment` scope and candidate YAML; the server derives the repository, ref,
path and publication policy.

Preview strictly validates the candidate and returns the exact Git diff with a
ten-minute opaque token bound to the actor, binding, scope/path, base revision,
ETag/parser version and candidate hash. Saving the same bytes consumes that
authority, requires an idempotency key, and creates a durable
`variable-set.git-write` Operation. Development environments use a normal
fast-forward direct commit. Protected environments use the shared protected
publication state machine and become indexed only after the exact pull request
merge is provider-verified. Stale bases, path/mode substitution, parser drift,
token replay with different bytes, and idempotency substitution fail closed;
lost responses replay the original Operation.

These management endpoints and the page are human-only. They store ordinary
strings and never accept secret material. Application secret-binding references
remain in `app.yaml` and use the separate strict Sealed Secrets workflow;
External Secrets remain disabled until a concrete audited remote writer exists.

Argo passes project, environment, and application documents as three ordered
Helm value files. Missing parents are allowed, while operator-owned expected
identity parameters plus the runtime chart's identity check make a missing or
wrong application document fail closed. The runtime chart deterministically
merges parent ordinary values with application `runtime.env`, materializes the
effective ordinary values in a content-addressed immutable ConfigMap, and lets
an application-level ordinary value or authorized secret-binding reference
override an inherited name.
