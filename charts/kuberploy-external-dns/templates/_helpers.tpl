{{- define "kuberploy-external-dns.labels" -}}
app.kubernetes.io/name: kuberploy-external-dns-foundation
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-external-dns.validate" -}}
{{- if not .Values.foundation.enabled -}}
  {{- if or .Values.foundation.externalDNS.managed .Values.foundation.externalDNS.adoptExisting -}}{{ fail "disabled external-dns foundation cannot manage or adopt an integration" }}{{- end -}}
  {{- $identity := .Values.foundation.externalDNS.identity -}}
  {{- if or $identity.providerKind $identity.credentialSecretRef $identity.providerConfigRef $identity.egressConfigRef -}}{{ fail "disabled external-dns foundation rejects dormant runtime identity" }}{{- end -}}
{{- else -}}
  {{- if ne .Release.Namespace "kuberploy-system" -}}{{ fail "kuberploy-external-dns must use the shared protected kuberploy-system namespace" }}{{- end -}}
  {{- if eq .Values.foundation.externalDNS.managed .Values.foundation.externalDNS.adoptExisting -}}{{ fail "enabled integration requires exactly one managed or adopted external-dns" }}{{- end -}}
  {{- if and .Values.foundation.externalDNS.adoptExisting (not .Values.foundation.externalDNS.filtersConfirmed) -}}{{ fail "external-dns adoption requires foundation.externalDNS.filtersConfirmed=true" }}{{- end -}}
  {{- if not .Values.foundation.networkPolicy.enabled -}}{{ fail "foundation.networkPolicy.enabled must remain true" }}{{- end -}}
  {{- if not (deepEqual .Values.externaldns.sources (list "ingress")) -}}{{ fail "external-dns sources are locked to Ingress" }}{{- end -}}
  {{- if not (has .Values.externaldns.policy (list "upsert-only" "sync")) -}}{{ fail "external-dns policy must be upsert-only or sync" }}{{- end -}}
  {{- if and (eq .Values.externaldns.policy "sync") (not .Values.foundation.allowDestructiveSync) -}}{{ fail "sync policy requires foundation.allowDestructiveSync=true" }}{{- end -}}
  {{- if ne .Values.externaldns.registry "txt" -}}{{ fail "external-dns registry is locked to TXT ownership" }}{{- end -}}
  {{- if empty .Values.externaldns.txtOwnerId -}}{{ fail "externalDns.txtOwnerId is required and must be unique per integration" }}{{- end -}}
  {{- if empty .Values.externaldns.domainFilters -}}{{ fail "externalDns.domainFilters must contain at least one managed suffix" }}{{- end -}}
  {{- if not (regexMatch "^kuberploy\\.io/dns-integration=[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .Values.externaldns.labelFilter) -}}{{ fail "externalDns.labelFilter must select exactly one kuberploy.io/dns-integration value" }}{{- end -}}
  {{- if ne .Values.externaldns.annotationFilter "external-dns.alpha.kubernetes.io/hostname" -}}{{ fail "external-dns hostname annotation opt-in must remain enabled" }}{{- end -}}
  {{- if not (kindIs "map" .Values.externaldns.provider) -}}{{ fail "externalDns.provider must be an object" }}{{- end -}}
  {{- if empty .Values.externaldns.provider.name -}}{{ fail "externalDns.provider.name is required" }}{{- end -}}
  {{- $identity := .Values.foundation.externalDNS.identity -}}
  {{- if or (not (has $identity.providerKind (list "aws" "azure" "cloudflare" "google" "rfc2136"))) (ne $identity.providerKind .Values.externaldns.provider.name) -}}{{ fail "external-dns runtime provider identity must match the deployed provider" }}{{- end -}}
  {{- if eq .Values.externaldns.provider.name "webhook" -}}{{ fail "webhook providers require a separately locked adapter" }}{{- end -}}
  {{- if .Values.foundation.externalDNS.managed -}}
    {{- if or (empty $identity.credentialSecretRef) (empty $identity.providerConfigRef) (empty $identity.egressConfigRef) -}}{{ fail "managed external-dns requires exact credential, provider-config, and egress identities" }}{{- end -}}
    {{- if or (empty .Values.foundation.networkPolicy.kubeAPIServerCIDRs) (empty .Values.foundation.networkPolicy.providerEgressCIDRs) -}}{{ fail "managed external-dns requires explicit API-server and provider egress CIDRs" }}{{- end -}}
    {{- range .Values.foundation.networkPolicy.kubeAPIServerCIDRs -}}
      {{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "external-dns API-server CIDRs cannot be an all-address range" }}{{- end -}}
    {{- end -}}
    {{- if or (ne .Values.externaldns.image.repository "registry.k8s.io/external-dns/external-dns") (ne .Values.externaldns.image.tag "v0.21.0") -}}{{ fail "external-dns image is locked to v0.21.0" }}{{- end -}}
    {{- if or (ne .Values.externaldns.nameOverride "kuberploy-external-dns") (not (empty .Values.externaldns.fullnameOverride)) (not (empty .Values.externaldns.namespaceOverride)) (not (empty .Values.externaldns.commonLabels)) (not (empty .Values.externaldns.podLabels)) -}}{{ fail "external-dns namespace and NetworkPolicy identity labels are locked" }}{{- end -}}
    {{- if or (not .Values.externaldns.rbac.create) .Values.externaldns.namespaced -}}{{ fail "managed external-dns requires cluster-scoped read RBAC" }}{{- end -}}
    {{- if or (not .Values.externaldns.serviceAccount.create) (not (empty .Values.externaldns.serviceAccount.name)) (not (empty .Values.externaldns.serviceAccount.annotations)) (not .Values.externaldns.serviceAccount.automountServiceAccountToken) (not .Values.externaldns.automountServiceAccountToken) (not (empty .Values.externaldns.rbac.additionalPermissions)) -}}{{ fail "managed external-dns requires its exact dedicated API ServiceAccount and RBAC without ambient workload identity" }}{{- end -}}
    {{- $credentialRefs := dict -}}
    {{- range $entry := .Values.externaldns.env -}}
      {{- if or (hasKey $entry "value") (not (hasKey $entry "valueFrom")) (not (hasKey $entry.valueFrom "secretKeyRef")) -}}{{ fail "externalDns.env accepts only valueFrom.secretKeyRef entries" }}{{- end -}}
      {{- $_ := set $credentialRefs $entry.valueFrom.secretKeyRef.name true -}}
    {{- end -}}
    {{- if or (ne (len $credentialRefs) 1) (not (hasKey $credentialRefs $identity.credentialSecretRef)) -}}{{ fail "managed external-dns credential environment must use only the exact runtime credential Secret" }}{{- end -}}
    {{- if or .Values.externaldns.secretConfiguration.enabled (not (empty .Values.externaldns.secretConfiguration.data)) -}}{{ fail "external-dns cannot render provider credentials" }}{{- end -}}
    {{- if or (not (empty .Values.externaldns.extraArgs)) (not (empty .Values.externaldns.extraContainers)) (not (empty .Values.externaldns.initContainers)) (not (empty .Values.externaldns.extraVolumes)) (not (empty .Values.externaldns.extraVolumeMounts)) (not (empty .Values.externaldns.deploymentAnnotations)) (not (empty .Values.externaldns.podAnnotations)) .Values.externaldns.shareProcessNamespace -}}{{ fail "managed external-dns does not permit process, pod, annotation, container, or volume injection" }}{{- end -}}
    {{- if or (not (empty .Values.externaldns.annotationPrefix)) .Values.externaldns.serviceMonitor.enabled -}}{{ fail "external-dns annotation namespace and monitoring ownership are locked" }}{{- end -}}
    {{- if or .Values.externaldns.securityContext.privileged .Values.externaldns.securityContext.allowPrivilegeEscalation (not .Values.externaldns.securityContext.readOnlyRootFilesystem) (not .Values.externaldns.securityContext.runAsNonRoot) (not (deepEqual .Values.externaldns.securityContext.capabilities.drop (list "ALL"))) -}}{{ fail "external-dns container security must remain restricted" }}{{- end -}}
    {{- if or (empty .Values.externaldns.resources.requests.cpu) (empty .Values.externaldns.resources.requests.memory) (empty .Values.externaldns.resources.limits.cpu) (empty .Values.externaldns.resources.limits.memory) -}}{{ fail "external-dns CPU and memory requests and limits are required" }}{{- end -}}
  {{- else -}}
    {{- if or $identity.credentialSecretRef $identity.providerConfigRef $identity.egressConfigRef -}}{{ fail "adopted external-dns rejects managed runtime reference identities" }}{{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}
