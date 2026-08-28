{{- define "kuberploy.fullname" -}}
{{- printf "%s" .Release.Name | trunc 52 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.componentName" -}}
{{- printf "%s-%s" (include "kuberploy.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.registryMaintenanceServiceAccount" -}}
{{- printf "%s-registry-maintenance" (include "kuberploy.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.runtimeSecretAdmissionPolicyName" -}}
{{- printf "%s-runtime-secrets-%s" (include "kuberploy.fullname" .) (.Release.Namespace | sha256sum | trunc 10) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.runtimeSecretAPIUsername" -}}
{{- printf "system:serviceaccount:%s:%s" .Release.Namespace (include "kuberploy.componentName" (dict "root" . "component" "api")) -}}
{{- end -}}

{{- define "kuberploy.runtimeRegistryPullAdmissionPolicyName" -}}
{{- printf "%s-registry-pulls-%s" (include "kuberploy.fullname" .root) (.namespace | sha256sum | trunc 10) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.runtimeRegistryPullManagedAdmissionPolicyName" -}}
{{- printf "%s-registry-pulls-managed" (include "kuberploy.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.runtimeRegistryPullWorkerUsername" -}}
{{- printf "system:serviceaccount:%s:%s" .Release.Namespace (include "kuberploy.componentName" (dict "root" . "component" "worker")) -}}
{{- end -}}

{{- define "kuberploy.argoRepositoryCredentialAdmissionPolicyName" -}}
{{- printf "%s-argo-repositories-%s" (include "kuberploy.fullname" .) (.Release.Namespace | sha256sum | trunc 10) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.argoRootRefreshAdmissionPolicyName" -}}
{{- printf "%s-argo-root-refresh-%s" (include "kuberploy.fullname" .) (.Release.Namespace | sha256sum | trunc 10) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.argoApplicationSetRefreshAdmissionPolicyName" -}}
{{- printf "%s-argo-appset-refresh-%s" (include "kuberploy.fullname" .) (.Release.Namespace | sha256sum | trunc 10) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.argoDesiredStateWorkerUsername" -}}
{{- printf "system:serviceaccount:%s:%s" .Release.Namespace (include "kuberploy.componentName" (dict "root" . "component" "worker")) -}}
{{- end -}}

{{- define "kuberploy.helmApplicationsAPIUsername" -}}
{{- printf "system:serviceaccount:%s:%s" .Release.Namespace (include "kuberploy.componentName" (dict "root" . "component" "api")) -}}
{{- end -}}

{{- define "kuberploy.helmApplicationsAdmissionPolicyName" -}}
{{- printf "%s-helm-applications-%s" (include "kuberploy.fullname" .) (.Release.Namespace | sha256sum | trunc 10) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kuberploy.runtimeRegistryPullSecretName" -}}
{{- $targetID := required "runtime registry pull target ID is required" .profile.targetId -}}
{{- $revision := int64 (required "runtime registry pull profile revision is required" .profile.revision) -}}
{{- $identity := printf "kuberploy-runtime-pull-v2%c%s%c%d" 0 $targetID 0 $revision -}}
{{- printf "kuberploy-pull-%s" (sha256sum $identity | trunc 24) -}}
{{- end -}}

{{- define "kuberploy.selectorLabels" -}}
app.kubernetes.io/name: kuberploy
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kuberploy.labels" -}}
{{ include "kuberploy.selectorLabels" . }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- with .Values.global.testRun }}
kuberploy.io/test-run: {{ . | quote }}
{{- end }}
{{- end -}}

{{- define "kuberploy.componentLabels" -}}
{{ include "kuberploy.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "kuberploy.image" -}}
{{- $reference := required (printf "components.%s.image.reference is required" .component) .config.image.reference -}}
{{- if contains ":latest" (lower $reference) -}}
  {{- fail (printf "components.%s.image.reference must never use latest" .component) -}}
{{- end -}}
{{- if not (or (contains "@sha256:" $reference) (regexMatch ":[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" $reference)) -}}
  {{- fail (printf "components.%s.image.reference must contain an exact tag or sha256 digest" .component) -}}
{{- end -}}
{{- if and .root.Values.global.requireImageDigest (not (regexMatch "@sha256:[a-f0-9]{64}$" $reference)) -}}
  {{- fail (printf "components.%s.image.reference must end in @sha256:<64hex> for published release packaging" .component) -}}
{{- end -}}
{{- $reference -}}
{{- end -}}

{{- define "kuberploy.configName" -}}
{{- $builder := deepCopy .Values.builder -}}
{{- $_ := set $builder.networkPolicy "sourceEgressCIDRs" (uniq (sortAlpha .Values.builder.networkPolicy.sourceEgressCIDRs)) -}}
{{- $_ = set $builder.networkPolicy "registryEgressCIDRs" (uniq (sortAlpha .Values.builder.networkPolicy.registryEgressCIDRs)) -}}
{{- $networkPolicy := dict "enabled" .Values.networkPolicy.enabled "externalEgressCIDRs" (uniq (sortAlpha .Values.networkPolicy.externalEgressCIDRs)) "kubeAPIServerCIDRs" (uniq (sortAlpha .Values.networkPolicy.kubeAPIServerCIDRs)) -}}
{{- $input := dict "renderVersion" "v4" "publicURL" .Values.config.publicURL "logLevel" .Values.config.logLevel "monitoring" .Values.config.monitoring "git" .Values.config.git "githubApp" .Values.config.githubApp "buildLogs" .Values.config.buildLogs "gitProjection" .Values.config.gitProjection "autoDeploy" .Values.config.autoDeploy "runtimeSecrets" .Values.config.runtimeSecrets "certificateObservation" .Values.config.certificateObservation "certificateIssuerObserver" .Values.config.certificateIssuerObserver "runtimeRegistryPulls" .Values.config.runtimeRegistryPulls "imageTagResolution" .Values.config.imageTagResolution "edgeRuntime" .Values.config.edgeRuntime "argoObservation" .Values.config.argoObservation "environmentFoundation" .Values.config.environmentFoundation "argoDesiredState" .Values.config.argoDesiredState "helmApplications" .Values.config.helmApplications "managedRegistry" .Values.config.managedRegistry "workerImage" .Values.components.worker.image.reference "builder" $builder "networkPolicy" $networkPolicy "ingressClass" .Values.ingress.className -}}
{{- printf "%s-config-%s" (include "kuberploy.fullname" .) (toJson $input | sha256sum | trunc 12) -}}
{{- end -}}

{{- define "kuberploy.validateNetworkPolicy" -}}
{{- if .Values.networkPolicy.enabled -}}
  {{- if ne .Values.networkPolicy.managedPostgreSQLNamespace "kuberploy-system" -}}
  {{- fail "networkPolicy.managedPostgreSQLNamespace is locked to kuberploy-system" -}}
  {{- end -}}
  {{- if ne .Values.networkPolicy.managedValkeyNamespace "kuberploy-system" -}}
  {{- fail "networkPolicy.managedValkeyNamespace is locked to kuberploy-system" -}}
  {{- end -}}
  {{- if and (eq .Values.config.valkey.mode "managed") (not (empty .Values.networkPolicy.externalValkeyEgressCIDRs)) -}}
  {{- fail "managed Valkey cannot also enable external Valkey egress" -}}
  {{- end -}}
  {{- if and (eq .Values.config.valkey.mode "external") (empty .Values.networkPolicy.externalValkeyEgressCIDRs) -}}
  {{- fail "external Valkey requires explicit networkPolicy.externalValkeyEgressCIDRs" -}}
  {{- end -}}
  {{- range .Values.networkPolicy.kubeAPIServerCIDRs -}}
    {{- if regexMatch `/0+$` . -}}
    {{- fail "networkPolicy.kubeAPIServerCIDRs cannot contain an all-address range" -}}
    {{- end -}}
  {{- end -}}
  {{- range $external := .Values.networkPolicy.externalEgressCIDRs -}}
    {{- if not (or (regexMatch `^(?:[0-9]{1,3}\.){3}[0-9]{1,3}/(?:[89]|[12][0-9]|3[0-2])$` $external) (regexMatch `^[0-9a-f:]+/(?:1[6-9]|[2-9][0-9]|1[01][0-9]|12[0-8])$` $external)) -}}
    {{- fail "networkPolicy.externalEgressCIDRs must contain only bounded IPv4 /8-/32 or IPv6 /16-/128 ranges" -}}
    {{- end -}}
    {{- range $api := $.Values.networkPolicy.kubeAPIServerCIDRs -}}
      {{- if eq $external $api -}}
      {{- fail "networkPolicy external and Kubernetes API CIDRs must be disjoint" -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
  {{- range .Values.networkPolicy.externalPostgreSQLEgressCIDRs -}}
    {{- if regexMatch `/0+$` . -}}
    {{- fail "networkPolicy.externalPostgreSQLEgressCIDRs cannot contain an all-address range" -}}
    {{- end -}}
  {{- end -}}
  {{- range .Values.networkPolicy.externalValkeyEgressCIDRs -}}
    {{- if regexMatch `/0+$` . -}}
    {{- fail "networkPolicy.externalValkeyEgressCIDRs cannot contain an all-address range" -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{- if .Values.builder.networkPolicy.enabled -}}
  {{- if gt (add (len .Values.builder.networkPolicy.sourceEgressCIDRs) (len .Values.builder.networkPolicy.registryEgressCIDRs)) 128 -}}
  {{- fail "builder source and registry egress lists may contain at most 128 entries in total" -}}
  {{- end -}}
  {{- range $cidr := .Values.builder.networkPolicy.sourceEgressCIDRs -}}
    {{- if not (or (regexMatch `^(?:[0-9]{1,3}\.){3}[0-9]{1,3}/(?:[89]|[12][0-9]|3[0-2])$` $cidr) (regexMatch `^[0-9a-f:]+/(?:1[6-9]|[2-9][0-9]|1[01][0-9]|12[0-8])$` $cidr)) -}}
    {{- fail "builder.networkPolicy.sourceEgressCIDRs must contain only bounded IPv4 /8-/32 or IPv6 /16-/128 ranges" -}}
    {{- end -}}
    {{- range $api := $.Values.networkPolicy.kubeAPIServerCIDRs -}}
      {{- if eq $cidr $api -}}
      {{- fail "builder egress and Kubernetes API CIDRs must be disjoint" -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
  {{- range $cidr := .Values.builder.networkPolicy.registryEgressCIDRs -}}
    {{- if not (regexMatch `(?:/32|/128)$` $cidr) -}}
    {{- fail "builder.networkPolicy.registryEgressCIDRs must contain only /32 or /128 hosts" -}}
    {{- end -}}
    {{- range $api := $.Values.networkPolicy.kubeAPIServerCIDRs -}}
      {{- if eq $cidr $api -}}
      {{- fail "builder egress and Kubernetes API CIDRs must be disjoint" -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy.edgeRBACName" -}}
{{- $identity := printf "%s/%s/%s/%s/%s" .root.Release.Namespace .root.Release.Name .kind .namespace .identity -}}
{{- printf "kuberploy-edge-%s-%s" .kind (sha256sum $identity | trunc 12) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
