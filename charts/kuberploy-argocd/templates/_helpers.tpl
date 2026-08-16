{{- define "kuberploy-argocd.labels" -}}
app.kubernetes.io/name: kuberploy-argocd
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-argocd.bootstrapRepositoryURL" -}}
{{- printf "https://github.com/%s/%s.git" .Values.argoFoundation.bootstrap.repositoryOwner .Values.argoFoundation.bootstrap.repositoryName -}}
{{- end -}}

{{- define "kuberploy-argocd.bootstrapPath" -}}
{{- printf "clusters/%s/argocd" .Values.argoFoundation.bootstrap.clusterID -}}
{{- end -}}

{{- define "kuberploy-argocd.bootstrapRepositorySecretName" -}}
{{- printf "kuberploy-repo-%s" (replace "-" "" .Values.argoFoundation.bootstrap.bindingID) -}}
{{- end -}}

{{- define "kuberploy-argocd.validateEmptyInjection" -}}
{{- $component := index . 0 -}}
{{- $name := index . 1 -}}
{{- range $key := list "extraArgs" "env" "envFrom" "extraEnv" "extraEnvFrom" "extraContainers" "initContainers" "volumeMounts" "volumes" "extraVolumeMounts" "extraVolumes" "podAnnotations" "podLabels" "imagePullSecrets" -}}
  {{- if and (hasKey $component $key) (not (empty (index $component $key))) -}}{{ fail (printf "managed Argo CD forbids %s.%s injection" $name $key) }}{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-argocd.validateSecurity" -}}
{{- $component := index . 0 -}}
{{- $name := index . 1 -}}
{{- $security := $component.containerSecurityContext -}}
{{- if or (not $security.runAsNonRoot) (not $security.readOnlyRootFilesystem) $security.allowPrivilegeEscalation (ne $security.seccompProfile.type "RuntimeDefault") (not (deepEqual $security.capabilities.drop (list "ALL"))) -}}{{ fail (printf "%s container security context is locked" $name) }}{{- end -}}
{{- end -}}

{{- define "kuberploy-argocd.validate" -}}
{{- $argo := index .Values "argo-cd" -}}
{{- if ne .Release.Namespace "kuberploy-system" -}}{{ fail "kuberploy-argocd must be bootstrapped by the installer release in kuberploy-system" }}{{- end -}}
{{- if not (regexMatch "^[^[:space:]]+$" .Values.argoFoundation.applicationReconcilerImage) -}}{{ fail "Argo bootstrap reconciler requires a Kubernetes image reference" }}{{- end -}}
{{- if eq .Values.argoFoundation.argoCD.managed .Values.argoFoundation.argoCD.adoptExisting -}}{{ fail "exactly one of managed Argo CD or adopted Argo CD must be selected" }}{{- end -}}
{{- if and .Values.argoFoundation.argoCD.adoptExisting (not .Values.argoFoundation.argoCD.capabilitiesConfirmed) -}}{{ fail "Argo CD adoption requires a completed compatibility and capability check" }}{{- end -}}
{{- if and (not .Values.argoFoundation.argoCD.managed) .Values.argoFoundation.argoCD.crdsPreinstalledByParent -}}{{ fail "adopted Argo CD cannot claim installer-owned CRD preinstallation" }}{{- end -}}
{{- range $key, $want := dict "valkeyNamespace" "kuberploy-system" "controlPlaneNamespace" "kuberploy-system" "edgeNamespace" "kuberploy-system" "monitoringNamespace" "kuberploy-monitoring" -}}
  {{- if ne (index $.Values.argoFoundation.networkPolicy $key) $want -}}{{ fail (printf "Argo CD %s is locked" $key) }}{{- end -}}
{{- end -}}
{{- range $key := keys .Values.argoFoundation.bootstrap -}}
  {{- if not (has $key (list "enabled" "clusterID" "bindingID" "repositoryOwner" "repositoryName" "targetRevision")) -}}{{ fail (printf "unknown root bootstrap authority field %s" $key) }}{{- end -}}
{{- end -}}
{{- if not (kindIs "bool" .Values.argoFoundation.bootstrap.enabled) -}}{{ fail "root bootstrap enabled must be boolean" }}{{- end -}}
{{- range $key := list "clusterID" "bindingID" "repositoryOwner" "repositoryName" "targetRevision" -}}
  {{- if not (kindIs "string" (index $.Values.argoFoundation.bootstrap $key)) -}}{{ fail (printf "root bootstrap %s must be a string" $key) }}{{- end -}}
{{- end -}}
{{- if .Values.argoFoundation.bootstrap.enabled -}}
  {{- $bootstrap := .Values.argoFoundation.bootstrap -}}
  {{- if not (regexMatch `^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` $bootstrap.clusterID) -}}{{ fail "bootstrap clusterID must be the exact platform binding cluster UUID" }}{{- end -}}
  {{- if not (regexMatch `^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` $bootstrap.bindingID) -}}{{ fail "bootstrap bindingID must be the exact platform binding UUID" }}{{- end -}}
  {{- if not (regexMatch `^[A-Za-z0-9]([A-Za-z0-9-]{0,38}[A-Za-z0-9])?$` $bootstrap.repositoryOwner) -}}{{ fail "bootstrap repositoryOwner must be a canonical GitHub login" }}{{- end -}}
  {{- if or (not (regexMatch `^[A-Za-z0-9_.-]{1,100}$` $bootstrap.repositoryName)) (has $bootstrap.repositoryName (list "." "..")) -}}{{ fail "bootstrap repositoryName must be the provider-verified GitHub repository name" }}{{- end -}}
  {{- if or (not (regexMatch `^refs/heads/[A-Za-z0-9]([A-Za-z0-9._/-]{0,198}[A-Za-z0-9])?$` $bootstrap.targetRevision)) (contains ".." $bootstrap.targetRevision) (contains "//" $bootstrap.targetRevision) (hasSuffix ".lock" $bootstrap.targetRevision) (contains "@{" $bootstrap.targetRevision) -}}{{ fail "bootstrap targetRevision must be the exact normalized platform branch ref" }}{{- end -}}
  {{- range (splitList "/" (trimPrefix "refs/heads/" $bootstrap.targetRevision)) -}}
    {{- if or (empty .) (hasPrefix "." .) (hasSuffix "." .) -}}{{ fail "bootstrap targetRevision contains an invalid branch component" }}{{- end -}}
  {{- end -}}
{{- end -}}
{{- if .Values.argoFoundation.argoCD.managed -}}
  {{- range .Values.argoFoundation.networkPolicy.kubeAPIServerCIDRs -}}{{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "Kubernetes API CIDRs cannot be all-address ranges" }}{{- end -}}{{- end -}}
  {{- range .Values.argoFoundation.networkPolicy.repositoryEgressCIDRs -}}{{- if has . (list "0.0.0.0/0" "::/0") -}}{{ fail "repository egress all-address defaults must be selected by an empty optional CIDR list" }}{{- end -}}{{- end -}}
  {{- if or (ne $argo.nameOverride "argocd") (ne $argo.fullnameOverride "argocd") (ne $argo.namespaceOverride "argocd") -}}{{ fail "Argo CD names and namespace identity are locked" }}{{- end -}}
  {{- $parentOwnsCRDs := .Values.argoFoundation.argoCD.crdsPreinstalledByParent -}}
  {{- if or (not $argo.crds.keep) (not $argo.createClusterRoles) $argo.createAggregateRoles -}}{{ fail "Argo CD CRD retention and RBAC ownership are locked" }}{{- end -}}
  {{- if eq $argo.crds.install $parentOwnsCRDs -}}{{ fail "managed Argo CD must install CRDs itself or attest exact parent preinstallation, never both or neither" }}{{- end -}}
  {{- if or $argo.global.networkPolicy.create (not (empty $argo.global.env)) (not (empty $argo.global.extraVolumes)) (not (empty $argo.global.extraVolumeMounts)) (not (empty $argo.global.podAnnotations)) (not (empty $argo.global.podLabels)) (not (empty $argo.global.hostAliases)) -}}{{ fail "Argo CD global injection and upstream NetworkPolicy overrides are forbidden" }}{{- end -}}
  {{- if or (ne $argo.global.logging.format "json") (ne $argo.global.logging.level "info") (not $argo.global.securityContext.runAsNonRoot) (ne $argo.global.securityContext.seccompProfile.type "RuntimeDefault") -}}{{ fail "Argo CD global logging and Pod security are locked" }}{{- end -}}
  {{- if or (not $argo.configs.cm.create) (index $argo.configs.cm "admin.enabled") (index $argo.configs.cm "exec.enabled") (index $argo.configs.cm "statusbadge.enabled") -}}{{ fail "Argo CD local admin, exec, and status badge must remain disabled" }}{{- end -}}
  {{- if or (not $argo.configs.params.create) (ne (index $argo.configs.params "redis.db") "1") (ne (index $argo.configs.params "server.insecure") "false") (ne (index $argo.configs.params "server.disable.auth") "false") (ne (index $argo.configs.params "reposerver.disable.tls") "false") -}}{{ fail "Argo CD transport, authentication, and Valkey database parameters are locked" }}{{- end -}}
  {{- if or (not $argo.configs.rbac.create) (not (empty (index $argo.configs.rbac "policy.default"))) (not (empty (index $argo.configs.rbac "policy.csv"))) -}}{{ fail "Argo CD default users receive no direct permissions" }}{{- end -}}
  {{- if or $argo.configs.secret.createSecret (not (empty $argo.configs.secret.githubSecret)) (not (empty $argo.configs.secret.gitlabSecret)) (not (empty $argo.configs.secret.extra)) (not (empty $argo.configs.credentialTemplates)) (not (empty $argo.configs.repositories)) (not (empty $argo.configs.clusterCredentials)) (not (empty $argo.extraObjects)) -}}{{ fail "Argo CD credentials and arbitrary resources must be delivered out of band" }}{{- end -}}
  {{- if or $argo.dex.enabled $argo.redis.enabled (index $argo "redis-ha").enabled $argo.redisSecretInit.enabled $argo.notifications.enabled $argo.commitServer.enabled -}}{{ fail "managed Argo CD uses external Valkey and disables Dex, notifications, commit-server, and bundled Redis" }}{{- end -}}
  {{- if or (ne $argo.externalRedis.host "kuberploy-valkey.kuberploy-system.svc.cluster.local") (ne (int $argo.externalRedis.port) 6379) (ne $argo.externalRedis.existingSecret "kuberploy-argocd-valkey-auth") (not (empty $argo.externalRedis.username)) (not (empty $argo.externalRedis.password)) -}}{{ fail "Argo CD external Valkey endpoint and existing Secret reference are locked" }}{{- end -}}
  {{- include "kuberploy-argocd.validateEmptyInjection" (list $argo.controller "controller") -}}
  {{- include "kuberploy-argocd.validateEmptyInjection" (list $argo.server "server") -}}
  {{- include "kuberploy-argocd.validateEmptyInjection" (list $argo.repoServer "repoServer") -}}
  {{- include "kuberploy-argocd.validateEmptyInjection" (list $argo.applicationSet "applicationSet") -}}
  {{- include "kuberploy-argocd.validateSecurity" (list $argo.controller "controller") -}}
  {{- include "kuberploy-argocd.validateSecurity" (list $argo.server "server") -}}
  {{- include "kuberploy-argocd.validateSecurity" (list $argo.repoServer "repoServer") -}}
  {{- include "kuberploy-argocd.validateSecurity" (list $argo.applicationSet "applicationSet") -}}
  {{- if or (lt (int $argo.controller.replicas) 1) (not $argo.controller.automountServiceAccountToken) (not $argo.controller.serviceAccount.create) (not $argo.controller.serviceAccount.automountServiceAccountToken) -}}{{ fail "Argo CD application-controller must remain enabled with its ServiceAccount" }}{{- end -}}
  {{- if or (lt (int $argo.server.replicas) 1) $argo.server.hostNetwork (ne $argo.server.service.type "ClusterIP") $argo.server.ingress.enabled $argo.server.ingressGrpc.enabled $argo.server.route.enabled $argo.server.httproute.enabled $argo.server.grpcroute.enabled $argo.server.backendTLSPolicy.enabled $argo.server.listenerset.enabled $argo.server.extensions.enabled -}}{{ fail "Argo CD server must remain authenticated and ClusterIP-only" }}{{- end -}}
  {{- if or (lt (int $argo.repoServer.replicas) 1) $argo.repoServer.hostNetwork -}}{{ fail "Argo CD repo-server must remain enabled without host networking" }}{{- end -}}
  {{- if or (not $argo.applicationSet.enabled) (lt (int $argo.applicationSet.replicas) 1) $argo.applicationSet.ingress.enabled $argo.applicationSet.httproute.enabled -}}{{ fail "Argo CD ApplicationSet must remain private and enabled" }}{{- end -}}
{{- end -}}
{{- end -}}
