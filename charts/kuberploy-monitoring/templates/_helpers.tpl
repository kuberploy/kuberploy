{{- define "kuberploy-monitoring.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy-monitoring.fullname" -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy-monitoring.labels" -}}
app.kubernetes.io/name: {{ include "kuberploy-monitoring.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
kuberploy.io/ownership-boundary: monitoring-only
{{- end -}}

{{- define "kuberploy-monitoring.validate" -}}
{{- if ne .Release.Name "monitoring" -}}
  {{- fail "kuberploy-monitoring must use the fixed monitoring release identity" -}}
{{- end -}}
{{- if ne .Release.Namespace "kuberploy-monitoring" -}}
  {{- fail "kuberploy-monitoring must be installed in the fixed kuberploy-monitoring namespace" -}}
{{- end -}}
{{- if not .Values.monitoring.managed -}}
  {{- fail "kuberploy-monitoring is an independently managed release; monitoring.managed must remain true" -}}
{{- end -}}
{{- if not .Values.monitoring.namespace.create -}}
  {{- fail "the managed monitoring release must own its fixed namespace" -}}
{{- end -}}
{{- if not .Values.monitoring.networkPolicy.enabled -}}
  {{- fail "monitoring NetworkPolicies may not be disabled" -}}
{{- end -}}
{{- $monitoringGuard := deepCopy .Values.monitoring -}}
{{- $_ := unset $monitoringGuard.networkPolicy "kubeAPIServerCIDRs" -}}
{{- if ne (sha256sum (toJson $monitoringGuard)) "7b835a7ee3c5e0e6400230fe84a739f8bef96373e1c46d0a22ac0daa90574d2d" -}}
  {{- fail "monitoring ownership and query-client selectors are immutable" -}}
{{- end -}}
{{- if empty .Values.monitoring.networkPolicy.kubeAPIServerCIDRs -}}
  {{- fail "managed Prometheus discovery requires exact Kubernetes API server CIDRs" -}}
{{- end -}}
{{- $seenAPICIDRs := dict -}}
{{- range $cidr := .Values.monitoring.networkPolicy.kubeAPIServerCIDRs -}}
  {{- if not (or (regexMatch "^(?:[0-9]{1,3}\\.){3}[0-9]{1,3}/32$" $cidr) (regexMatch "^[0-9A-Fa-f:]+/128$" $cidr)) -}}
    {{- fail "monitoring Kubernetes API server CIDRs must be exact IPv4 /32 or IPv6 /128 identities" -}}
  {{- end -}}
  {{- if hasKey $seenAPICIDRs $cidr -}}{{ fail "monitoring Kubernetes API server CIDRs must be unique" }}{{- end -}}
  {{- $_ := set $seenAPICIDRs $cidr true -}}
{{- end -}}
{{- $stack := index .Values "kube-prometheus-stack" -}}
{{- $guarded := deepCopy $stack -}}
{{- $guardedPrometheus := get (get $guarded "prometheus") "prometheusSpec" -}}
{{- $_ := unset $guardedPrometheus "retention" -}}
{{- $_ := unset $guardedPrometheus "retentionSize" -}}
{{- $_ := unset $guardedPrometheus "resources" -}}
{{- $_ := unset $guardedPrometheus "storageSpec" -}}
{{- $guardedHash := sha256sum (toJson $guarded) -}}
{{- if not (has $guardedHash (list "9e7e674433cb780480753ff952140220585ec1f396ae883d421604ce99ced408" "cc206bc68c2502341a4f882fe2a741dcc0c5c7eda5e9c8d57602a1b7c7d59314" "27f90edac8b5f59543970443cde35d28d757ff1023e76d62170cc9fb0f9a16b0")) -}}
  {{- fail (printf "only Prometheus retention, PVC, and resources are configurable; the managed upstream profile is otherwise immutable (%s)" $guardedHash) -}}
{{- end -}}
{{- if $stack.grafana.enabled -}}{{ fail "Grafana is disabled in the managed foundation" }}{{- end -}}
{{- if or $stack.prometheus.ingress.enabled $stack.alertmanager.ingress.enabled -}}{{ fail "monitoring UIs may not render Ingress resources" }}{{- end -}}
{{- if or (ne $stack.prometheus.service.type "ClusterIP") (ne $stack.alertmanager.service.type "ClusterIP") (ne (index $stack "kube-state-metrics").service.type "ClusterIP") (ne (index $stack "prometheus-node-exporter").service.type "ClusterIP") -}}
  {{- fail "all monitoring Services must remain ClusterIP" -}}
{{- end -}}
{{- if ne (int $stack.prometheus.prometheusSpec.replicas) 1 -}}{{ fail "managed Prometheus must have exactly one replica" }}{{- end -}}
{{- if $stack.prometheus.prometheusSpec.ignoreNamespaceSelectors -}}{{ fail "protected ServiceMonitors must retain their exact namespace selectors" }}{{- end -}}
{{- $fsGuard := $stack.prometheus.prometheusSpec.arbitraryFSAccessThroughSMs -}}
{{- if or (not (kindIs "map" $fsGuard)) (ne (keys $fsGuard | sortAlpha | join ",") "deny") $fsGuard.deny -}}{{ fail "the protected kubelet monitor requires only the mounted service-account token and cluster CA" }}{{- end -}}
{{- if or $stack.prometheus.prometheusSpec.enableAdminAPI $stack.prometheus.prometheusSpec.enableRemoteWriteReceiver $stack.prometheus.prometheusSpec.enableOTLPReceiver -}}
  {{- fail "Prometheus write/admin receivers are disabled" -}}
{{- end -}}
{{- if or (gt (len $stack.prometheus.prometheusSpec.additionalScrapeConfigs) 0) (gt (len $stack.prometheus.prometheusSpec.additionalArgs) 0) (gt (len $stack.prometheus.prometheusSpec.containers) 0) (gt (len $stack.prometheus.prometheusSpec.initContainers) 0) -}}
  {{- fail "unreviewed Prometheus scrape configuration, arguments, and container injection are forbidden" -}}
{{- end -}}
{{- if or $stack.prometheusOperator.admissionWebhooks.enabled $stack.prometheusOperator.hostNetwork (gt (len $stack.prometheusOperator.extraArgs) 0) -}}
  {{- fail "operator admission hooks, host networking, and argument injection are disabled" -}}
{{- end -}}
{{- $ksm := index $stack "kube-state-metrics" -}}
{{- range $allow := $ksm.metricLabelsAllowlist -}}
  {{- if contains "*" $allow -}}{{ fail "kube-state-metrics wildcard label allowlists are forbidden" }}{{- end -}}
{{- end -}}
{{- if gt (len $ksm.metricAnnotationsAllowList) 0 -}}{{ fail "Kubernetes annotations may not be projected into metrics" }}{{- end -}}
{{- $node := index $stack "prometheus-node-exporter" -}}
{{- if or $node.hostNetwork $node.hostPID $node.hostIPC -}}{{ fail "node-exporter host namespaces are forbidden" }}{{- end -}}
{{- if or (gt (len $node.extraHostVolumeMounts) 0) (gt (len $node.sidecars) 0) (gt (len $node.extraInitContainers) 0) -}}
  {{- fail "node-exporter host mount and container injection are forbidden" -}}
{{- end -}}
{{- end -}}
