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
  {{- if not (or (regexMatch "^[^[:space:]@]+:v?[0-9]+\\.[0-9]+\\.[0-9]+(?:[-.][A-Za-z0-9]+)*$" $image) (regexMatch "^[^[:space:]@]+@sha256:[a-f0-9]{64}$" $image)) -}}
    {{- fail "builderAgentImage must use an explicit text version or a release integrity reference" -}}
  {{- end -}}
  {{- if not .Values.admissionPolicy.enabled -}}
    {{- fail "admissionPolicy.enabled must remain true for the privileged DinD namespace" -}}
  {{- end -}}
  {{- if not .Values.networkPolicy.enabled -}}
    {{- fail "networkPolicy.enabled must remain true for the privileged DinD namespace" -}}
  {{- end -}}
{{- end -}}
{{- end -}}
