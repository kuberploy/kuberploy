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
    {{- $relative := trimPrefix "examples/installer/" $valueFile -}}
    {{- if or (not (regexMatch "^examples/installer/[a-z0-9][a-z0-9._/-]{0,180}\\.ya?ml$" $valueFile)) (contains ".." $relative) (contains "//" $relative) -}}
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

{{- $github := .Values.integrations.github -}}
{{- if $github.enabled -}}
  {{- if or (not .Values.components.controlPlane.enabled) (not .Values.components.builder.enabled) -}}{{ fail "GitHub integration requires enabled controlPlane and builder components" }}{{- end -}}
  {{- if or (not $public.enabled) (not $public.tls.enabled) -}}{{ fail "GitHub integration requires the public HTTPS endpoint" }}{{- end -}}
  {{- if or (le (int64 $github.appID) 0) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9_-]{5,127}$" $github.clientID)) (not (regexMatch "^[a-z0-9](?:[a-z0-9-]{0,98}[a-z0-9])?$" $github.appSlug)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $github.secretName)) (not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $github.clusterID)) (not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $github.platformBindingID)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $github.argoNamespace)) (not (regexMatch "^v1[.](?:2[5-9]|[3-9][0-9])$" $github.psaVersion)) (not (regexMatch "^sha256:[0-9a-f]{64}$" $github.runtimeChartDigest)) (not (regexMatch "^[^\\s@]+:v0\\.32\\.2$" $github.buildKitImage)) -}}{{ fail "GitHub/GitOps integration identities, runtime lock, and BuildKit image are invalid" }}{{- end -}}
  {{- $rootBootstrap := .Values.argoCD.argoFoundation.bootstrap -}}
  {{- if or (not $rootBootstrap.enabled) (ne $rootBootstrap.clusterID $github.clusterID) (ne $rootBootstrap.bindingID $github.platformBindingID) -}}{{ fail "GitHub desired-state integration requires the exact platform root bootstrap binding" }}{{- end -}}
  {{- if or (empty $github.controlPlaneEgressCIDRs) (empty $github.sourceEgressCIDRs) (empty $github.registryEgressCIDRs) -}}{{ fail "GitHub integration requires exact control-plane, source, and registry egress CIDRs" }}{{- end -}}
  {{- range concat $github.controlPlaneEgressCIDRs $github.sourceEgressCIDRs $github.registryEgressCIDRs -}}
    {{- if or (has . (list "0.0.0.0/0" "::/0")) (not (regexMatch "(?:/32|/128)$" .)) -}}{{ fail "GitHub integration egress accepts only exact /32 or /128 hosts" }}{{- end -}}
  {{- end -}}
{{- else if or (ne (int64 $github.appID) 0) (ne $github.clientID "") (ne $github.appSlug "") (ne $github.secretName "") (ne $github.clusterID "") (ne $github.platformBindingID "") (ne $github.argoNamespace "") (ne $github.psaVersion "") (ne $github.runtimeChartDigest "") (ne $github.buildKitImage "") (not (empty $github.controlPlaneEgressCIDRs)) (not (empty $github.sourceEgressCIDRs)) (not (empty $github.registryEgressCIDRs)) $github.allowSharedNodeEndpoint -}}
  {{- fail "disabled GitHub integration rejects dormant configuration" -}}
{{- end -}}

{{- $registry := .Values.integrations.registry -}}
{{- $runtimePull := $registry.runtimePull -}}
{{- if $registry.enabled -}}
  {{- if or (not .Values.components.registry.enabled) (ne .Values.components.registry.mode "managed") -}}{{ fail "managed registry integration requires the managed registry component" }}{{- end -}}
  {{- if not .Values.components.controlPlane.enabled -}}{{ fail "managed registry integration requires the control plane" }}{{- end -}}
  {{- if or (not .Values.components.edge.enabled) (not .Values.publicEndpoint.tls.enabled) -}}{{ fail "managed registry integration requires managed edge and public TLS" }}{{- end -}}
  {{- if or (not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $registry.targetID)) (empty $registry.targetName) (not (regexMatch "^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$" $registry.repositoryPrefix)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.lifecycleCredentialSecretName)) -}}{{ fail "managed registry target identity is invalid" }}{{- end -}}
  {{- $registryCredentialRefs := list $registry.lifecycleCredentialRef $registry.pullCredentialRef $registry.pushCredentialRef $registry.cacheCredentialRef -}}
  {{- if or (ne (len (uniq $registryCredentialRefs)) 4) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._:/+\\-]{0,255}$" $registry.lifecycleCredentialRef)) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._:/+\\-]{0,255}$" $registry.pullCredentialRef)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.pushCredentialRef)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.cacheCredentialRef)) -}}{{ fail "managed registry lifecycle, pull, push, and cache credentials must be distinct exact identities" }}{{- end -}}
  {{- if or (empty $registry.controlPlaneEgressCIDRs) (ne (len $registry.controlPlaneEgressCIDRs) (len (uniq $registry.controlPlaneEgressCIDRs))) -}}{{ fail "managed registry requires unique exact control-plane egress hosts" }}{{- end -}}
  {{- range $registry.controlPlaneEgressCIDRs -}}
    {{- if or (has . (list "0.0.0.0/0" "::/0")) (not (regexMatch "(?:/32|/128)$" .)) -}}{{ fail "managed registry control-plane egress accepts only exact /32 or /128 hosts" }}{{- end -}}
  {{- end -}}
  {{- if or (not (has $registry.exposureMode (list "ingress" "loadBalancer"))) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.authSecretName)) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" $registry.secretRevision)) (not (regexMatch "^(?:(?:[0-9]{1,3}\\.){3}[0-9]{1,3}|[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?(?:\\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)+)$" $registry.endpoint)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.tlsSecretName)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.clusterIssuerName)) -}}{{ fail "managed registry integration requires exact auth, endpoint, and TLS identities" }}{{- end -}}
  {{- if ne $registry.clusterIssuerName .Values.publicEndpoint.tls.clusterIssuerName -}}{{ fail "managed registry must use the installer-owned public ClusterIssuer" }}{{- end -}}
  {{- if eq $registry.exposureMode "loadBalancer" -}}
    {{- if eq (len $registry.loadBalancer.sourceRanges) 0 -}}{{ fail "managed registry LoadBalancer requires explicit source ranges" }}{{- end -}}
    {{- if or (hasKey $registry.loadBalancer.annotations "external-dns.alpha.kubernetes.io/cloudflare-proxied") (hasKey $registry.loadBalancer.annotations "external-dns.kubernetes.io/cloudflare-proxied") -}}{{ fail "managed registry Cloudflare proxy mode is locked to DNS-only" }}{{- end -}}
  {{- else if or (gt (len $registry.loadBalancer.annotations) 0) (ne $registry.loadBalancer.class "") (ne $registry.loadBalancer.ip "") (gt (len $registry.loadBalancer.sourceRanges) 0) -}}
    {{- fail "registry ingress mode rejects dormant LoadBalancer configuration" -}}
  {{- end -}}
  {{- if $runtimePull.enabled -}}
    {{- if or (not .Values.components.controlPlane.enabled) (not .Values.integrations.github.enabled) -}}{{ fail "managed registry runtime pull requires the GitOps control plane" }}{{- end -}}
    {{- if or (not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $runtimePull.targetID)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $runtimePull.profileName)) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._:/+\\-]{0,255}$" $runtimePull.credentialRef)) (le (int64 $runtimePull.revision) 0) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $runtimePull.sourceSecretName)) (not (regexMatch "^[A-Za-z0-9._-]{1,253}$" $runtimePull.sourceSecretKey)) (empty $runtimePull.namespaces) -}}{{ fail "managed registry runtime pull identities are invalid" }}{{- end -}}
    {{- if or (ne (len $runtimePull.namespaces) (len (uniq $runtimePull.namespaces))) (ne (join "," $runtimePull.namespaces) (join "," (sortAlpha $runtimePull.namespaces))) -}}{{ fail "managed registry runtime pull namespaces must be sorted and unique" }}{{- end -}}
    {{- if or (ne $runtimePull.targetID $registry.targetID) (ne $runtimePull.credentialRef $registry.pullCredentialRef) -}}{{ fail "managed registry runtime pull must use the operator-owned target and pull credential" }}{{- end -}}
    {{- range $runtimePull.namespaces -}}
      {{- if not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" .) -}}{{ fail "managed registry runtime pull namespace is invalid" }}{{- end -}}
    {{- end -}}
  {{- else if or (ne $runtimePull.targetID "") (ne $runtimePull.profileName "") (ne $runtimePull.credentialRef "") (ne (int64 $runtimePull.revision) 0) (ne $runtimePull.sourceSecretName "") (ne $runtimePull.sourceSecretKey "") (not (empty $runtimePull.namespaces)) -}}
    {{- fail "disabled managed registry runtime pull rejects dormant configuration" -}}
  {{- end -}}
{{- else if or (ne $registry.targetID "") (ne $registry.targetName "") (ne $registry.repositoryPrefix "") (ne $registry.lifecycleCredentialRef "") (ne $registry.lifecycleCredentialSecretName "") (ne $registry.pullCredentialRef "") (ne $registry.pushCredentialRef "") (ne $registry.cacheCredentialRef "") (not (empty $registry.controlPlaneEgressCIDRs)) (ne $registry.authSecretName "") (ne $registry.secretRevision "") (ne $registry.exposureMode "internal") (ne $registry.endpoint "") (ne $registry.tlsSecretName "") (ne $registry.clusterIssuerName "") (gt (len $registry.loadBalancer.annotations) 0) (ne $registry.loadBalancer.class "") (ne $registry.loadBalancer.ip "") (gt (len $registry.loadBalancer.sourceRanges) 0) $runtimePull.enabled (ne $runtimePull.targetID "") (ne $runtimePull.profileName "") (ne $runtimePull.credentialRef "") (ne (int64 $runtimePull.revision) 0) (ne $runtimePull.sourceSecretName "") (ne $runtimePull.sourceSecretKey "") (not (empty $runtimePull.namespaces)) -}}
  {{- fail "disabled managed registry integration rejects dormant configuration" -}}
{{- end -}}
{{- if and .Values.components.registry.enabled (not $registry.enabled) -}}
  {{- fail "managed registry component requires integrations.registry" -}}
{{- end -}}

{{- if and $anyChild (not $argo.enabled) -}}{{ fail "enabled child Applications require the explicit Argo CD bootstrap/adoption boundary" }}{{- end -}}
{{- if $anyChild -}}
  {{- if ne .Values.source.chartRepository "ghcr.io/kuberploy/charts" -}}{{ fail "source.chartRepository is locked to the public Kuberploy OCI chart repository" }}{{- end -}}
  {{- if not (regexMatch "^https://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\\.git$" .Values.source.valuesRepository) -}}{{ fail "source.valuesRepository must be a canonical HTTPS GitHub repository URL without credentials" }}{{- end -}}
  {{- if not (regexMatch "^v[0-9]+\\.[0-9]+\\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$" .Values.source.valuesRevision) -}}{{ fail "source.valuesRevision must be an explicit v-prefixed semantic release tag" }}{{- end -}}
{{- else if or (ne .Values.source.valuesRepository "") (ne .Values.source.valuesRevision "") (ne .Values.source.chartRepository "ghcr.io/kuberploy/charts") -}}
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
