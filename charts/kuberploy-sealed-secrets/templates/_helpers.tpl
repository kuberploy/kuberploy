{{- define "kuberploy-sealed-secrets.labels" -}}
app.kubernetes.io/name: kuberploy-sealed-secrets
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-sealed-secrets.validate" -}}
{{- $sealed := .Values.sealedSecrets -}}
{{- $foundation := .Values.secretFoundation -}}
{{- if ne .Release.Namespace "sealed-secrets" -}}{{ fail "kuberploy-sealed-secrets must use the protected sealed-secrets namespace" }}{{- end -}}
{{- if eq $foundation.controller.managed $foundation.controller.adoptExisting -}}{{ fail "exactly one of managed Sealed Secrets or adopted Sealed Secrets must be selected" }}{{- end -}}
{{- if and $foundation.controller.adoptExisting (not $foundation.controller.capabilitiesConfirmed) -}}{{ fail "Sealed Secrets adoption requires a completed compatibility, strict-scope, and key-recovery check" }}{{- end -}}
{{- if or (not $foundation.networkPolicy.enabled) (ne $foundation.networkPolicy.controlPlaneNamespace "kuberploy-system") (ne $foundation.networkPolicy.monitoringNamespace "kuberploy-monitoring") -}}{{ fail "Sealed Secrets namespace and NetworkPolicy identities are locked" }}{{- end -}}
{{- if or (not $foundation.keyRecovery.required) (ne $foundation.keyRecovery.keyPrefix "kuberploy-sealed-secrets-key") -}}{{ fail "Sealed Secrets requires the installer key-recovery contract" }}{{- end -}}
{{- if $foundation.controller.managed -}}
  {{- if empty $foundation.networkPolicy.kubeAPIServerCIDRs -}}{{ fail "managed Sealed Secrets requires explicit Kubernetes API CIDRs" }}{{- end -}}
  {{- range $foundation.networkPolicy.kubeAPIServerCIDRs -}}{{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "Kubernetes API CIDRs cannot be all-address ranges" }}{{- end -}}{{- end -}}
  {{- if or (ne $sealed.nameOverride "sealed-secrets") (ne $sealed.fullnameOverride "kuberploy-sealed-secrets") (not (empty $sealed.namespace)) -}}{{ fail "Sealed Secrets names and namespace identity are locked" }}{{- end -}}
  {{- if or (ne $sealed.image.registry "docker.io") (ne $sealed.image.repository "bitnami/sealed-secrets-controller") (ne $sealed.image.tag "0.38.4@sha256:ab8e4687a97fb097f30ca2f028222f779f231c224555ba05f43d172c61f84497") (not (empty $sealed.image.pullSecrets)) -}}{{ fail "Sealed Secrets image is locked by multi-platform digest" }}{{- end -}}
  {{- if or (not $sealed.createController) (not $sealed.updateStatus) $sealed.skipRecreate (ne $sealed.secretName "kuberploy-sealed-secrets-key") (ne ($sealed.keyrenewperiod | toString) "720h") (ne ($sealed.keyttl | toString) "87600h") (not (empty $sealed.keycutofftime)) -}}{{ fail "Sealed Secrets reconciliation and rotation policy is locked" }}{{- end -}}
  {{- if or (not (empty $sealed.additionalNamespaces)) (not (empty $sealed.command)) (not (empty $sealed.args)) (not (empty $sealed.extraDeploy)) (not (empty $sealed.additionalVolumes)) (not (empty $sealed.additionalVolumeMounts)) (not (empty $sealed.podLabels)) (not (empty $sealed.podAnnotations)) -}}{{ fail "Sealed Secrets arbitrary namespace, command, object, volume, and metadata injection is forbidden" }}{{- end -}}
  {{- if or (not (deepEqual $sealed.privateKeyAnnotations (dict "kuberploy.io/recovery-required" "true"))) (not (deepEqual $sealed.privateKeyLabels (dict "kuberploy.io/sealing-key" "true"))) -}}{{ fail "Sealed Secrets key identity and recovery labels are locked" }}{{- end -}}
  {{- if or (ne ($sealed.rateLimit | toString) "10") (ne ($sealed.rateLimitBurst | toString) "20") (not $sealed.logInfoStdout) (ne $sealed.logLevel "INFO") (ne $sealed.logFormat "json") (ne ($sealed.maxRetries | toString) "5") $sealed.watchForSecrets -}}{{ fail "Sealed Secrets logging, retry, and certificate endpoint limits are locked" }}{{- end -}}
  {{- if or (not $sealed.livenessProbe.enabled) (not $sealed.readinessProbe.enabled) (not $sealed.startupProbe.enabled) (not (empty $sealed.customLivenessProbe)) (not (empty $sealed.customReadinessProbe)) (not (empty $sealed.customStartupProbe)) -}}{{ fail "Sealed Secrets health probes cannot be disabled or replaced" }}{{- end -}}
  {{- if or (not $sealed.podSecurityContext.enabled) (ne (int $sealed.podSecurityContext.fsGroup) 65534) (ne $sealed.podSecurityContext.seccompProfile.type "RuntimeDefault") -}}{{ fail "Sealed Secrets Pod security context is locked" }}{{- end -}}
  {{- $security := $sealed.containerSecurityContext -}}
  {{- if or (not $security.enabled) (not $security.readOnlyRootFilesystem) (not $security.runAsNonRoot) (ne (int $security.runAsUser) 1001) $security.allowPrivilegeEscalation (not (deepEqual $security.capabilities.drop (list "ALL"))) -}}{{ fail "Sealed Secrets container security context is locked" }}{{- end -}}
  {{- if or (empty $sealed.resources.requests.cpu) (empty $sealed.resources.requests.memory) (empty $sealed.resources.limits.cpu) (empty $sealed.resources.limits.memory) -}}{{ fail "Sealed Secrets CPU and memory requests and limits are required" }}{{- end -}}
  {{- if or $sealed.hostNetwork (not (empty $sealed.hostPorts.http)) (not (empty $sealed.hostPorts.metrics)) (ne (int $sealed.containerPorts.http) 8080) (ne (int $sealed.containerPorts.metrics) 8081) (ne $sealed.dnsPolicy "ClusterFirst") -}}{{ fail "Sealed Secrets host and port network boundary is locked" }}{{- end -}}
  {{- if or (ne $sealed.service.type "ClusterIP") (ne (int $sealed.service.port) 8080) $sealed.ingress.enabled $sealed.ingress.tls $sealed.ingress.selfSigned (not (empty $sealed.ingress.secrets)) (ne $sealed.metrics.service.type "ClusterIP") (ne (int $sealed.metrics.service.port) 8081) -}}{{ fail "Sealed Secrets must remain private and ClusterIP-only" }}{{- end -}}
  {{- if or $sealed.networkPolicy.enabled (not $sealed.serviceAccount.create) (ne $sealed.serviceAccount.name "kuberploy-sealed-secrets") (not $sealed.rbac.create) (not $sealed.rbac.clusterRole) $sealed.rbac.namespacedRoles $sealed.rbac.pspEnabled $sealed.rbac.serviceProxier.create $sealed.rbac.serviceProxier.bind -}}{{ fail "Sealed Secrets RBAC and wrapper NetworkPolicy ownership are locked" }}{{- end -}}
  {{- if or $sealed.metrics.prometheusRule.enabled $sealed.metrics.serviceMonitor.enabled $sealed.metrics.dashboards.create $sealed.pdb.create -}}{{ fail "Sealed Secrets optional rule, dashboard, and PDB injection is forbidden" }}{{- end -}}
{{- end -}}
{{- end -}}
