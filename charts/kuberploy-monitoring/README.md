# Kuberploy managed monitoring release

This wrapper is an independent, protected Helm/Argo release around
`kube-prometheus-stack` 88.1.5. It is deliberately not a dependency of the
Kuberploy control-plane chart. The chart package checksum, dependency lock, and
all images used by the managed profile are immutable.

## Ownership and access

Install the release only into `kuberploy-monitoring`. It owns the namespace,
Prometheus Operator CRDs, one Prometheus replica, Alertmanager,
kube-state-metrics, node-exporter, and the Kuberploy recording rules. Grafana,
Ingress/Gateway routes, remote-write receivers, the Prometheus admin API,
arbitrary scrape configuration, and injected containers are disabled.

Prometheus remains ClusterIP-only. Port 9090 accepts ingress only from a Pod
with `app.kubernetes.io/name=kuberploy` and
`app.kubernetes.io/component=api` in a namespace carrying
`kuberploy.io/control-plane-namespace=true`. This label is an administrator
owned namespace trust decision; a tenant must never be allowed to set it.

`monitoring.networkPolicy.kubeAPIServerCIDRs` must contain the cluster's exact
API Service and/or node endpoint identities as `/32` or `/128` CIDRs. The
installer derives this value from `cluster.kubeAPIServerCIDRs`. This explicit
allowlist is required because some Kubernetes distributions apply Service DNAT
before egress policy evaluation; the three default private ranges alone cannot
authorize a public node API endpoint.

ServiceMonitor and PodMonitor discovery requires both:

- the object's namespace label `kuberploy.io/monitoring-namespace=true`; and
- the object label `kuberploy.io/monitoring-source=protected`.

Approved monitors live in `kuberploy-monitoring`. Their namespace selector is
honored, so the protected Traefik monitor can select only
`kuberploy-system`; the wrapper locks that exact selector and rejects arbitrary
targets. PrometheusRule discovery is limited to protected rules in
`kuberploy-monitoring`.

## Runtime identity and metrics

kube-state-metrics projects only the four Kuberploy labels from Pods,
Deployments, Services, and Ingresses: `kuberploy.io/project`,
`kuberploy.io/environment`, `kuberploy.io/application`, and
`kuberploy.io/service`. Wildcards and annotation projection are forbidden.

The protected PrometheusRule emits exactly the seven names consumed by
`internal/observability`:

- `kuberploy:service:cpu_usage_cores`
- `kuberploy:service:memory_working_set_bytes`
- `kuberploy:service:replicas_ready`
- `kuberploy:service:container_restarts_total`
- `kuberploy:service:http_requests_per_second`
- `kuberploy:service:http_5xx_ratio`
- `kuberploy:service:http_latency_seconds:p95`

Each output is reduced to `namespace`, `kuberploy_project`,
`kuberploy_environment`, `kuberploy_application`, and `kuberploy_service`.
HTTP rules join Traefik's Kubernetes service metric name to the immutable
`kp-a-<hash>` runtime Service. They produce no series when compatible Traefik
metrics are absent; absence is never converted to a false zero.

## Storage and security

Prometheus retention, retention-size cap, CPU/memory resources, PVC size, and
storage class are the only upstream values that may be changed. PVCs use
`ReadWriteOnce` and are retained when the StatefulSet is deleted or scaled.
Scrapes have enforced sample, target, label, body-size, and dropped-target
limits. Kubelet scraping verifies the serving certificate against the projected
cluster CA and uses only the Prometheus Pod's mounted service-account token and
cluster CA. The protected monitor namespace and exact selector remain the
authority boundary. Clusters whose kubelet certificate cannot be validated
fail closed.

node-exporter reads the host's `/proc` and `/sys` through read-only hostPath
mounts. Therefore the namespace must use the `privileged` Pod Security
enforcement level; it also audits and warns against `restricted`. The DaemonSet
does not use host networking, host PID/IPC, host root, host ports, privileged
containers, added capabilities, or a ServiceAccount token. All other managed
containers run non-root with RuntimeDefault seccomp, read-only root filesystems
where their upstream API permits it, and bounded resources.

Do not enable the product monitoring capability merely because Helm completed.
The API reads the immutable `monitoring-monitoring-profile`, the exact operator
Deployment, and the exact protected PrometheusRule through resource-name-scoped
`get` permissions. It verifies the fixed release/chart identity, pinned
operator image and arguments digest, current Deployment generation, and rule
spec digest and exact Prometheus scrape policy. It then requires a healthy
query probe, all seven recording rules to be uniquely loaded with no evaluation
error, and healthy active targets for kube-state-metrics, kubelet/cAdvisor, and
Traefik. Any drift or missing source keeps the capability and monitoring status
fail-closed.
