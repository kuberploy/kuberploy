{{- define "kuberploy-external-secrets.labels" -}}
app.kubernetes.io/name: kuberploy-external-secrets
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-external-secrets.securityValid" -}}
{{- $security := index . 0 -}}
{{- $name := index . 1 -}}
{{- if or (not $security.enabled) (not $security.runAsNonRoot) (not $security.readOnlyRootFilesystem) $security.allowPrivilegeEscalation (ne (int $security.runAsUser) 1000) (ne $security.seccompProfile.type "RuntimeDefault") (not (deepEqual $security.capabilities.drop (list "ALL"))) -}}{{ fail (printf "%s security context is locked" $name) }}{{- end -}}
{{- end -}}

{{- define "kuberploy-external-secrets.resourcesValid" -}}
{{- $resources := index . 0 -}}
{{- $name := index . 1 -}}
{{- if or (empty $resources.requests.cpu) (empty $resources.requests.memory) (empty $resources.limits.cpu) (empty $resources.limits.memory) -}}{{ fail (printf "%s CPU and memory requests and limits are required" $name) }}{{- end -}}
{{- end -}}

{{- define "kuberploy-external-secrets.noInjection" -}}
{{- $component := index . 0 -}}
{{- $name := index . 1 -}}
{{- range $key := list "extraEnv" "extraArgs" "extraVolumes" "extraVolumeMounts" "extraInitContainers" "extraContainers" "hostAliases" "podAnnotations" "podLabels" "deploymentAnnotations" "imagePullSecrets" -}}
  {{- if and (hasKey $component $key) (not (empty (index $component $key))) -}}{{ fail (printf "%s.%s injection is forbidden" $name $key) }}{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-external-secrets.validate" -}}
{{- $eso := .Values.externalSecrets -}}
{{- $foundation := .Values.secretFoundation -}}
{{- if ne .Release.Namespace "external-secrets" -}}{{ fail "kuberploy-external-secrets must use the protected external-secrets namespace" }}{{- end -}}
{{- if eq $foundation.operator.managed $foundation.operator.adoptExisting -}}{{ fail "exactly one of managed External Secrets or adopted External Secrets must be selected" }}{{- end -}}
{{- if and $foundation.operator.adoptExisting (not $foundation.operator.capabilitiesConfirmed) -}}{{ fail "External Secrets adoption requires a completed compatibility and capability check" }}{{- end -}}
{{- if ne $foundation.networkPolicy.monitoringNamespace "kuberploy-monitoring" -}}{{ fail "External Secrets monitoring namespace is locked" }}{{- end -}}
{{- if $foundation.operator.managed -}}
  {{- range $foundation.networkPolicy.kubeAPIServerCIDRs -}}{{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "Kubernetes API CIDRs cannot be all-address ranges" }}{{- end -}}{{- end -}}
  {{- range $foundation.networkPolicy.providerHTTPSCIDRs -}}{{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "provider HTTPS public defaults must be selected by an empty optional CIDR list" }}{{- end -}}{{- end -}}
  {{- if or (ne $eso.nameOverride "external-secrets") (ne $eso.fullnameOverride "kuberploy-external-secrets") (not (empty $eso.namespaceOverride)) -}}{{ fail "External Secrets names and namespace identity are locked" }}{{- end -}}
  {{- if or (not $eso.installCRDs) $eso.crds.createClusterExternalSecret $eso.crds.createClusterSecretStore (not $eso.crds.createSecretStore) $eso.crds.createClusterGenerator $eso.crds.createClusterPushSecret $eso.crds.createPushSecret $eso.crds.conversion.enabled $eso.crds.unsafeServeV1Beta1 -}}{{ fail "External Secrets CRD surface is locked to ExternalSecret and namespaced SecretStore" }}{{- end -}}
  {{- if or (lt (int $eso.replicaCount) 1) (not $eso.leaderElect) (ne $eso.controllerClass "kuberploy") (not $eso.createOperator) (lt (int $eso.concurrent) 1) -}}{{ fail "External Secrets controller must remain enabled with leader election" }}{{- end -}}
  {{- if or $eso.scopedRBAC $eso.openshiftFinalizers $eso.systemAuthDelegator $eso.processClusterExternalSecret $eso.processClusterPushSecret $eso.processClusterStore (not $eso.processSecretStore) $eso.processClusterGenerator $eso.processPushSecret $eso.genericTargets.enabled (not (empty $eso.genericTargets.resources)) -}}{{ fail "External Secrets cluster-scoped and generic reconciliation is forbidden" }}{{- end -}}
  {{- if or (not $eso.rbac.create) $eso.rbac.serviceAccountTokenCreate $eso.rbac.servicebindings.create $eso.rbac.aggregateToView $eso.rbac.aggregateToEdit $eso.rbac.aggregateToAdmin -}}{{ fail "External Secrets RBAC expansion is forbidden" }}{{- end -}}
  {{- if or (not $eso.serviceAccount.create) (not $eso.serviceAccount.automount) (ne $eso.serviceAccount.name "kuberploy-external-secrets") (not $eso.webhook.serviceAccount.create) (not $eso.webhook.serviceAccount.automount) (ne $eso.webhook.serviceAccount.name "kuberploy-external-secrets-webhook") (not $eso.certController.serviceAccount.create) (not $eso.certController.serviceAccount.automount) (ne $eso.certController.serviceAccount.name "kuberploy-external-secrets-cert-controller") -}}{{ fail "External Secrets ServiceAccount identities are locked" }}{{- end -}}
  {{- if or (index $eso "bitwarden-sdk-server").enabled (not (empty $eso.extraObjects)) (not (empty $eso.podSpecExtra)) (not (empty $eso.global.imagePullSecrets)) (not (empty $eso.global.repository)) (not (empty $eso.global.podLabels)) (not (empty $eso.global.podAnnotations)) (not (empty $eso.global.hostAliases)) -}}{{ fail "External Secrets dependency and global injection is forbidden" }}{{- end -}}
  {{- if or (not $eso.webhook.create) (lt (int $eso.webhook.replicaCount) 1) (ne $eso.webhook.failurePolicy "Fail") $eso.webhook.hostNetwork (not $eso.certController.create) (lt (int $eso.certController.replicaCount) 1) $eso.certController.hostNetwork -}}{{ fail "External Secrets webhook and certificate controller must remain private, fail closed, and enabled" }}{{- end -}}
  {{- if or (not $eso.webhook.service.enabled) (ne $eso.webhook.service.type "ClusterIP") -}}{{ fail "External Secrets webhook Service must remain private" }}{{- end -}}
  {{- include "kuberploy-external-secrets.securityValid" (list $eso.securityContext "controller") -}}
  {{- include "kuberploy-external-secrets.securityValid" (list $eso.webhook.securityContext "webhook") -}}
  {{- include "kuberploy-external-secrets.securityValid" (list $eso.certController.securityContext "certController") -}}
  {{- include "kuberploy-external-secrets.noInjection" (list $eso "controller") -}}
  {{- include "kuberploy-external-secrets.noInjection" (list $eso.webhook "webhook") -}}
  {{- include "kuberploy-external-secrets.noInjection" (list $eso.certController "certController") -}}
{{- end -}}
{{- end -}}
