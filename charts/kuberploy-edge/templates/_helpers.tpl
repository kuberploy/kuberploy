{{- define "kuberploy-edge.labels" -}}
app.kubernetes.io/name: kuberploy-edge
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-edge.validatePublicIPv4" -}}
{{- $value := . -}}
{{- if not (kindIs "string" $value) -}}
  {{- fail "edge.traefik.sslip.staticPublicIPv4 must be one canonical public IPv4 string" -}}
{{- end -}}
{{- if not (regexMatch "^(0|[1-9][0-9]{0,2})[.](0|[1-9][0-9]{0,2})[.](0|[1-9][0-9]{0,2})[.](0|[1-9][0-9]{0,2})$" $value) -}}
  {{- fail "edge.traefik.sslip.staticPublicIPv4 must be one canonical public IPv4 string" -}}
{{- end -}}
{{- $parts := splitList "." $value -}}
{{- $a := int (index $parts 0) -}}
{{- $b := int (index $parts 1) -}}
{{- $c := int (index $parts 2) -}}
{{- $d := int (index $parts 3) -}}
{{- if or (gt $a 255) (gt $b 255) (gt $c 255) (gt $d 255) -}}
  {{- fail "edge.traefik.sslip.staticPublicIPv4 must be one canonical public IPv4 string" -}}
{{- end -}}
{{- $forbidden192 := and (eq $a 192) (or (and (eq $b 0) (or (eq $c 0) (eq $c 2))) (and (eq $b 88) (eq $c 99)) (eq $b 168)) -}}
{{- $forbidden198 := and (eq $a 198) (or (eq $b 18) (eq $b 19) (and (eq $b 51) (eq $c 100))) -}}
{{- if or (eq $a 0) (eq $a 10) (eq $a 127) (and (eq $a 100) (ge $b 64) (le $b 127)) (and (eq $a 169) (eq $b 254)) (and (eq $a 172) (ge $b 16) (le $b 31)) $forbidden192 $forbidden198 (and (eq $a 203) (eq $b 0) (eq $c 113)) (ge $a 224) -}}
  {{- fail "edge.traefik.sslip.staticPublicIPv4 must be globally routable and outside reserved, private, documentation, and benchmark ranges" -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-edge.validate" -}}
{{- if ne .Release.Namespace "kuberploy-system" -}}
{{- fail "kuberploy-edge must use the shared protected kuberploy-system namespace" -}}
{{- end -}}
{{- if eq .Values.edge.traefik.managed .Values.edge.traefik.adoptExisting -}}
{{- fail "exactly one of edge.traefik.managed or edge.traefik.adoptExisting must be true" -}}
{{- end -}}
{{- if and .Values.edge.traefik.adoptExisting (not .Values.edge.traefik.crdProviderConfirmed) -}}
{{- fail "adopting Traefik requires edge.traefik.crdProviderConfirmed=true" -}}
{{- end -}}
{{- if hasKey .Values.edge.traefik "sslip" -}}
  {{- $sslip := get .Values.edge.traefik "sslip" -}}
  {{- if not (kindIs "map" $sslip) -}}
    {{- fail "edge.traefik.sslip must be omitted or one exact mode object" -}}
  {{- end -}}
  {{- $mode := get $sslip "mode" -}}
  {{- $keys := join "," (sortAlpha (keys $sslip)) -}}
  {{- if eq $mode "auto-first-ip" -}}
    {{- /* A known static value is dormant in automatic mode and is ignored. */ -}}
  {{- else if eq $mode "verified-static-ip" -}}
    {{- if ne $keys "mode,staticPublicIPv4" -}}
      {{- fail "verified-static-ip requires exactly mode and staticPublicIPv4" -}}
    {{- end -}}
    {{- include "kuberploy-edge.validatePublicIPv4" (get $sslip "staticPublicIPv4") -}}
  {{- else -}}
    {{- fail "edge.traefik.sslip.mode must be auto-first-ip or verified-static-ip" -}}
  {{- end -}}
{{- end -}}
{{- if .Values.edge.traefik.managed -}}
  {{- range .Values.edge.networkPolicy.kubeAPIServerCIDRs -}}
    {{- if not $.Values.edge.networkPolicy.enabled -}}{{- continue -}}{{- end -}}
    {{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "Traefik API-server CIDRs cannot be an all-address range" }}{{- end -}}
  {{- end -}}
  {{- if or (ne .Values.traefik.nameOverride "traefik") (not (empty .Values.traefik.namespaceOverride)) (not (empty .Values.traefik.instanceLabelOverride)) (not (empty .Values.traefik.fullnameOverride)) (not (empty .Values.traefik.commonLabels)) -}}{{ fail "Traefik namespace and policy identity labels are locked" }}{{- end -}}
  {{- if or (not .Values.traefik.deployment.enabled) (ne .Values.traefik.deployment.kind "Deployment") (lt (int .Values.traefik.deployment.replicas) 1) -}}{{ fail "managed Traefik requires at least one Deployment replica" }}{{- end -}}
  {{- if or .Values.traefik.hostNetwork .Values.traefik.deployment.shareProcessNamespace -}}{{ fail "managed Traefik cannot use host or shared process namespaces" }}{{- end -}}
  {{- if or (not (empty .Values.traefik.deployment.additionalContainers)) (not (empty .Values.traefik.deployment.additionalVolumes)) (not (empty .Values.traefik.deployment.initContainers)) -}}{{ fail "managed Traefik does not permit injected containers or volumes" }}{{- end -}}
  {{- if or (not (empty .Values.traefik.deployment.podLabels)) (not (empty .Values.traefik.deployment.podAnnotations)) -}}{{ fail "managed Traefik Pod identity labels and injection annotations cannot be overridden" }}{{- end -}}
  {{- if or (ne .Values.traefik.updateStrategy.type "RollingUpdate") (ne (int .Values.traefik.updateStrategy.rollingUpdate.maxUnavailable) 0) (ne (int .Values.traefik.updateStrategy.rollingUpdate.maxSurge) 1) -}}{{ fail "managed Traefik requires zero-unavailable rolling updates" }}{{- end -}}
  {{- if or (not .Values.traefik.rbac.enabled) .Values.traefik.rbac.namespaced (not (empty .Values.traefik.rbac.aggregateTo)) (not (empty .Values.traefik.serviceAccount.name)) (not (empty .Values.traefik.serviceAccountAnnotations)) -}}{{ fail "managed multi-namespace Traefik requires its exact dedicated upstream RBAC and ServiceAccount" }}{{- end -}}
  {{- if or (not .Values.traefik.ingressClass.enabled) .Values.traefik.ingressClass.isDefaultClass (empty .Values.traefik.ingressClass.name) -}}{{ fail "managed Traefik requires an explicit non-default IngressClass" }}{{- end -}}
  {{- if or (not .Values.traefik.providers.kubernetesCRD.enabled) .Values.traefik.providers.kubernetesCRD.allowCrossNamespace .Values.traefik.providers.kubernetesCRD.allowExternalNameServices .Values.traefik.providers.kubernetesCRD.allowEmptyServices -}}{{ fail "managed Traefik CRD provider safety settings cannot be relaxed" }}{{- end -}}
  {{- if or (not .Values.traefik.providers.kubernetesIngress.enabled) .Values.traefik.providers.kubernetesIngress.allowExternalNameServices .Values.traefik.providers.kubernetesIngress.allowEmptyServices -}}{{ fail "managed Traefik Ingress provider safety settings cannot be relaxed" }}{{- end -}}
  {{- if ne .Values.traefik.providers.kubernetesIngress.ingressClass .Values.traefik.ingressClass.name -}}{{ fail "Traefik provider and IngressClass names must match" }}{{- end -}}
  {{- if or .Values.traefik.providers.kubernetesGateway.enabled .Values.traefik.gateway.enabled -}}{{ fail "Gateway API is outside this managed edge profile" }}{{- end -}}
  {{- if or .Values.traefik.providers.file.enabled .Values.traefik.providers.kubernetesIngressNGINX.enabled -}}{{ fail "unmanaged Traefik providers are disabled" }}{{- end -}}
  {{- if or (not (empty .Values.traefik.experimental.plugins)) (not (empty .Values.traefik.experimental.localPlugins)) .Values.traefik.hub.enabled (not (empty .Values.traefik.hub.token)) -}}{{ fail "Traefik plugins and Hub are outside the managed edge trust boundary" }}{{- end -}}
  {{- if or .Values.traefik.api.dashboard .Values.traefik.api.insecure .Values.traefik.api.debug .Values.traefik.ingressRoute.dashboard.enabled .Values.traefik.ingressRoute.healthcheck.enabled -}}{{ fail "Traefik API and internal routes must remain disabled" }}{{- end -}}
  {{- if or (not .Values.traefik.accessLog.enabled) (ne .Values.traefik.accessLog.format "json") (ne .Values.traefik.accessLog.fields.headers.defaultMode "drop") (not (empty .Values.traefik.accessLog.fields.headers.names)) (not (empty .Values.traefik.accessLog.fields.names)) (ne .Values.traefik.accessLog.fields.queryParameters.defaultMode "drop") -}}{{ fail "Traefik access logs must remain JSON with headers and query parameters dropped" }}{{- end -}}
  {{- $metrics := .Values.traefik.metrics -}}
  {{- $prometheus := $metrics.prometheus -}}
  {{- $monitor := $prometheus.serviceMonitor -}}
  {{- if or $metrics.addInternals (ne $prometheus.entryPoint "metrics") (not $prometheus.addEntryPointsLabels) $prometheus.addRoutersLabels (not $prometheus.addServicesLabels) $prometheus.manualRouting (not (empty $prometheus.headerLabels)) -}}{{ fail "managed Traefik metrics use only the protected service and entrypoint label set" }}{{- end -}}
  {{- if or (not $prometheus.service.enabled) (ne (get $prometheus.service.labels "kuberploy.io/monitoring-source") "protected") (not (empty $prometheus.service.annotations)) -}}{{ fail "managed Traefik requires its exact protected ClusterIP metrics Service" }}{{- end -}}
  {{- if $monitor.enabled -}}
    {{- if or (not $prometheus.disableAPICheck) (ne $monitor.apiVersion "monitoring.coreos.com/v1") (ne $monitor.namespace "kuberploy-monitoring") (ne (get $monitor.additionalLabels "kuberploy.io/monitoring-source") "protected") (ne $monitor.interval "30s") (ne $monitor.scrapeTimeout "10s") $monitor.honorLabels $monitor.honorTimestamps $monitor.enableHttp2 $monitor.followRedirects (not (empty $monitor.relabelings)) -}}{{ fail "managed Traefik requires its exact protected ServiceMonitor" }}{{- end -}}
    {{- if not (deepEqual $monitor.namespaceSelector.matchNames (list "kuberploy-system")) -}}{{ fail "the Traefik ServiceMonitor may select only kuberploy-system" }}{{- end -}}
    {{- if or (ne (len $monitor.metricRelabelings) 1) (ne (index $monitor.metricRelabelings 0).action "keep") (not (deepEqual (index $monitor.metricRelabelings 0).sourceLabels (list "__name__"))) (ne (index $monitor.metricRelabelings 0).regex "traefik_service_requests_total|traefik_service_request_duration_seconds_bucket") -}}{{ fail "the Traefik ServiceMonitor metric allowlist is immutable" }}{{- end -}}
  {{- end -}}
  {{- if $prometheus.prometheusRule.enabled -}}{{ fail "Traefik recording rules are owned by the monitoring release" }}{{- end -}}
  {{- if or .Values.traefik.ports.web.forwardedHeaders.insecure .Values.traefik.ports.web.proxyProtocol.insecure .Values.traefik.ports.websecure.forwardedHeaders.insecure .Values.traefik.ports.websecure.proxyProtocol.insecure -}}{{ fail "untrusted forwarded headers and proxy protocol are forbidden" }}{{- end -}}
  {{- if or (not .Values.traefik.service.enabled) (ne .Values.traefik.service.spec.type "LoadBalancer") -}}{{ fail "managed Traefik must use its LoadBalancer Service" }}{{- end -}}
  {{- range $portName, $port := .Values.traefik.ports -}}
    {{- if not (has $portName (list "traefik" "web" "websecure" "metrics")) -}}{{ fail "managed Traefik accepts only web, websecure, health, and metrics ports" }}{{- end -}}
    {{- if not (empty $port.hostPort) -}}{{ fail "managed LoadBalancer Traefik cannot bind host ports" }}{{- end -}}
  {{- end -}}
  {{- if or .Values.traefik.ports.traefik.expose.default .Values.traefik.ports.metrics.expose.default (not .Values.traefik.ports.web.expose.default) (not .Values.traefik.ports.websecure.expose.default) -}}{{ fail "only Traefik web and websecure ports may be public" }}{{- end -}}
  {{- if or (ne (int .Values.traefik.ports.web.port) 8000) (ne (int .Values.traefik.ports.web.exposedPort) 80) (ne .Values.traefik.ports.web.protocol "TCP") (ne (int .Values.traefik.ports.websecure.port) 8443) (ne (int .Values.traefik.ports.websecure.exposedPort) 443) (ne .Values.traefik.ports.websecure.protocol "TCP") -}}{{ fail "managed Traefik public ports are locked to TCP 80 and 443" }}{{- end -}}
  {{- if or (not (empty .Values.traefik.ports.web.http.redirections.entryPoint)) (not (empty .Values.traefik.ports.websecure.http.middlewares)) (not (empty .Values.traefik.ports.websecure.http.tls.options)) (not (empty .Values.traefik.ports.websecure.http.tls.certResolver)) (not (empty .Values.traefik.ports.websecure.http.tls.domains)) -}}{{ fail "global redirects, middleware, and certificate resolvers are forbidden because TLS is selected per route" }}{{- end -}}
  {{- if or .Values.traefik.persistence.enabled (not (empty .Values.traefik.certificatesResolvers)) -}}{{ fail "cert-manager, not Traefik ACME storage, owns certificates" }}{{- end -}}
  {{- if or (not (empty .Values.traefik.tlsStore)) (not (empty .Values.traefik.tlsOptions)) -}}{{ fail "global TLS stores and dependency-injected TLS options are forbidden; route Secrets remain namespace-local" }}{{- end -}}
  {{- if or (not (empty .Values.traefik.env)) (not (empty .Values.traefik.envFrom)) (not (empty .Values.traefik.additionalArguments)) (not (empty .Values.traefik.additionalVolumeMounts)) (not (empty .Values.traefik.volumes)) (not (empty .Values.traefik.extraObjects)) -}}{{ fail "managed Traefik does not permit arbitrary process or object injection" }}{{- end -}}
  {{- if or .Values.traefik.securityContext.allowPrivilegeEscalation (not .Values.traefik.securityContext.readOnlyRootFilesystem) (not (deepEqual .Values.traefik.securityContext.capabilities.drop (list "ALL"))) -}}{{ fail "Traefik container security must remain restricted" }}{{- end -}}
  {{- if or (not .Values.traefik.podSecurityContext.runAsNonRoot) (ne .Values.traefik.podSecurityContext.seccompProfile.type "RuntimeDefault") -}}{{ fail "Traefik pod security must remain restricted" }}{{- end -}}
  {{- if empty .Values.traefik.topologySpreadConstraints -}}{{ fail "managed LoadBalancer Traefik requires topology spread" }}{{- end -}}
{{- end -}}
{{- end -}}
