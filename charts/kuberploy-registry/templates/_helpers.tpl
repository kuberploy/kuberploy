{{- define "kuberploy-registry.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy-registry.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-registry.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kuberploy-registry.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: registry
{{- end -}}

{{- define "kuberploy-registry.labels" -}}
{{ include "kuberploy-registry.selectorLabels" . }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- with .Values.global.testRun }}
kuberploy.io/test-run: {{ . | quote }}
{{- end }}
{{- end -}}

{{- define "kuberploy-registry.serviceAccountName" -}}
{{- printf "%s" (include "kuberploy-registry.fullname" .) -}}
{{- end -}}

{{- define "kuberploy-registry.claimName" -}}
{{- default (include "kuberploy-registry.fullname" .) .Values.persistence.existingClaim -}}
{{- end -}}

{{- define "kuberploy-registry.image" -}}
{{- $reference := required "image.reference is required" .Values.image.reference -}}
{{- if ne $reference "docker.io/library/registry:3.1.1" -}}
{{- fail "image.reference must use the locked OCI Distribution 3.1.1 release" -}}
{{- end -}}
{{- $reference -}}
{{- end -}}

{{- define "kuberploy-registry.configName" -}}
{{- $config := dict "authMode" .Values.auth.mode "authRealm" .Values.auth.realm "port" .Values.service.port -}}
{{- printf "%s-config-%s" (include "kuberploy-registry.fullname" .) (toJson $config | sha256sum | trunc 12) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy-registry.validate" -}}
{{- if .Values.enabled -}}
  {{- $_ := include "kuberploy-registry.image" . -}}
  {{- if ne .Values.service.type "ClusterIP" -}}
    {{- fail "service.type must remain ClusterIP; edge exposure belongs to Traefik/cert-manager" -}}
  {{- end -}}
  {{- $exposure := .Values.exposure -}}
  {{- if ne $exposure.mode "internal" -}}
    {{- if ne .Values.auth.mode "htpasswd" -}}{{ fail "registry exposure requires htpasswd authentication" }}{{- end -}}
    {{- if or (ne $exposure.ingressClassName "traefik") (empty $exposure.endpoint) (empty $exposure.secretName) (empty $exposure.clusterIssuerName) -}}
      {{- fail "registry exposure requires exact Traefik endpoint and cert-manager TLS identities" -}}
    {{- end -}}
  {{- end -}}
  {{- if eq $exposure.mode "loadBalancer" -}}
    {{- if ne .Release.Namespace "kuberploy-system" -}}{{ fail "registry LoadBalancer exposure requires the shared Traefik namespace kuberploy-system" }}{{- end -}}
    {{- if eq (len $exposure.loadBalancer.sourceRanges) 0 -}}{{ fail "registry LoadBalancer requires explicit source ranges" }}{{- end -}}
    {{- if or (hasKey $exposure.loadBalancer.annotations "external-dns.alpha.kubernetes.io/cloudflare-proxied") (hasKey $exposure.loadBalancer.annotations "external-dns.kubernetes.io/cloudflare-proxied") -}}{{ fail "registry LoadBalancer Cloudflare proxy policy is server-owned DNS-only" }}{{- end -}}
  {{- end -}}
  {{- if eq .Values.auth.mode "htpasswd" -}}
    {{- $_ := required "auth.existingSecret is required when enabled with htpasswd authentication" .Values.auth.existingSecret -}}
  {{- else if eq .Values.auth.mode "testOnlyUnauthenticated" -}}
    {{- if .Values.auth.existingSecret -}}
      {{- fail "auth.existingSecret must be empty in testOnlyUnauthenticated mode" -}}
    {{- end -}}
  {{- else -}}
    {{- fail "auth.mode must be htpasswd or testOnlyUnauthenticated" -}}
  {{- end -}}
  {{- if and .Values.networkPolicy.enabled (eq (len .Values.networkPolicy.allowedNamespaces) 0) -}}
    {{- fail "networkPolicy.allowedNamespaces must contain at least one explicit namespace when enabled" -}}
  {{- end -}}
{{- end -}}
{{- end -}}
