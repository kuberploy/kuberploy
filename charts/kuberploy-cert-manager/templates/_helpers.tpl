{{- define "kuberploy-cert-manager.labels" -}}
app.kubernetes.io/name: kuberploy-cert-manager
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-cert-manager.dns01ProfileNames" -}}
{{- $names := list -}}
{{- range . -}}{{- $names = append $names .name -}}{{- end -}}
{{- join "," (sortAlpha $names) -}}
{{- end -}}

{{- define "kuberploy-cert-manager.validateContainer" -}}
{{- $name := index . 0 -}}
{{- $security := index . 1 -}}
{{- if or $security.allowPrivilegeEscalation (not $security.readOnlyRootFilesystem) (not (deepEqual $security.capabilities.drop (list "ALL"))) -}}
{{- fail (printf "%s container security must remain restricted" $name) -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-cert-manager.validateResources" -}}
{{- $name := index . 0 -}}
{{- $resources := index . 1 -}}
{{- if or (empty $resources.requests.cpu) (empty $resources.requests.memory) (empty $resources.limits.cpu) (empty $resources.limits.memory) -}}
{{- fail (printf "%s CPU and memory requests and limits are required" $name) -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-cert-manager.validateIssuer" -}}
{{- $environment := index . 0 -}}
{{- $issuer := index . 1 -}}
{{- $expectedServer := index . 2 -}}
{{- if not (kindIs "map" $issuer) -}}{{ fail (printf "%s issuer configuration must be an object" $environment) }}{{- end -}}
{{- if ne (keys $issuer | sortAlpha | join ",") "dns01Profiles,email,enabled,name,privateKeySecretName,server" -}}
{{- fail (printf "%s issuer configuration contains unknown or missing fields" $environment) -}}
{{- end -}}
{{- if ne $issuer.server $expectedServer -}}{{ fail (printf "%s issuer must use the exact Let's Encrypt directory" $environment) }}{{- end -}}
{{- if not (kindIs "slice" $issuer.dns01Profiles) -}}{{ fail (printf "%s dns01Profiles must be an array" $environment) }}{{- end -}}
{{- if and (not $issuer.enabled) (gt (len $issuer.dns01Profiles) 0) -}}{{ fail (printf "disabled %s issuer cannot retain DNS-01 profiles" $environment) }}{{- end -}}
{{- $names := dict -}}
{{- $zones := dict -}}
{{- range $index, $profile := $issuer.dns01Profiles -}}
  {{- if not (kindIs "map" $profile) -}}{{ fail (printf "%s DNS-01 profile %d must be an object" $environment $index) }}{{- end -}}
  {{- if ne (keys $profile | sortAlpha | join ",") "cloudflare,dnsZones,name,provider" -}}{{ fail (printf "%s DNS-01 profile %d contains unknown or missing fields" $environment $index) }}{{- end -}}
  {{- if or (ne $profile.provider "cloudflare") (not (regexMatch "^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$" $profile.name)) -}}{{ fail (printf "%s DNS-01 profile %d must be a named Cloudflare profile" $environment $index) }}{{- end -}}
  {{- if hasKey $names $profile.name -}}{{ fail (printf "%s DNS-01 profile names must be unique" $environment) }}{{- end -}}
  {{- $_ := set $names $profile.name true -}}
  {{- if or (not (kindIs "slice" $profile.dnsZones)) (eq (len $profile.dnsZones) 0) (gt (len $profile.dnsZones) 32) -}}{{ fail (printf "%s DNS-01 profile %s requires 1-32 zones" $environment $profile.name) }}{{- end -}}
  {{- range $zone := $profile.dnsZones -}}
    {{- if or (not (kindIs "string" $zone)) (not (regexMatch "^(?:[a-z0-9](?:[-a-z0-9]*[a-z0-9])?\\.)*[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$" $zone)) -}}{{ fail (printf "%s DNS-01 profile %s contains an invalid zone" $environment $profile.name) }}{{- end -}}
    {{- if hasKey $zones $zone -}}{{ fail (printf "%s DNS-01 zones must not overlap between profiles" $environment) }}{{- end -}}
    {{- $_ := set $zones $zone true -}}
  {{- end -}}
  {{- if or (not (kindIs "map" $profile.cloudflare)) (ne (keys $profile.cloudflare | sortAlpha | join ",") "apiTokenSecretRef") -}}{{ fail (printf "%s DNS-01 profile %s requires only cloudflare.apiTokenSecretRef" $environment $profile.name) }}{{- end -}}
  {{- $ref := $profile.cloudflare.apiTokenSecretRef -}}
  {{- if or (not (kindIs "map" $ref)) (ne (keys $ref | sortAlpha | join ",") "key,name") -}}{{ fail (printf "%s DNS-01 profile %s requires an exact Secret key reference" $environment $profile.name) }}{{- end -}}
  {{- if or (not (regexMatch "^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$" $ref.name)) (not (regexMatch "^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$" $ref.key)) -}}{{ fail (printf "%s DNS-01 profile %s has an invalid Secret key reference" $environment $profile.name) }}{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-cert-manager.validate" -}}
{{- include "kuberploy-cert-manager.validateIssuer" (list "production" .Values.foundation.issuers.production "https://acme-v02.api.letsencrypt.org/directory") -}}
{{- include "kuberploy-cert-manager.validateIssuer" (list "staging" .Values.foundation.issuers.staging "https://acme-staging-v02.api.letsencrypt.org/directory") -}}
{{- if not .Values.foundation.enabled -}}
  {{- if or .Values.foundation.certManager.managed .Values.foundation.certManager.adoptExisting .Values.foundation.issuers.production.enabled .Values.foundation.issuers.staging.enabled -}}
  {{- fail "disabled cert-manager foundation cannot manage, adopt, or create issuers" -}}
  {{- end -}}
{{- else -}}
  {{- if ne .Release.Namespace "cert-manager" -}}{{ fail "kuberploy-cert-manager must use the protected cert-manager namespace" }}{{- end -}}
  {{- if eq .Values.foundation.certManager.managed .Values.foundation.certManager.adoptExisting -}}{{ fail "enabled foundation requires exactly one managed or adopted cert-manager" }}{{- end -}}
  {{- if and .Values.foundation.certManager.adoptExisting (not .Values.foundation.certManager.crdsConfirmed) -}}{{ fail "cert-manager adoption requires foundation.certManager.crdsConfirmed=true" }}{{- end -}}
  {{- if not .Values.foundation.networkPolicy.enabled -}}{{ fail "foundation.networkPolicy.enabled must remain true" }}{{- end -}}
  {{- range $issuer := list .Values.foundation.issuers.production .Values.foundation.issuers.staging -}}
    {{- if and $issuer.enabled (empty $issuer.email) -}}{{ fail "each enabled Let's Encrypt issuer requires an account email" }}{{- end -}}
  {{- end -}}
  {{- if .Values.foundation.certManager.managed -}}
    {{- if empty .Values.foundation.networkPolicy.kubeAPIServerCIDRs -}}{{ fail "managed cert-manager requires explicit foundation.networkPolicy.kubeAPIServerCIDRs" }}{{- end -}}
    {{- range .Values.foundation.networkPolicy.kubeAPIServerCIDRs -}}
      {{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "cert-manager API-server CIDRs cannot be an all-address range" }}{{- end -}}
    {{- end -}}
    {{- if or .Values.certmanager.installCRDs (not .Values.certmanager.crds.enabled) (not .Values.certmanager.crds.keep) -}}{{ fail "managed cert-manager requires retained chart-managed CRDs" }}{{- end -}}
    {{- if or (ne .Values.certmanager.nameOverride "cert-manager") (not (empty .Values.certmanager.namespace)) (not (empty .Values.certmanager.clusterResourceNamespace)) (not (empty .Values.certmanager.global.commonLabels)) -}}{{ fail "cert-manager name, namespace, and policy identity are locked to this protected release" }}{{- end -}}
    {{- if or (not .Values.certmanager.global.rbac.create) .Values.certmanager.global.rbac.aggregateClusterRoles -}}{{ fail "cert-manager RBAC must remain dedicated and cannot aggregate into user-facing roles" }}{{- end -}}
    {{- if or (ne .Values.certmanager.image.repository "quay.io/jetstack/cert-manager-controller") (ne .Values.certmanager.image.tag "v1.21.1") -}}{{ fail "cert-manager controller image is locked to v1.21.1" }}{{- end -}}
    {{- if or (ne .Values.certmanager.webhook.image.repository "quay.io/jetstack/cert-manager-webhook") (ne .Values.certmanager.webhook.image.tag "v1.21.1") -}}{{ fail "cert-manager webhook image is locked to v1.21.1" }}{{- end -}}
    {{- if or (ne .Values.certmanager.cainjector.image.repository "quay.io/jetstack/cert-manager-cainjector") (ne .Values.certmanager.cainjector.image.tag "v1.21.1") -}}{{ fail "cert-manager cainjector image is locked to v1.21.1" }}{{- end -}}
    {{- if or (ne .Values.certmanager.acmesolver.image.repository "quay.io/jetstack/cert-manager-acmesolver") (ne .Values.certmanager.acmesolver.image.tag "v1.21.1") -}}{{ fail "cert-manager ACME solver image is locked to v1.21.1" }}{{- end -}}
    {{- if or (lt (int .Values.certmanager.replicaCount) 2) (lt (int .Values.certmanager.webhook.replicaCount) 2) (lt (int .Values.certmanager.cainjector.replicaCount) 2) -}}{{ fail "managed cert-manager components require two replicas" }}{{- end -}}
    {{- if or (not .Values.certmanager.podDisruptionBudget.enabled) (not .Values.certmanager.webhook.podDisruptionBudget.enabled) (not .Values.certmanager.cainjector.podDisruptionBudget.enabled) -}}{{ fail "managed cert-manager components require PDBs" }}{{- end -}}
    {{- if .Values.certmanager.startupapicheck.enabled -}}{{ fail "startup hook is disabled for deterministic GitOps reconciliation" }}{{- end -}}
    {{- if or (not .Values.certmanager.serviceAccount.create) (not .Values.certmanager.webhook.serviceAccount.create) (not .Values.certmanager.cainjector.serviceAccount.create) (not .Values.certmanager.serviceAccount.automountServiceAccountToken) (not .Values.certmanager.webhook.serviceAccount.automountServiceAccountToken) (not .Values.certmanager.cainjector.serviceAccount.automountServiceAccountToken) (not .Values.certmanager.automountServiceAccountToken) (not .Values.certmanager.webhook.automountServiceAccountToken) (not .Values.certmanager.cainjector.automountServiceAccountToken) (not (empty .Values.certmanager.serviceAccount.annotations)) (not (empty .Values.certmanager.webhook.serviceAccount.annotations)) (not (empty .Values.certmanager.cainjector.serviceAccount.annotations)) -}}{{ fail "cert-manager components require dedicated API ServiceAccounts without cloud identity" }}{{- end -}}
    {{- if or (not (empty .Values.certmanager.extraArgs)) (not (empty .Values.certmanager.extraContainers)) (not (empty .Values.certmanager.extraEnv)) (not (empty .Values.certmanager.volumes)) (not (empty .Values.certmanager.volumeMounts)) (not (empty .Values.certmanager.podLabels)) (not (empty .Values.certmanager.podAnnotations)) -}}{{ fail "cert-manager controller process, volume, and identity injection is forbidden" }}{{- end -}}
    {{- if or (not (empty .Values.certmanager.webhook.extraArgs)) (not (empty .Values.certmanager.webhook.extraEnv)) (not (empty .Values.certmanager.webhook.volumes)) (not (empty .Values.certmanager.webhook.volumeMounts)) (not (empty .Values.certmanager.webhook.podLabels)) (not (empty .Values.certmanager.webhook.podAnnotations)) .Values.certmanager.webhook.hostNetwork (ne .Values.certmanager.webhook.serviceType "ClusterIP") (not (empty .Values.certmanager.webhook.url)) -}}{{ fail "cert-manager webhook process, network, volume, and identity injection is forbidden" }}{{- end -}}
    {{- if or (not (empty .Values.certmanager.cainjector.extraArgs)) (not (empty .Values.certmanager.cainjector.extraEnv)) (not (empty .Values.certmanager.cainjector.volumes)) (not (empty .Values.certmanager.cainjector.volumeMounts)) (not (empty .Values.certmanager.cainjector.podLabels)) (not (empty .Values.certmanager.cainjector.podAnnotations)) -}}{{ fail "cert-manager cainjector process, volume, and identity injection is forbidden" }}{{- end -}}
    {{- if or .Values.certmanager.networkPolicy.enabled .Values.certmanager.webhook.networkPolicy.enabled .Values.certmanager.cainjector.networkPolicy.enabled -}}{{ fail "upstream cert-manager policies must remain disabled in favor of the bounded wrapper policies" }}{{- end -}}
    {{- include "kuberploy-cert-manager.validateContainer" (list "cert-manager" .Values.certmanager.containerSecurityContext) -}}
    {{- include "kuberploy-cert-manager.validateContainer" (list "cert-manager webhook" .Values.certmanager.webhook.containerSecurityContext) -}}
    {{- include "kuberploy-cert-manager.validateContainer" (list "cert-manager cainjector" .Values.certmanager.cainjector.containerSecurityContext) -}}
    {{- include "kuberploy-cert-manager.validateResources" (list "cert-manager" .Values.certmanager.resources) -}}
    {{- include "kuberploy-cert-manager.validateResources" (list "cert-manager webhook" .Values.certmanager.webhook.resources) -}}
    {{- include "kuberploy-cert-manager.validateResources" (list "cert-manager cainjector" .Values.certmanager.cainjector.resources) -}}
  {{- end -}}
{{- end -}}
{{- end -}}
