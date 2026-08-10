{{- define "kuberploy-valkey.labels" -}}
app.kubernetes.io/name: kuberploy-valkey
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-valkey.validate" -}}
{{- if ne .Release.Namespace "kuberploy-system" -}}{{ fail "kuberploy-valkey must use the shared protected kuberploy-system namespace" }}{{- end -}}
{{- if eq .Values.valkeyFoundation.managed .Values.valkeyFoundation.adoptExisting -}}{{ fail "exactly one of valkeyFoundation.managed or valkeyFoundation.adoptExisting must be true" }}{{- end -}}
{{- if not .Values.valkeyFoundation.networkPolicy.enabled -}}{{ fail "Valkey NetworkPolicy cannot be disabled" }}{{- end -}}
{{- if ne .Values.valkeyFoundation.networkPolicy.controlPlaneNamespace "kuberploy-system" -}}{{ fail "Valkey control-plane namespace identity is locked" }}{{- end -}}
{{- if ne .Values.valkeyFoundation.networkPolicy.argoCDNamespace "argocd" -}}{{ fail "Valkey Argo CD namespace identity is locked" }}{{- end -}}
{{- if .Values.valkeyFoundation.managed -}}
  {{- if ne .Values.valkey.image.registry "docker.io" -}}{{ fail "Valkey image registry is locked" }}{{- end -}}
  {{- if ne .Values.valkey.image.repository "valkey/valkey" -}}{{ fail "Valkey image repository is locked" }}{{- end -}}
  {{- if ne .Values.valkey.image.tag "9.1.1@sha256:3acc0687f2a2e1091fae6450d7842dd658c941338cf0a873ddd9e14b9e4ea4dd" -}}{{ fail "Valkey image digest is locked" }}{{- end -}}
  {{- if or (not (empty .Values.valkey.global.imageRegistry)) (not (empty .Values.valkey.global.imagePullSecrets)) (not (empty .Values.valkey.imagePullSecrets)) -}}{{ fail "Valkey image registry and pull credentials cannot be overridden" }}{{- end -}}
  {{- if or (ne .Values.valkey.nameOverride "valkey") (ne .Values.valkey.fullnameOverride "kuberploy-valkey") -}}{{ fail "Valkey policy identity names are locked" }}{{- end -}}
  {{- if or (not .Values.valkey.serviceAccount.create) .Values.valkey.serviceAccount.automount (not (empty .Values.valkey.serviceAccount.annotations)) (not (empty .Values.valkey.serviceAccount.name)) -}}{{ fail "Valkey requires its exact tokenless ServiceAccount" }}{{- end -}}
  {{- if or (ne (int .Values.valkey.podSecurityContext.runAsUser) 1000) (ne (int .Values.valkey.podSecurityContext.runAsGroup) 1000) (ne (int .Values.valkey.podSecurityContext.fsGroup) 1000) (ne .Values.valkey.podSecurityContext.seccompProfile.type "RuntimeDefault") -}}{{ fail "Valkey pod security context is locked" }}{{- end -}}
  {{- if or .Values.valkey.securityContext.allowPrivilegeEscalation (not .Values.valkey.securityContext.readOnlyRootFilesystem) (not .Values.valkey.securityContext.runAsNonRoot) (ne (int .Values.valkey.securityContext.runAsUser) 1000) (not (deepEqual .Values.valkey.securityContext.capabilities.drop (list "ALL"))) -}}{{ fail "Valkey container security context is locked" }}{{- end -}}
  {{- if or (ne .Values.valkey.service.type "ClusterIP") (ne (int .Values.valkey.service.port) 6379) (ne (int .Values.valkey.service.nodePort) 0) (not (empty .Values.valkey.service.annotations)) (not (empty .Values.valkey.service.loadBalancerClass)) (not (empty .Values.valkey.service.loadBalancerSourceRanges)) -}}{{ fail "Valkey must remain ClusterIP-only on TCP 6379" }}{{- end -}}
  {{- if or (not .Values.valkey.auth.enabled) (empty .Values.valkey.auth.usersExistingSecret) (not (empty .Values.valkey.auth.aclConfig)) (ne (len .Values.valkey.auth.aclUsers) 6) (not (hasKey .Values.valkey.auth.aclUsers "default")) (not (hasKey .Values.valkey.auth.aclUsers "api-cache")) (not (hasKey .Values.valkey.auth.aclUsers "api-limiter")) (not (hasKey .Values.valkey.auth.aclUsers "outbox-publisher")) (not (hasKey .Values.valkey.auth.aclUsers "worker-consumer")) (not (hasKey .Values.valkey.auth.aclUsers "argocd")) -}}{{ fail "Valkey requires the exact existing-Secret-backed health, cache, limiter, publisher, consumer, and Argo CD ACL users" }}{{- end -}}
  {{- $default := index .Values.valkey.auth.aclUsers "default" -}}
  {{- if or (ne $default.permissions "resetkeys resetchannels -@all +ping") (not (empty $default.password)) (ne $default.passwordKey "health-password") -}}{{ fail "Valkey default user is locked to authenticated health probes only" }}{{- end -}}
  {{- $apiCache := index .Values.valkey.auth.aclUsers "api-cache" -}}
  {{- if or (ne $apiCache.permissions "~kp:v1:cache:* +get +set +del +ping") (not (empty $apiCache.password)) (ne $apiCache.passwordKey "api-cache-password") -}}{{ fail "Valkey API cache ACL and password source are locked" }}{{- end -}}
  {{- $apiLimiter := index .Values.valkey.auth.aclUsers "api-limiter" -}}
  {{- if or (ne $apiLimiter.permissions "~kp:v1:limit:* +evalsha +eval +script|load +incrby +pttl +pexpire +ping") (not (empty $apiLimiter.password)) (ne $apiLimiter.passwordKey "api-limiter-password") -}}{{ fail "Valkey API limiter ACL and password source are locked" }}{{- end -}}
  {{- $publisher := index .Values.valkey.auth.aclUsers "outbox-publisher" -}}
  {{- if or (ne $publisher.permissions "~kp:v1:work:git-write ~kp:v1:work:dataset-id +xadd +get +set +ping") (not (empty $publisher.password)) (ne $publisher.passwordKey "outbox-publisher-password") -}}{{ fail "Valkey outbox publisher ACL and password source are locked" }}{{- end -}}
  {{- $consumer := index .Values.valkey.auth.aclUsers "worker-consumer" -}}
  {{- if or (ne $consumer.permissions "~kp:v1:work:* +xgroup +xreadgroup +xautoclaim +xack +ping") (not (empty $consumer.password)) (ne $consumer.passwordKey "worker-consumer-password") -}}{{ fail "Valkey worker consumer ACL and password source are locked" }}{{- end -}}
  {{- $argocd := index .Values.valkey.auth.aclUsers "argocd" -}}
  {{- if or (ne $argocd.permissions "~* &* +@all -@dangerous") (not (empty $argocd.password)) (ne $argocd.passwordKey "argocd-password") -}}{{ fail "Valkey Argo CD ACL permissions and password source are locked" }}{{- end -}}
  {{- if ne .Values.valkey.replica.replicationUser "worker-consumer" -}}{{ fail "disabled P0 replication identity is locked away from the API identities" }}{{- end -}}
  {{- if .Values.valkey.replica.enabled -}}{{ fail "the P0 managed Valkey profile is one recoverable standalone instance" }}{{- end -}}
  {{- if or (not .Values.valkey.dataStorage.enabled) (empty .Values.valkey.dataStorage.requestedSize) (ne .Values.valkey.dataStorage.volumeName "valkey-data") (not .Values.valkey.dataStorage.keepPvc) (not (empty .Values.valkey.dataStorage.hostPath)) (not (empty .Values.valkey.dataStorage.subPath)) (not (deepEqual .Values.valkey.dataStorage.accessModes (list "ReadWriteOnce"))) -}}{{ fail "managed Valkey requires a retained RWO PVC and forbids hostPath" }}{{- end -}}
  {{- if ne .Values.valkey.valkeyConfig "maxmemory 384mb\nmaxmemory-policy noeviction\nappendonly yes\nappendfsync everysec\naof-use-rdb-preamble yes\nsave 900 1\nsave 300 10\nsave 60 10000\n" -}}{{ fail "Valkey durability, memory, and noeviction policy is locked" }}{{- end -}}
  {{- if or (not .Values.valkey.startupProbe.enabled) (not .Values.valkey.livenessProbe.enabled) (not .Values.valkey.readinessProbe.enabled) (not (empty .Values.valkey.startupProbe.customProbe)) (not (empty .Values.valkey.livenessProbe.customProbe)) (not (empty .Values.valkey.readinessProbe.customProbe)) -}}{{ fail "Valkey health probes cannot be disabled or replaced" }}{{- end -}}
  {{- if or (empty .Values.valkey.resources.requests.cpu) (empty .Values.valkey.resources.requests.memory) (empty .Values.valkey.resources.limits.cpu) (empty .Values.valkey.resources.limits.memory) -}}{{ fail "Valkey CPU and memory requests and limits are required" }}{{- end -}}
  {{- if or (ne .Values.valkey.deploymentStrategy "Recreate") .Values.valkey.tls.enabled .Values.valkey.metrics.enabled (not (empty .Values.valkey.networkPolicy)) -}}{{ fail "Valkey deployment strategy, TLS boundary, metrics sidecar, and wrapper NetworkPolicy are locked" }}{{- end -}}
  {{- if or (not (empty .Values.valkey.podAnnotations)) (not (empty .Values.valkey.podLabels)) (not (empty .Values.valkey.workloadAnnotations)) (not (empty .Values.valkey.commonLabels)) (not (empty .Values.valkey.env)) -}}{{ fail "Valkey identity and environment injection are forbidden" }}{{- end -}}
  {{- if or (not (empty .Values.valkey.extraInitContainers)) (not (empty .Values.valkey.extraContainers)) (not (empty .Values.valkey.extraValkeySecrets)) (not (empty .Values.valkey.extraValkeyConfigs)) .Values.valkey.extraSecretValkeyConfigs (not (empty .Values.valkey.extraVolumes)) (not (empty .Values.valkey.extraVolumeMounts)) -}}{{ fail "Valkey arbitrary container, volume, and config injection are forbidden" }}{{- end -}}
{{- end -}}
{{- end -}}
