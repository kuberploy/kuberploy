{{- define "kuberploy-installer.labels" -}}
app.kubernetes.io/name: kuberploy-installer
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kuberploy
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
kuberploy.io/ownership-boundary: bootstrap-applications-only
{{- end -}}

{{- define "kuberploy-installer.applicationCatalog" -}}
- key: controlPlane
  application: kuberploy-control-plane
  chart: kuberploy
  release: kuberploy
  namespace: kuberploy-system
  wave: "20"
- key: postgresql
  application: kuberploy-postgresql
  chart: kuberploy-postgresql
  release: postgresql
  namespace: kuberploy-system
  wave: "0"
- key: edge
  application: kuberploy-edge
  chart: kuberploy-edge
  release: edge
  namespace: kuberploy-system
  wave: "0"
- key: certManager
  application: kuberploy-cert-manager
  chart: kuberploy-cert-manager
  release: cert
  namespace: cert-manager
  wave: "5"
- key: externalDNS
  application: kuberploy-external-dns
  chart: kuberploy-external-dns
  release: dns
  namespace: kuberploy-system
  wave: "10"
- key: externalSecrets
  application: kuberploy-external-secrets
  chart: kuberploy-external-secrets
  release: external-secrets
  namespace: external-secrets
  wave: "0"
- key: sealedSecrets
  application: kuberploy-sealed-secrets
  chart: kuberploy-sealed-secrets
  release: sealed-secrets
  namespace: sealed-secrets
  wave: "0"
- key: monitoring
  application: kuberploy-monitoring
  chart: kuberploy-monitoring
  release: monitoring
  namespace: kuberploy-monitoring
  wave: "-10"
- key: builder
  application: kuberploy-builder
  chart: kuberploy-builder
  release: builder
  namespace: kuberploy-system
  wave: "10"
- key: registry
  application: kuberploy-registry
  chart: kuberploy-registry
  release: registry
  namespace: kuberploy-system
  wave: "10"
{{- end -}}

{{- define "kuberploy-installer.validateComponent" -}}
{{- $name := index . 0 -}}
{{- $component := index . 1 -}}
{{- $adoptable := index . 2 -}}
{{- $chartVersion := index . 3 -}}
{{- if and $component.enabled (eq $component.mode "disabled") -}}
  {{- fail (printf "components.%s requires an explicit managed or adopted mode" $name) -}}
{{- end -}}
{{- if and $component.enabled (eq $component.mode "adopted") (not $adoptable) -}}
  {{- fail (printf "components.%s does not support adopted mode" $name) -}}
{{- end -}}
{{- if and $component.enabled (eq $component.mode "adopted") (not $component.adoptionConfirmed) -}}
  {{- fail (printf "components.%s adoption requires an explicit completed compatibility attestation" $name) -}}
{{- end -}}
{{- if and $component.enabled (ne $component.mode "adopted") $component.adoptionConfirmed -}}
  {{- fail (printf "components.%s.adoptionConfirmed is valid only in adopted mode" $name) -}}
{{- end -}}
{{- if $component.enabled -}}
  {{- if not (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+(?:-rc\\.[0-9]+)?$" $component.expectedPackageVersion) -}}
    {{- fail (printf "components.%s.expectedPackageVersion must be an explicit semantic version" $name) -}}
  {{- end -}}
  {{- if ne $component.expectedPackageVersion $chartVersion -}}
    {{- fail (printf "components.%s.expectedPackageVersion must match installer chart version %s; use the release values file with --reset-values instead of --reuse-values" $name $chartVersion) -}}
  {{- end -}}
  {{- range $valueFile := $component.valueFiles -}}
    {{- $relative := trimPrefix "examples/installer/" $valueFile -}}
    {{- if or (not (regexMatch "^examples/installer/[a-z0-9][a-z0-9._/-]{0,180}\\.ya?ml$" $valueFile)) (contains ".." $relative) (contains "//" $relative) -}}
      {{- fail (printf "components.%s.valueFiles must stay below examples/installer in the same release tag" $name) -}}
    {{- end -}}
  {{- end -}}
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
{{- if and $argo.enabled (not (has $argo.mode (list "managed" "adopted"))) -}}{{ fail "bootstrap.argoCD requires an explicit managed or adopted mode" }}{{- end -}}
{{- if and $argo.enabled (eq $argo.mode "managed") (not .Values.bootstrap.valkey.enabled) -}}{{ fail "managed Argo bootstrap requires the installer-owned Valkey bootstrap dependency" }}{{- end -}}
{{- if and $argo.enabled (ne $argo.mode "managed") .Values.bootstrap.valkey.enabled -}}{{ fail "the installer-owned Valkey bootstrap dependency is valid only for managed Argo" }}{{- end -}}
{{- if and (not $argo.enabled) .Values.bootstrap.valkey.enabled -}}{{ fail "the installer-owned Valkey bootstrap dependency requires enabled managed Argo" }}{{- end -}}
{{- if and $argo.enabled (eq $argo.mode "managed") (not $argo.managedPrerequisitesConfirmed) -}}{{ fail "managed Argo bootstrap requires completed prerequisite confirmation" }}{{- end -}}
{{- if $argo.enabled -}}
  {{- if eq $argo.mode "managed" -}}
    {{- if or (not .Values.argoCD.argoFoundation.argoCD.managed) .Values.argoCD.argoFoundation.argoCD.adoptExisting -}}{{ fail "managed Argo bootstrap requires the direct wrapper's exact managed mode" }}{{- end -}}
  {{- else -}}
    {{- if or .Values.argoCD.argoFoundation.argoCD.managed (not .Values.argoCD.argoFoundation.argoCD.adoptExisting) -}}{{ fail "adopted Argo bootstrap requires the direct wrapper's exact adopted mode" }}{{- end -}}
  {{- end -}}
{{- end -}}

{{- include "kuberploy-installer.validateComponent" (list "controlPlane" .Values.components.controlPlane false .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "postgresql" .Values.components.postgresql true .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "valkey" .Values.components.valkey true .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "edge" .Values.components.edge true .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "certManager" .Values.components.certManager true .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "externalDNS" .Values.components.externalDNS true .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "externalSecrets" .Values.components.externalSecrets true .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "sealedSecrets" .Values.components.sealedSecrets true .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "monitoring" .Values.components.monitoring false .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "builder" .Values.components.builder false .Chart.Version) -}}
{{- include "kuberploy-installer.validateComponent" (list "registry" .Values.components.registry false .Chart.Version) -}}

{{- $token := .Values.bootstrap.controlPlaneToken -}}
{{- if .Values.components.controlPlane.enabled -}}
  {{- if not (has $token.mode (list "generated" "precreated")) -}}{{ fail "enabled control plane requires bootstrap.controlPlaneToken mode generated or precreated" }}{{- end -}}
{{- end -}}

{{- if and .Values.components.controlPlane.enabled (or (not .Values.components.postgresql.enabled) (not .Values.components.valkey.enabled)) -}}
  {{- fail "the control-plane Application requires explicit PostgreSQL and Valkey managed/adopted Applications" -}}
{{- end -}}


{{- /* API-server CIDRs are optional hardening inputs. */ -}}
{{- $apiConsumers := or
  (and .Values.components.edge.enabled (eq .Values.components.edge.mode "managed"))
  (and .Values.components.certManager.enabled (eq .Values.components.certManager.mode "managed"))
  (and .Values.components.externalDNS.enabled (eq .Values.components.externalDNS.mode "managed"))
  (and .Values.components.externalSecrets.enabled (eq .Values.components.externalSecrets.mode "managed"))
  (and .Values.components.sealedSecrets.enabled (eq .Values.components.sealedSecrets.mode "managed"))
  .Values.components.monitoring.enabled
-}}
{{- range .Values.cluster.kubeAPIServerCIDRs -}}
  {{- if and $.Values.cluster.networkPolicyEnabled (has . (list "0.0.0.0/0" "::/0")) -}}{{ fail "cluster.kubeAPIServerCIDRs cannot contain all-address ranges" }}{{- end -}}
{{- end -}}

{{- $public := .Values.publicEndpoint -}}
{{- if $public.enabled -}}
  {{- if or (not .Values.components.controlPlane.enabled) (not .Values.components.edge.enabled) -}}{{ fail "publicEndpoint requires enabled controlPlane and edge components" }}{{- end -}}
  {{- if empty $public.hostname -}}{{ fail "publicEndpoint requires one exact hostname" }}{{- end -}}
  {{- if $public.tls.enabled -}}
    {{- if or (empty $public.tls.secretName) (empty $public.tls.clusterIssuerName) (empty $public.tls.accountEmail) -}}{{ fail "publicEndpoint TLS requires exact Secret, ClusterIssuer, and account email values" }}{{- end -}}
    {{- if or (not .Values.components.certManager.enabled) (ne .Values.components.certManager.mode "managed") -}}{{ fail "publicEndpoint managed TLS requires the managed certManager component" }}{{- end -}}
  {{- end -}}
{{- end -}}

{{- $github := .Values.integrations.github -}}
{{- if hasKey $github "runtimeChartDigest" -}}{{ fail "integrations.github.runtimeChartDigest was removed; the installer release owns the runtime chart lock" }}{{- end -}}
{{- if $github.enabled -}}
  {{- if or (not .Values.components.controlPlane.enabled) (not .Values.components.builder.enabled) -}}{{ fail "GitHub integration requires enabled controlPlane and builder components" }}{{- end -}}
  {{- if or (not $public.enabled) (not $public.tls.enabled) -}}{{ fail "GitHub integration requires the public HTTPS endpoint" }}{{- end -}}
  {{- if or (le (int64 $github.appID) 0) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9_-]{5,127}$" $github.clientID)) (not (regexMatch "^[a-z0-9](?:[a-z0-9-]{0,98}[a-z0-9])?$" $github.appSlug)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $github.secretName)) (not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $github.clusterID)) (not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $github.platformBindingID)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $github.argoNamespace)) (not (regexMatch "^v1[.](?:2[5-9]|[3-9][0-9])$" $github.psaVersion)) (not (regexMatch "^(?:[^[:space:]@]+:v0[.]32[.]2|[^[:space:]@]+@sha256:[0-9a-f]{64})$" $github.buildKitImage)) (not (regexMatch "^(?:[^[:space:]@]+:v?[0-9]+[.][0-9]+[.][0-9]+(?:[-.][A-Za-z0-9]+)*|[^[:space:]@]+@sha256:[0-9a-f]{64})$" $github.dindImage)) -}}{{ fail "GitHub/GitOps integration identities and builder images are invalid" }}{{- end -}}
  {{- $runtimeVersion := index .Chart.Annotations "kuberploy.io/runtime-chart-version" | default "" -}}
  {{- $runtimeDigest := index .Chart.Annotations "kuberploy.io/runtime-chart-digest" | default "" -}}
  {{- $runtimeLock := index .Chart.Annotations "kuberploy.io/runtime-chart-lock" | default "" -}}
  {{- $expectedRuntimeLock := printf "kuberploy-runtime-lock-v1|%s|%s" $runtimeVersion $runtimeDigest | sha256sum | printf "sha256:%s" -}}
  {{- if or (ne $runtimeVersion .Chart.Version) (not (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$" $runtimeVersion)) (not (regexMatch "^sha256:[0-9a-f]{64}$" $runtimeDigest)) (ne $runtimeLock $expectedRuntimeLock) -}}{{ fail "installer release runtime chart lock is absent, malformed, or inconsistent" }}{{- end -}}
  {{- $rootBootstrap := .Values.argoCD.argoFoundation.bootstrap -}}
  {{- if or (not $rootBootstrap.enabled) (ne $rootBootstrap.clusterID $github.clusterID) (ne $rootBootstrap.bindingID $github.platformBindingID) -}}{{ fail "GitHub desired-state integration requires the exact platform root bootstrap binding" }}{{- end -}}
  {{- if gt (add (len $github.sourceEgressCIDRs) (len $github.registryEgressCIDRs)) 128 -}}{{ fail "GitHub builder source and registry egress lists may contain at most 128 entries in total" }}{{- end -}}
  {{- range concat $github.controlPlaneEgressCIDRs $github.sourceEgressCIDRs -}}
    {{- if not (or (regexMatch `^(?:[0-9]{1,3}\.){3}[0-9]{1,3}/(?:[89]|[12][0-9]|3[0-2])$` .) (regexMatch `^[0-9a-f:]+/(?:1[6-9]|[2-9][0-9]|1[01][0-9]|12[0-8])$` .)) -}}{{ fail "GitHub control-plane and source egress accept only bounded IPv4 /8-/32 or IPv6 /16-/128 ranges" }}{{- end -}}
  {{- end -}}
  {{- range $github.registryEgressCIDRs -}}
    {{- if not (regexMatch "(?:/32|/128)$" .) -}}{{ fail "GitHub registry egress accepts only exact /32 or /128 hosts" }}{{- end -}}
  {{- end -}}
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
  {{- range $registry.controlPlaneEgressCIDRs -}}
    {{- if or (has . (list "0.0.0.0/0" "::/0")) (not (regexMatch "(?:/32|/128)$" .)) -}}{{ fail "managed registry control-plane egress accepts only exact /32 or /128 hosts" }}{{- end -}}
  {{- end -}}
  {{- if or (not (has $registry.exposureMode (list "ingress" "loadBalancer"))) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.authSecretName)) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" $registry.secretRevision)) (not (regexMatch "^(?:(?:[0-9]{1,3}\\.){3}[0-9]{1,3}|[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?(?:\\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)+)$" $registry.endpoint)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.tlsSecretName)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $registry.clusterIssuerName)) -}}{{ fail "managed registry integration requires exact auth, endpoint, and TLS identities" }}{{- end -}}
  {{- if ne $registry.clusterIssuerName .Values.publicEndpoint.tls.clusterIssuerName -}}{{ fail "managed registry must use the installer-owned public ClusterIssuer" }}{{- end -}}
  {{- if eq $registry.exposureMode "loadBalancer" -}}
    {{- if or (hasKey $registry.loadBalancer.annotations "external-dns.alpha.kubernetes.io/cloudflare-proxied") (hasKey $registry.loadBalancer.annotations "external-dns.kubernetes.io/cloudflare-proxied") -}}{{ fail "managed registry Cloudflare proxy policy is controlled only by cloudflareProxied" }}{{- end -}}
  {{- end -}}
  {{- if $runtimePull.enabled -}}
    {{- if or (not .Values.components.controlPlane.enabled) (not .Values.integrations.github.enabled) -}}{{ fail "managed registry runtime pull requires the GitOps control plane" }}{{- end -}}
    {{- if or (not (regexMatch "^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" $runtimePull.targetID)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $runtimePull.profileName)) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._:/+\\-]{0,255}$" $runtimePull.credentialRef)) (le (int64 $runtimePull.revision) 0) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $runtimePull.sourceSecretName)) (not (regexMatch "^[A-Za-z0-9._-]{1,253}$" $runtimePull.sourceSecretKey)) (empty $runtimePull.namespaces) -}}{{ fail "managed registry runtime pull identities are invalid" }}{{- end -}}
    {{- if or (ne $runtimePull.targetID $registry.targetID) (ne $runtimePull.credentialRef $registry.pullCredentialRef) -}}{{ fail "managed registry runtime pull must use the operator-owned target and pull credential" }}{{- end -}}
    {{- range $runtimePull.namespaces -}}
      {{- if not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" .) -}}{{ fail "managed registry runtime pull namespace is invalid" }}{{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- if and .Values.components.registry.enabled (not $registry.enabled) -}}
  {{- fail "managed registry component requires integrations.registry" -}}
{{- end -}}

{{- if and $anyChild (not $argo.enabled) -}}{{ fail "enabled child Applications require the explicit Argo CD bootstrap/adoption boundary" }}{{- end -}}
{{- if $anyChild -}}
  {{- if ne .Values.source.chartRepository "ghcr.io/kuberploy/charts" -}}{{ fail "source.chartRepository is locked to the public Kuberploy OCI chart repository" }}{{- end -}}
  {{- if not (regexMatch "^https://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\\.git$" .Values.source.valuesRepository) -}}{{ fail "source.valuesRepository must be a canonical HTTPS GitHub repository URL without credentials" }}{{- end -}}
  {{- if not (regexMatch "^v[0-9]+\\.[0-9]+\\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$" .Values.source.valuesRevision) -}}{{ fail "source.valuesRevision must be an explicit v-prefixed semantic release tag" }}{{- end -}}
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
