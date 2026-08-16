{{- define "kuberploy-builder.labels" -}}
app.kubernetes.io/name: kuberploy-builder
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-builder.policyName" -}}
{{- printf "kuberploy-dind-%s" (.Values.namespace.name | sha256sum | trunc 12) -}}
{{- end -}}

{{- define "kuberploy-builder.controllerUsername" -}}
{{- printf "system:serviceaccount:%s:%s" .Values.controllerServiceAccount.namespace .Values.controllerServiceAccount.name -}}
{{- end -}}

{{- define "kuberploy-builder.validate" -}}
{{- if .Values.enabled -}}
  {{- $image := required "builderAgentImage is required when enabled" .Values.builderAgentImage -}}
  {{- if not (regexMatch "^[^[:space:]]+$" $image) -}}
    {{- fail "builderAgentImage must be a non-empty Kubernetes image reference" -}}
  {{- end -}}
  {{- if not (regexMatch "^(?:[^[:space:]@]+:v0[.]32[.]2|[^[:space:]@]+@sha256:[0-9a-f]{64})$" .Values.buildKitImage) -}}
    {{- fail "buildKitImage must be v0.32.2 or an immutable sha256 image reference" -}}
  {{- end -}}
  {{- if not (regexMatch "^(?:[^[:space:]@]+:v?[0-9]+[.][0-9]+[.][0-9]+(?:[-.][A-Za-z0-9]+)*|[^[:space:]@]+@sha256:[0-9a-f]{64})$" .Values.dindImage) -}}
    {{- fail "dindImage must use an explicit semantic version or sha256 digest" -}}
  {{- end -}}
  {{- if gt (add (len .Values.networkPolicy.sourceEgressCIDRs) (len .Values.networkPolicy.registryEgressCIDRs)) 128 -}}
    {{- fail "source and registry egress lists may contain at most 128 entries in total" -}}
  {{- end -}}
  {{- range $cidr := .Values.networkPolicy.sourceEgressCIDRs -}}
    {{- if not (or (regexMatch `^(?:[0-9]{1,3}\.){3}[0-9]{1,3}/(?:[89]|[12][0-9]|3[0-2])$` $cidr) (regexMatch `^[0-9a-f:]+/(?:1[6-9]|[2-9][0-9]|1[01][0-9]|12[0-8])$` $cidr)) -}}
      {{- fail "sourceEgressCIDRs must contain only bounded IPv4 /8-/32 or IPv6 /16-/128 ranges" -}}
    {{- end -}}
  {{- end -}}
  {{- range $cidr := .Values.networkPolicy.registryEgressCIDRs -}}
    {{- if not (regexMatch `(?:/32|/128)$` $cidr) -}}
      {{- fail "registryEgressCIDRs must contain only exact /32 or /128 hosts" -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}
