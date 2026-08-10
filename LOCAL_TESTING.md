# Development build and Kubernetes integration testing

Status: development test contract, updated 2026-08-09.

Kuberploy separates local container builds from Kubernetes integration. A
local Docker-compatible engine may be used for Buildx and registry-cache
testing, but it is not the project integration cluster.
Kubernetes behavior is exercised against an operator-supplied, non-production
cluster that conforms to the release's supported Kubernetes and capability
contract.

No real cluster address, hostname, credential path, or context name belongs in
the repository. Operators select a cluster explicitly at invocation time.

## Test lanes

| Lane | Runtime | Purpose |
|---|---|---|
| Unit and contract | Local processes | API/schema, authorization, rendering, queue, Git, retention, and UI behavior |
| Local container build/cache | Operator-selected Docker-compatible engine | Dockerfiles, native development images, Buildx behavior, and registry cache import/export |
| Kubernetes integration | Operator-supplied conforming cluster | Helm, Argo CD, workloads, Traefik, policy, logs, metrics, registry pull, upgrade, and rollback |
| Provider-facing opt-in | Explicitly approved test accounts/endpoints | GitHub delivery, public DNS, ACME, and registry-provider behavior |

Passing one lane does not imply that another passed. In particular, an image
present in a local engine is not evidence that a Kubernetes node can pull
it from a registry.

## Local Docker and build-cache lane

Commands must select an operator-approved Docker context explicitly instead of
changing or trusting ambient state.

Local source builds may use a run-scoped builder and an ephemeral or dedicated
development registry. Cache tests exercise the same registry cache contract as
the in-cluster builder:

- `cache-from=type=registry,ref=<service-and-platform-scoped-ref>`;
- `cache-to=type=registry,ref=<service-and-platform-scoped-ref>,mode=max`;
- OCI media types and image-manifest mode enabled;
- no cache sharing across tenant or trust boundaries;
- a cache import/export failure degrades to a cold-build warning, while failure
  to push the final image is terminal.

Any image used by the Kubernetes integration lane must be pushed and deployed
by immutable digest through a registry reachable by that cluster. Direct local
engine image sharing is never part of the integration proof.

## Explicit Kubernetes integration target

The test operator supplies these inputs; the repository provides no default
cluster identity:

| Input | Contract |
|---|---|
| `KUBECONFIG` | One absolute path to a credential file outside tracked source |
| `KUBERPLOY_TEST_CONTEXT` | Exact context name in that kubeconfig |
| `KUBERPLOY_TEST_SERVER` | Exact expected HTTPS API server URL for identity validation |
| `KUBERPLOY_E2E_RUN_ID` | Lowercase run identifier used in names and ownership labels |

Every Kubernetes command must carry both selectors explicitly:

```bash
kubectl --kubeconfig "${KUBECONFIG:?set an absolute kubeconfig path}" \
  --context "${KUBERPLOY_TEST_CONTEXT:?set the exact test context}" \
  version
```

Test tooling must not call `kubectl config use-context`, merge kubeconfig files,
or infer a target from the ambient current context. It aborts when either input
is missing, when `KUBECONFIG` is not one absolute regular-file path, or when the
named context is absent. It may report the selected server URL during an
operator-invoked preflight, but must never render raw kubeconfig, tokens, client
keys, or certificates.

Before mutation, a read-only preflight verifies:

- Kubernetes is within the locked 1.34-1.36 support window;
- required stable APIs and admission capabilities are present;
- all selected nodes report their OS, architecture, runtime, and Ready state;
- a usable default StorageClass exists, or the operator selected a compatible
  class explicitly;
- existing IngressClasses, K3s-packaged components, Argo CD, cert-manager,
  Prometheus Operator, and other adopted dependencies are detected rather than
  overwritten;
- required registry endpoints are reachable from the cluster, not merely from
  the development machine;
- network-policy enforcement and LoadBalancer behavior are capability-probed
  before a test claims them.

The operator is responsible for selecting a disposable or dedicated test
cluster and confirming that the requested test scope is safe there. Production
clusters are not supported test targets. With all four variables set, the
implemented harness is:

```bash
make kubernetes-preflight
make kubernetes-smoke
make kubernetes-cleanup
```

`kubernetes-preflight` is read-only. `kubernetes-smoke` creates one new
run-scoped namespace containing digest-pinned Pods, a ClusterIP Service, PVC,
and DNS/service probe. It then proves that the cluster actually enforces a
default-deny ingress NetworkPolicy and an exact pod-label/port allow rule; API
discovery alone is not accepted as policy enforcement. The command
automatically deletes that namespace after rechecking its exact ownership
labels. `kubernetes-cleanup` is idempotent and refuses to delete a namespace
whose run and managed-by labels do not both match.

The code-side full-MVP runner is
`scripts/kubernetes/test/e2e/qualification.sh`. It fixes the execution order,
assertion IDs, target wrappers, run-scoped ownership inventory, reverse cleanup,
and final report gate for the complete matrix below. Repository-owned code
performs the Helm, Kubernetes, API, HTTP, DNS, and TLS interactions. The
operator supplies only a strict declarative scenario containing exact external
identities, endpoints, expected values, and closed workflow fields; executable
stage drivers are not accepted. The repository initiates the direct/protected
deployment and rollback, source-build promotion, registry cleanup and platform
upgrade mutations with session Cookie/CSRF boundaries and polls their exact
terminal projections. Missing evidence or an assertion/probe substitution
fails before mutation. Successful qualification is explicitly reported as
`qualified-teardown-required`: the acknowledged disposable cluster must be
destroyed because not every retained product/Argo object has a deletion API.
Only `finalize-teardown.sh` can transition that report to `passed`, after it
verifies an exact destruction receipt signed by the scenario-pinned
infrastructure authority key.
Its hermetic fake-tool tests run as part of
`make kubernetes-harness-test`. See `scripts/kubernetes/test/e2e/README.md` for
the scenario contract.

## GitHub development target

| Setting | Required value |
|---|---|
| Organization | [`kuberploy`](https://github.com/kuberploy) |
| GitHub CLI host | `github.com` |
| GitHub CLI account | Exact operator-selected `KUBERPLOY_GITHUB_CLI_USER` |
| Verified membership | Active organization administrator, checked immediately before use |

Before any GitHub-backed test or repository operation, tooling runs an identity
preflight equivalent to:

```bash
: "${KUBERPLOY_GITHUB_CLI_USER:?set the intended GitHub CLI account}"
gh auth switch --hostname github.com --user "${KUBERPLOY_GITHUB_CLI_USER}"
gh api user --jq .login
gh api user/memberships/orgs/kuberploy --jq '[.state, .role]'
```

The operation aborts unless the observed login exactly matches
`KUBERPLOY_GITHUB_CLI_USER`, membership state is `active`, and the requested
action is valid for the current task. Commands name the organization and
repository explicitly rather than inferring an owner. Automation must not
print, export, or persist the GitHub CLI token, and it must not copy that token
into Kubernetes.

GitHub App installation tokens used by Kuberploy are separate, short-lived
credentials minted through the App flow. Selecting the intended CLI profile
does not authorize repository creation, GitHub App registration, organization
settings changes, or webhook installation unless the implementation task
explicitly includes that external write.

## Safety rules for Kubernetes tests

1. Every command passes the explicit kubeconfig and context; no mutation uses
   ambient kubectl state.
2. Namespaced resources use `kuberploy-e2e-<run-id>` and the label
   `kuberploy.io/test-run=<run-id>`. Cleanup targets that exact ownership tuple,
   never a wildcard or all namespaces.
3. Cluster-scoped resources carry the run ID where naming permits it. A test
   records shared CRDs and other pre-existing resources before mutation and
   never deletes an object it did not create.
4. Credentials are synthetic by default. Opt-in provider credentials use
   short-lived Kubernetes Secrets and are never committed or logged.
5. Tests do not change firewalls, public DNS, certificate issuers, GitHub App
   callbacks/webhooks, public tunnels, or provider settings without explicit
   approval for that external effect.
6. Development image tags are unique and never `latest`; deployment resolves
   them to immutable digests before GitOps commit.
7. Failed tests leave an inventory and exact cleanup command. They do not hide
   partial Helm releases, finalizers, PVCs, or cluster-scoped objects.

## Routing, GitHub, DNS, and certificate tests

The Kubernetes harness assumes no particular hostname, LoadBalancer provider,
or public IP. HTTP-only tests may use a run-scoped port-forward or another
operator-selected private route. Managed Traefik tests detect the cluster's
exposure capabilities before choosing `LoadBalancer`, `NodePort`, or an
explicitly approved alternative.

Public ingress is not required for the default GitHub suite:

| Test mode | What it proves | Public endpoint |
|---|---|---:|
| Signed webhook replay | Exact-body HMAC verification, GitHub headers, delivery deduplication, ordering, enqueue, and build generation | No |
| In-cluster GitHub API emulator | Callback state, installation-token exchange, permissions, pagination, throttling, errors, and redaction | No |
| Test Git/OCI services | Clone/fetch, commit/push CAS, Argo reconciliation, image push/pull, and digest deployment | No |
| Real GitHub outbound test | Allowed repository discovery, clone/fetch, and missed-webhook polling | No inbound endpoint; opt-in credentials |
| Live GitHub webhook test | Actual delivery, retries, and end-to-end App configuration | Yes; separately approved endpoint |

Certificate and DNS modes are also separated:

- `httpOnly`: complete on a private test route;
- `customCertificate`: disposable test CA/certificate through the normal
  write-only secret path;
- `letsEncrypt`: in-cluster ACME test server by default, with public Let's
  Encrypt staging as an opt-in provider test;
- `externalDns`: fake/webhook provider by default, with a delegated real test
  zone only when explicitly configured.

No test sends a private, reserved, or local-only name to a public ACME or DNS
provider.

## Integration coverage and order

An item is not a shipped claim until its running capability API and test both
enable it. The target integration matrix covers:

- locked Helm installation and ordered upgrades;
- Argo CD Applications, AppProjects, ApplicationSets, sync, health, and rollback;
- PostgreSQL restart durability plus exact managed-Valkey Deployment/PVC dataset deletion, PostgreSQL outbox reconstruction into the empty dataset, exactly-once operation convergence, and restoration of both scaled Deployments;
- Git projection/index/write concurrency and normal fast-forward CAS;
- source-build retry, immutable image digest observation, and promotion (cache
  cold-start and split push-credential fault injection require a separate live
  workflow and are not claimed by this harness);
- Traefik Ingress and Middleware CRDs plus HTTP/custom/local-ACME TLS paths;
- cert-manager and external-dns generation/reconciliation with test backends;
- secure runtime-secret integrations and non-disclosure checks;
- guided/YAML API behavior, OpenAPI/Swagger endpoints, and browser UI;
- Pod/Deployment logs, events, Prometheus dashboards, and authorization filters;
- namespace RBAC, exact ResourceQuota admission rejection, admission policy,
  scenario-pinned ready CNI identity, enforced NetworkPolicy deny/allow
  connectivity, and the safe actor/resource/outcome audit timeline;
- retries under deleted Pods, restarted components, stale Git revisions, and
  duplicate webhooks.

The execution order is:

1. Read-only identity, version, and capability preflight.
2. Disposable namespace plus Git and registry smoke test.
3. PostgreSQL and Valkey durability/delivery test.
4. Argo CD and runtime-chart deployment by digest.
5. Traefik route, middleware, custom TLS, and local-ACME test.
6. Signed GitHub webhook replay, missed-event polling, and build/deploy flow.
7. Monitoring, logs, RBAC, admission, and isolation probes.
8. Upgrade/rollback, artifact report, and exact cleanup verification.

A single-node conforming cluster cannot prove multi-node scheduling, failure
tolerance, cloud load balancers/storage/KMS, production scale, or long-duration
soak behavior. Those require separate disposable multi-node CI environments.
Native amd64 and arm64 image builds plus an OCI-index merge prove release-image
platform coverage; a local engine or one Kubernetes cluster cannot prove it.
