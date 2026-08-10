{{- define "kuberploy-installer.labels" -}}
app.kubernetes.io/name: kuberploy-installer
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kuberploy
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
kuberploy.io/ownership-boundary: bootstrap-applications-only
{{- end -}}

{{- define "kuberploy-installer.validateComponent" -}}
{{- $name := index . 0 -}}
{{- $component := index . 1 -}}
{{- $adoptable := index . 2 -}}
{{- if and (not $component.enabled) (ne $component.mode "disabled") -}}
  {{- fail (printf "components.%s.mode must be disabled when the component is disabled" $name) -}}
{{- end -}}
{{- if and $component.enabled (eq $component.mode "disabled") -}}
  {{- fail (printf "components.%s requires an explicit managed or adopted mode" $name) -}}
{{- end -}}
{{- if and (eq $component.mode "adopted") (not $adoptable) -}}
  {{- fail (printf "components.%s does not support adopted mode" $name) -}}
{{- end -}}
{{- if and (eq $component.mode "adopted") (not $component.adoptionConfirmed) -}}
  {{- fail (printf "components.%s adoption requires an explicit completed compatibility attestation" $name) -}}
{{- end -}}
{{- if and (ne $component.mode "adopted") $component.adoptionConfirmed -}}
  {{- fail (printf "components.%s.adoptionConfirmed is valid only in adopted mode" $name) -}}
{{- end -}}
{{- if $component.enabled -}}
  {{- if not (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+(?:-rc\\.[0-9]+)?$" $component.expectedPackageVersion) -}}
    {{- fail (printf "components.%s.expectedPackageVersion must be an explicit semantic version" $name) -}}
  {{- end -}}
  {{- range $valueFile := $component.valueFiles -}}
    {{- $relative := trimPrefix "../../examples/installer/" $valueFile -}}
    {{- if or (not (regexMatch "^../../examples/installer/[a-z0-9][a-z0-9._/-]{0,180}\\.ya?ml$" $valueFile)) (contains ".." $relative) (contains "//" $relative) -}}
      {{- fail (printf "components.%s.valueFiles must stay below examples/installer in the same release tag" $name) -}}
    {{- end -}}
  {{- end -}}
{{- else if or (ne $component.expectedPackageVersion "") (not (empty $component.valueFiles)) -}}
  {{- fail (printf "components.%s rejects dormant version and value files while disabled" $name) -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-installer.validate" -}}
{{- $anyChild := false -}}
{{- range $name, $component := .Values.components -}}
  {{- if $component.enabled -}}{{- $anyChild = true -}}{{- end -}}
{{- end -}}
{{- $active := or $anyChild .Values.bootstrap.argoCD.enabled -}}
{{- if $active -}}
  {{- if ne .Release.Namespace "kuberploy-system" -}}{{ fail "kuberploy-installer must be installed in the shared protected kuberploy-system namespace" }}{{- end -}}
{{- end -}}

{{- $argo := .Values.bootstrap.argoCD -}}
{{- if and (not $argo.enabled) (ne $argo.mode "disabled") -}}{{ fail "bootstrap.argoCD.mode must be disabled when bootstrap is disabled" }}{{- end -}}
{{- if and $argo.enabled (not (has $argo.mode (list "managed" "adopted"))) -}}{{ fail "bootstrap.argoCD requires an explicit managed or adopted mode" }}{{- end -}}
{{- if and (eq $argo.mode "managed") (not .Values.bootstrap.valkey.enabled) -}}{{ fail "managed Argo bootstrap requires the installer-owned Valkey bootstrap dependency" }}{{- end -}}
{{- if and (ne $argo.mode "managed") .Values.bootstrap.valkey.enabled -}}{{ fail "the installer-owned Valkey bootstrap dependency is valid only for managed Argo" }}{{- end -}}
{{- if and (ne $argo.mode "managed") $argo.managedPrerequisitesConfirmed -}}{{ fail "bootstrap.argoCD.managedPrerequisitesConfirmed is valid only in managed mode" }}{{- end -}}
{{- if $argo.enabled -}}
  {{- if eq $argo.mode "managed" -}}
    {{- if or (not .Values.argoCD.argoFoundation.argoCD.managed) .Values.argoCD.argoFoundation.argoCD.adoptExisting -}}{{ fail "managed Argo bootstrap requires the direct wrapper's exact managed mode" }}{{- end -}}
  {{- else -}}
    {{- if or .Values.argoCD.argoFoundation.argoCD.managed (not .Values.argoCD.argoFoundation.argoCD.adoptExisting) -}}{{ fail "adopted Argo bootstrap requires the direct wrapper's exact adopted mode" }}{{- end -}}
  {{- end -}}
{{- end -}}

{{- include "kuberploy-installer.validateComponent" (list "controlPlane" .Values.components.controlPlane false) -}}
{{- include "kuberploy-installer.validateComponent" (list "postgresql" .Values.components.postgresql true) -}}
{{- include "kuberploy-installer.validateComponent" (list "valkey" .Values.components.valkey true) -}}
{{- include "kuberploy-installer.validateComponent" (list "edge" .Values.components.edge true) -}}
{{- include "kuberploy-installer.validateComponent" (list "certManager" .Values.components.certManager true) -}}
{{- include "kuberploy-installer.validateComponent" (list "externalDNS" .Values.components.externalDNS true) -}}
{{- include "kuberploy-installer.validateComponent" (list "externalSecrets" .Values.components.externalSecrets true) -}}
{{- include "kuberploy-installer.validateComponent" (list "sealedSecrets" .Values.components.sealedSecrets true) -}}
{{- include "kuberploy-installer.validateComponent" (list "monitoring" .Values.components.monitoring false) -}}
{{- include "kuberploy-installer.validateComponent" (list "builder" .Values.components.builder false) -}}
{{- include "kuberploy-installer.validateComponent" (list "registry" .Values.components.registry false) -}}

{{- $token := .Values.bootstrap.controlPlaneToken -}}
{{- if .Values.components.controlPlane.enabled -}}
  {{- if not (has $token.mode (list "generated" "precreated")) -}}{{ fail "enabled control plane requires bootstrap.controlPlaneToken mode generated or precreated" }}{{- end -}}
  {{- if and (eq $token.mode "generated") (empty $token.kubeAPIServerCIDRs) -}}{{ fail "generated control-plane token requires exact kubeAPIServerCIDRs" }}{{- end -}}
  {{- if and (eq $token.mode "precreated") (not (empty $token.kubeAPIServerCIDRs)) -}}{{ fail "precreated control-plane token rejects dormant kubeAPIServerCIDRs" }}{{- end -}}
{{- else -}}
  {{- if or (ne $token.mode "disabled") (not (empty $token.kubeAPIServerCIDRs)) -}}{{ fail "disabled control plane rejects bootstrap token configuration" }}{{- end -}}
{{- end -}}

{{- if and .Values.components.controlPlane.enabled (or (not .Values.components.postgresql.enabled) (not .Values.components.valkey.enabled)) -}}
  {{- fail "the control-plane Application requires explicit PostgreSQL and Valkey managed/adopted Applications" -}}
{{- end -}}

{{- $apiConsumers := or
  (and .Values.components.edge.enabled (eq .Values.components.edge.mode "managed"))
  (and .Values.components.certManager.enabled (eq .Values.components.certManager.mode "managed"))
  (and .Values.components.externalDNS.enabled (eq .Values.components.externalDNS.mode "managed"))
  (and .Values.components.externalSecrets.enabled (eq .Values.components.externalSecrets.mode "managed"))
  (and .Values.components.sealedSecrets.enabled (eq .Values.components.sealedSecrets.mode "managed"))
  .Values.components.monitoring.enabled
-}}
{{- if and $apiConsumers (empty .Values.cluster.kubeAPIServerCIDRs) -}}
  {{- fail "enabled cluster-integrated components require exact cluster.kubeAPIServerCIDRs" -}}
{{- end -}}
{{- range .Values.cluster.kubeAPIServerCIDRs -}}
  {{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "cluster.kubeAPIServerCIDRs cannot contain all-address ranges" }}{{- end -}}
{{- end -}}

{{- $public := .Values.publicEndpoint -}}
{{- if $public.enabled -}}
  {{- if or (not .Values.components.controlPlane.enabled) (not .Values.components.edge.enabled) -}}{{ fail "publicEndpoint requires enabled controlPlane and edge components" }}{{- end -}}
  {{- if empty $public.hostname -}}{{ fail "publicEndpoint requires one exact hostname" }}{{- end -}}
  {{- if $public.tls.enabled -}}
    {{- if or (empty $public.tls.secretName) (empty $public.tls.clusterIssuerName) (empty $public.tls.accountEmail) -}}{{ fail "publicEndpoint TLS requires exact Secret, ClusterIssuer, and account email values" }}{{- end -}}
    {{- if or (not .Values.components.certManager.enabled) (ne .Values.components.certManager.mode "managed") -}}{{ fail "publicEndpoint managed TLS requires the managed certManager component" }}{{- end -}}
  {{- else if or (not (empty $public.tls.secretName)) (not (empty $public.tls.clusterIssuerName)) (not (empty $public.tls.accountEmail)) -}}
    {{- fail "disabled publicEndpoint TLS rejects dormant certificate configuration" }}
  {{- end -}}
{{- else if or (not (empty $public.hostname)) $public.tls.enabled (not (empty $public.tls.secretName)) (not (empty $public.tls.clusterIssuerName)) (not (empty $public.tls.accountEmail)) -}}
  {{- fail "disabled publicEndpoint rejects dormant hostname and TLS configuration" -}}
{{- end -}}

{{- if and $anyChild (not $argo.enabled) -}}{{ fail "enabled child Applications require the explicit Argo CD bootstrap/adoption boundary" }}{{- end -}}
{{- if $anyChild -}}
  {{- if not (regexMatch "^https://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\\.git$" .Values.source.repoURL) -}}{{ fail "source.repoURL must be a canonical HTTPS GitHub repository URL without credentials" }}{{- end -}}
  {{- if not (regexMatch "^v[0-9]+\\.[0-9]+\\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$" .Values.source.targetRevision) -}}{{ fail "source.targetRevision must be an explicit v-prefixed semantic release tag" }}{{- end -}}
  {{- if ne .Values.source.chartRoot "charts" -}}{{ fail "source.chartRoot is locked to charts" }}{{- end -}}
{{- else if or (ne .Values.source.repoURL "") (ne .Values.source.targetRevision "") (ne .Values.source.chartRoot "charts") -}}
  {{- fail "disabled installer rejects dormant source configuration" -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-installer.modeFence" -}}
{{- $name := index . 0 -}}
{{- $mode := index . 1 -}}
{{- if eq $name "controlPlane" -}}
global:
  # RC builds intentionally use a readable immutable release-candidate tag.
  # Stable release packaging flips this fence back to digest-only.
  requireImageDigest: false
builder:
  enabled: false
{{- else if eq $name "postgresql" -}}
postgresqlFoundation:
  managed: {{ eq $mode "managed" }}
  adoptExisting: {{ eq $mode "adopted" }}
{{- else if eq $name "valkey" -}}
valkeyFoundation:
  managed: {{ eq $mode "managed" }}
  adoptExisting: {{ eq $mode "adopted" }}
{{- else if eq $name "edge" -}}
edge:
  namespace:
    # kuberploy-system is created once by the installer/PostgreSQL foundation;
    # the edge Application must not compete for Namespace ownership.
    create: false
  traefik:
    managed: {{ eq $mode "managed" }}
    adoptExisting: {{ eq $mode "adopted" }}
    crdProviderConfirmed: {{ eq $mode "adopted" }}
{{- else if eq $name "certManager" -}}
foundation:
  enabled: true
  certManager:
    managed: {{ eq $mode "managed" }}
    adoptExisting: {{ eq $mode "adopted" }}
    crdsConfirmed: {{ eq $mode "adopted" }}
{{- else if eq $name "externalDNS" -}}
foundation:
  enabled: true
  externalDNS:
    managed: {{ eq $mode "managed" }}
    adoptExisting: {{ eq $mode "adopted" }}
    filtersConfirmed: {{ eq $mode "adopted" }}
{{- else if eq $name "externalSecrets" -}}
secretFoundation:
  operator:
    managed: {{ eq $mode "managed" }}
    adoptExisting: {{ eq $mode "adopted" }}
    capabilitiesConfirmed: {{ eq $mode "adopted" }}
{{- else if eq $name "sealedSecrets" -}}
secretFoundation:
  controller:
    managed: {{ eq $mode "managed" }}
    adoptExisting: {{ eq $mode "adopted" }}
    capabilitiesConfirmed: {{ eq $mode "adopted" }}
{{- else if eq $name "monitoring" -}}
monitoring:
  managed: true
{{- else if eq $name "builder" -}}
enabled: true
{{- else if eq $name "registry" -}}
enabled: true
{{- end -}}
{{- end -}}
