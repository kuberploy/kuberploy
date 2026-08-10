# Scoped observability provider

This package is the fail-closed provider boundary for Kuberploy's monitoring
API. Tenant-facing handlers authorize a platform-resolved service, namespace,
or global scope and select one named metric. They never pass PromQL, label
matchers, a provider URL, or provider credentials through this boundary.

The Prometheus client:

- expands only a closed catalog of Kuberploy recording-rule series;
- injects exact scope matchers and verifies every returned series still carries
  the expected scope labels;
- accepts only HTTPS endpoints outside hermetic tests, follows no redirects,
  and bounds time, body size, series, samples, and time range;
- returns only bounded Kuberploy identity labels and discards provider labels
  such as Pod names or arbitrary scrape metadata;
- erases the caller-owned bearer-token buffer after constructing the request;
- exposes generic typed failures without reflecting provider bodies, queries,
  URLs, or credentials.

This package is not authorization by itself. The HTTP layer must resolve the
opaque Kuberploy service/namespace identifier under the caller's current grant
revision before constructing `Scope`. Global scope is platform-admin only.

The seven metric keys expect protected recording rules emitted by the managed
monitoring release or proven compatible in existing-backend mode. Until those
rules and a healthy query client are both observed, monitoring capability must
remain false.

Managed mode additionally reads only three exact Kubernetes objects with
resource-name-scoped `get` permissions: the immutable release profile, the
Prometheus Operator Deployment, and the protected PrometheusRule. The profile,
release/chart identity, pinned operator image and argument digest, observed
Deployment generation, and rule-spec digest must match the compiled contract.
The Prometheus rules API must then report each of the seven recording rules
exactly once with `recording` type, `ok` health, and no evaluation error. A
successful `vector(1)` probe by itself never enables the capability.
