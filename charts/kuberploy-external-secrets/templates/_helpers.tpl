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
{{- if not $foundation.networkPolicy.enabled -}}{{ fail "External Secrets NetworkPolicy cannot be disabled" }}{{- end -}}
{{- if ne $foundation.networkPolicy.monitoringNamespace "kuberploy-monitoring" -}}{{ fail "External Secrets monitoring namespace is locked" }}{{- end -}}
{{- if $foundation.operator.managed -}}
  {{- if empty $foundation.networkPolicy.kubeAPIServerCIDRs -}}{{ fail "managed External Secrets requires explicit Kubernetes API CIDRs" }}{{- end -}}
  {{- if empty $foundation.networkPolicy.providerHTTPSCIDRs -}}{{ fail "managed External Secrets requires explicit provider HTTPS CIDRs" }}{{- end -}}
  {{- range $foundation.networkPolicy.kubeAPIServerCIDRs -}}{{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "Kubernetes API CIDRs cannot be all-address ranges" }}{{- end -}}{{- end -}}
  {{- if or (ne $eso.nameOverride "external-secrets") (ne $eso.fullnameOverride "kuberploy-external-secrets") (not (empty $eso.namespaceOverride)) -}}{{ fail "External Secrets names and namespace identity are locked" }}{{- end -}}
  {{- if or (ne $eso.image.repository "ghcr.io/external-secrets/external-secrets") (ne $eso.image.tag "v2.8.0@sha256:24c0dd3699e0988520afd2218612758cd97d1f702757b5b4fcf89adaa33ef679") (ne $eso.webhook.image.repository $eso.image.repository) (ne $eso.webhook.image.tag $eso.image.tag) (ne $eso.certController.image.repository $eso.image.repository) (ne $eso.certController.image.tag $eso.image.tag) -}}{{ fail "External Secrets images are locked by multi-platform digest" }}{{- end -}}
  {{- if or (not $eso.installCRDs) $eso.crds.createClusterExternalSecret $eso.crds.createClusterSecretStore (not $eso.crds.createSecretStore) $eso.crds.createClusterGenerator $eso.crds.createClusterPushSecret $eso.crds.createPushSecret $eso.crds.conversion.enabled $eso.crds.unsafeServeV1Beta1 -}}{{ fail "External Secrets CRD surface is locked to ExternalSecret and namespaced SecretStore" }}{{- end -}}
  {{- if or (ne (int $eso.replicaCount) 2) (not $eso.leaderElect) (ne $eso.controllerClass "kuberploy") (not $eso.createOperator) (ne (int $eso.concurrent) 4) $eso.enableHTTP2 -}}{{ fail "External Secrets controller concurrency and identity are locked" }}{{- end -}}
  {{- if or $eso.scopedRBAC $eso.openshiftFinalizers $eso.systemAuthDelegator $eso.processClusterExternalSecret $eso.processClusterPushSecret $eso.processClusterStore (not $eso.processSecretStore) $eso.processClusterGenerator $eso.processPushSecret $eso.genericTargets.enabled (not (empty $eso.genericTargets.resources)) -}}{{ fail "External Secrets cluster-scoped and generic reconciliation is forbidden" }}{{- end -}}
  {{- if or (not $eso.rbac.create) $eso.rbac.serviceAccountTokenCreate $eso.rbac.servicebindings.create $eso.rbac.aggregateToView $eso.rbac.aggregateToEdit $eso.rbac.aggregateToAdmin -}}{{ fail "External Secrets RBAC expansion is forbidden" }}{{- end -}}
  {{- if or (not $eso.serviceAccount.create) (not $eso.serviceAccount.automount) (ne $eso.serviceAccount.name "kuberploy-external-secrets") (not $eso.webhook.serviceAccount.create) (not $eso.webhook.serviceAccount.automount) (ne $eso.webhook.serviceAccount.name "kuberploy-external-secrets-webhook") (not $eso.certController.serviceAccount.create) (not $eso.certController.serviceAccount.automount) (ne $eso.certController.serviceAccount.name "kuberploy-external-secrets-cert-controller") -}}{{ fail "External Secrets ServiceAccount identities are locked" }}{{- end -}}
  {{- if or (index $eso "bitwarden-sdk-server").enabled (not (empty $eso.extraObjects)) (not (empty $eso.podSpecExtra)) (not (empty $eso.global.imagePullSecrets)) (not (empty $eso.global.repository)) (not (empty $eso.global.podLabels)) (not (empty $eso.global.podAnnotations)) (not (empty $eso.global.hostAliases)) -}}{{ fail "External Secrets dependency and global injection is forbidden" }}{{- end -}}
  {{- if or (not $eso.webhook.create) (ne (int $eso.webhook.replicaCount) 2) (ne $eso.webhook.failurePolicy "Fail") $eso.webhook.hostNetwork (ne (int $eso.webhook.port) 10250) $eso.webhook.certManager.enabled (not $eso.certController.create) (ne (int $eso.certController.replicaCount) 2) $eso.certController.hostNetwork -}}{{ fail "External Secrets webhook and certificate controller profile is locked" }}{{- end -}}
  {{- if or (not $eso.webhook.service.enabled) (ne $eso.webhook.service.type "ClusterIP") (not $eso.podDisruptionBudget.enabled) (not $eso.webhook.podDisruptionBudget.enabled) (not $eso.certController.podDisruptionBudget.enabled) $eso.networkPolicy.enabled $eso.webhook.networkPolicy.enabled $eso.certController.networkPolicy.enabled -}}{{ fail "External Secrets availability, service, and wrapper NetworkPolicy ownership are locked" }}{{- end -}}
  {{- if or (not $eso.livenessProbe.enabled) (not $eso.readinessProbe.enabled) (not $eso.webhook.livenessProbe.enabled) (not $eso.webhook.readinessProbe.enabled) (not $eso.certController.livenessProbe.enabled) (not $eso.certController.readinessProbe.enabled) -}}{{ fail "External Secrets health probes cannot be disabled" }}{{- end -}}
  {{- include "kuberploy-external-secrets.securityValid" (list $eso.securityContext "controller") -}}
  {{- include "kuberploy-external-secrets.securityValid" (list $eso.webhook.securityContext "webhook") -}}
  {{- include "kuberploy-external-secrets.securityValid" (list $eso.certController.securityContext "certController") -}}
  {{- include "kuberploy-external-secrets.resourcesValid" (list $eso.resources "controller") -}}
  {{- include "kuberploy-external-secrets.resourcesValid" (list $eso.webhook.resources "webhook") -}}
  {{- include "kuberploy-external-secrets.resourcesValid" (list $eso.certController.resources "certController") -}}
  {{- include "kuberploy-external-secrets.noInjection" (list $eso "controller") -}}
  {{- include "kuberploy-external-secrets.noInjection" (list $eso.webhook "webhook") -}}
  {{- include "kuberploy-external-secrets.noInjection" (list $eso.certController "certController") -}}
{{- end -}}
{{- end -}}
