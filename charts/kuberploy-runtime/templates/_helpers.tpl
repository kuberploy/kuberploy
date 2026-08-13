{{/* Stable names are hashes of immutable IDs, never mutable display names. */}}
{{- define "kuberploy-runtime.appName" -}}
{{- $id := required "metadata.id is required" .Values.metadata.id -}}
{{- printf "kp-a-%s" (sha256sum $id | trunc 16) -}}
{{- end -}}

{{/* Parent VariableSet value files are merged by Helm in project ->
environment order; the final AppConfig remains the last value file. Resolve
application entries last so both ordinary values and explicit secret bindings
can override inherited ordinary values. */}}
{{- define "kuberploy-runtime.effectiveEnv" -}}
{{- $byName := dict -}}
{{- range $name := keys (.Values.values | default (dict)) | sortAlpha -}}
  {{- $_ := set $byName $name (dict "name" $name "value" (get $.Values.values $name)) -}}
{{- end -}}
{{- range (.Values.spec.runtime.env | default (list)) -}}
  {{- $_ := set $byName .name . -}}
{{- end -}}
{{- $effective := list -}}
{{- range $name := keys $byName | sortAlpha -}}
  {{- $effective = append $effective (get $byName $name) -}}
{{- end -}}
{{- toJson $effective -}}
{{- end -}}

{{- define "kuberploy-runtime.configMapName" -}}
{{- $ordinary := list -}}
{{- range (include "kuberploy-runtime.effectiveEnv" . | fromJsonArray) -}}
  {{- if hasKey . "value" -}}
    {{- $ordinary = append $ordinary (dict "name" .name "value" .value) -}}
  {{- end -}}
{{- end -}}
{{- printf "%s-c-%s" (include "kuberploy-runtime.appName" .) (toJson $ordinary | sha256sum | trunc 12) -}}
{{- end -}}

{{- define "kuberploy-runtime.routeKey" -}}
{{- if .route.id -}}
{{- .route.id -}}
{{- else -}}
{{- printf "%s|%s|%s" .route.host (.route.path | default "/") .route.port -}}
{{- end -}}
{{- end -}}

{{- define "kuberploy-runtime.routeName" -}}
{{- $key := include "kuberploy-runtime.routeKey" . -}}
{{- printf "%s-r-%s" (include "kuberploy-runtime.appName" .root) (sha256sum $key | trunc 12) -}}
{{- end -}}

{{- define "kuberploy-runtime.middlewareName" -}}
{{- $logical := .middleware.id | default .middleware.name -}}
{{- $identity := sha256sum $logical | trunc 10 -}}
{{- $content := toJson .middleware.spec | sha256sum | trunc 8 -}}
{{- printf "%s-m-%s-%s" (include "kuberploy-runtime.appName" .root) $identity $content -}}
{{- end -}}

{{- define "kuberploy-runtime.middlewareRef" -}}
{{- $root := .root -}}
{{- $wanted := .name -}}
{{- $matched := list -}}
{{- range ($root.Values.spec.middlewares | default (list)) -}}
  {{- if eq .name $wanted -}}
    {{- $matched = append $matched (include "kuberploy-runtime.middlewareName" (dict "root" $root "middleware" .)) -}}
  {{- end -}}
{{- end -}}
{{- if ne (len $matched) 1 -}}
  {{- fail (printf "route middlewareRef %q must match exactly one spec.middlewares entry" $wanted) -}}
{{- end -}}
{{- index $matched 0 -}}
{{- end -}}

{{- define "kuberploy-runtime.secretName" -}}
{{- $bindingID := required "secretBindingRef.bindingId is required" .bindingId -}}
{{- $name := required "secretBindingRef.name is required" .name -}}
{{- $version := int64 (required "secretBindingRef.version is required" .version) -}}
{{- if lt $version 1 -}}
  {{- fail "secretBindingRef.version must be a positive integer" -}}
{{- end -}}
{{- $versionText := printf "%d" $version -}}
{{- $identity := printf "%s%c%s" $bindingID 0 $versionText -}}
{{- $suffix := sha256sum $identity | trunc 10 -}}
{{- $maximumName := sub 47 (len $versionText) | int -}}
{{- $boundedName := trunc $maximumName $name | trimAll "-" -}}
{{- printf "kp-%s-v%s-%s" $boundedName $versionText $suffix -}}
{{- end -}}

{{- define "kuberploy-runtime.image" -}}
{{- $repository := required "spec.delivery.release.repository is required" .Values.spec.delivery.release.repository -}}
{{- $digest := required "spec.delivery.release.digest is required" .Values.spec.delivery.release.digest -}}
{{- if contains "@" $repository -}}
  {{- fail "spec.delivery.release.repository must not contain @; digest is a separate field" -}}
{{- end -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" $digest) -}}
  {{- fail "spec.delivery.release.digest must be an immutable sha256 digest" -}}
{{- end -}}
{{- printf "%s@%s" $repository $digest -}}
{{- end -}}

{{- define "kuberploy-runtime.imagePullSecretName" -}}
{{- $namespace := required "a release namespace is required" .Release.Namespace -}}
{{- $pull := required "spec.delivery.registryPull is required" .Values.spec.delivery.registryPull -}}
{{- $targetID := required "spec.delivery.registryPull.targetId is required" $pull.targetId -}}
{{- $profileName := required "spec.delivery.registryPull.profileName is required" $pull.profileName -}}
{{- $revision := int64 (required "spec.delivery.registryPull.profileRevision is required" $pull.profileRevision) -}}
{{- if or (not (regexMatch "^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$" $profileName)) (lt $revision 1) -}}
  {{- fail "spec.delivery.registryPull profile metadata is invalid" -}}
{{- end -}}
{{- $identity := printf "kuberploy-runtime-pull-v1%c%s%c%s%c%d" 0 $namespace 0 $targetID 0 $revision -}}
{{- printf "kuberploy-pull-%s" (sha256sum $identity | trunc 24) -}}
{{- end -}}

{{- define "kuberploy-runtime.selectorLabels" -}}
app.kubernetes.io/name: kuberploy-runtime
app.kubernetes.io/instance: {{ include "kuberploy-runtime.appName" . }}
kuberploy.io/application: {{ required "spec.applicationId is required" .Values.spec.applicationId | quote }}
{{- end -}}

{{- define "kuberploy-runtime.labels" -}}
{{ include "kuberploy-runtime.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- with .Values.spec.projectId }}
kuberploy.io/project: {{ . | quote }}
{{- end }}
{{- with .Values.spec.environmentId }}
kuberploy.io/environment: {{ . | quote }}
{{- end }}
kuberploy.io/service: {{ required "spec.applicationId is required" .Values.spec.applicationId | quote }}
{{- if hasPrefix "kuberploy-e2e-" .Release.Namespace }}
kuberploy.io/test-run: {{ trimPrefix "kuberploy-e2e-" .Release.Namespace | quote }}
{{- end }}
{{- end -}}

{{- define "kuberploy-runtime.validate" -}}
{{- $expected := required "operator-owned kuberployExpectedIdentity is required" .Values.kuberployExpectedIdentity -}}
{{- if or (ne (len (keys $expected)) 3) (not (hasKey $expected "projectId")) (not (hasKey $expected "environmentId")) (not (hasKey $expected "applicationId")) -}}
  {{- fail "kuberployExpectedIdentity accepts only exact project/environment/application IDs" -}}
{{- end -}}
{{- if or (ne (required "expected projectId is required" $expected.projectId) .Values.spec.projectId) (ne (required "expected environmentId is required" $expected.environmentId) .Values.spec.environmentId) (ne (required "expected applicationId is required" $expected.applicationId) .Values.spec.applicationId) -}}
  {{- fail "AppConfig identity does not match the operator-owned expected identity" -}}
{{- end -}}
{{- if ne .Values.metadata.id .Values.spec.applicationId -}}
  {{- fail "metadata.id and spec.applicationId must be the same immutable application ID" -}}
{{- end -}}
{{- end -}}

{{/* A schema-bypassing Helm caller still cannot inject an unreviewed
Traefik family or nested configuration key. Semantic values are revalidated
by the server/direct-Git policy; this is the final structural render fence. */}}
{{- define "kuberploy-runtime.validateMiddleware" -}}
{{- $middleware := . -}}
{{- range $key := keys $middleware -}}
  {{- if not (has $key (list "id" "name" "profileRef" "spec")) -}}
    {{- fail (printf "middleware contains unsupported field %q" $key) -}}
  {{- end -}}
{{- end -}}
{{- $spec := required "middleware.spec is required" $middleware.spec -}}
{{- if ne (len (keys $spec)) 1 -}}
  {{- fail "middleware.spec must contain exactly one supported family" -}}
{{- end -}}
{{- $family := first (keys $spec) -}}
{{- $allowedByFamily := dict
  "redirectScheme" (list "scheme" "port" "permanent")
  "redirectRegex" (list "regex" "replacement" "permanent")
  "addPrefix" (list "prefix")
  "stripPrefix" (list "prefixes" "forceSlash")
  "stripPrefixRegex" (list "regex")
  "replacePath" (list "path")
  "replacePathRegex" (list "regex" "replacement")
  "headers" (list "customRequestHeaders" "customResponseHeaders" "accessControlAllowCredentials" "accessControlAllowHeaders" "accessControlAllowMethods" "accessControlAllowOriginList" "accessControlAllowOriginListRegex" "accessControlExposeHeaders" "accessControlMaxAge" "addVaryHeader" "allowedHosts" "stsSeconds" "stsIncludeSubdomains" "stsPreload" "forceSTSHeader" "frameDeny" "customFrameOptionsValue" "contentTypeNosniff" "browserXssFilter" "customBrowserXSSValue" "contentSecurityPolicy" "contentSecurityPolicyReportOnly" "referrerPolicy" "permissionsPolicy" "isDevelopment")
  "rateLimit" (list "average" "period" "burst")
  "inFlightReq" (list "amount")
  "ipAllowList" (list "sourceRange" "ipStrategy")
  "compress" (list "excludedContentTypes" "includedContentTypes" "minResponseBodyBytes" "defaultEncoding" "encodings")
  "buffering" (list "maxRequestBodyBytes" "memRequestBodyBytes" "maxResponseBodyBytes" "memResponseBodyBytes" "retryExpression")
  "retry" (list "attempts" "initialInterval")
  "basicAuth" (list "secretBindingRef" "removeHeader" "headerField") -}}
{{- if not (hasKey $allowedByFamily $family) -}}
  {{- fail (printf "middleware family %q is not supported" $family) -}}
{{- end -}}
{{- $config := get $spec $family -}}
{{- if not (kindIs "map" $config) -}}
  {{- fail (printf "middleware family %q must be an object" $family) -}}
{{- end -}}
{{- $allowed := get $allowedByFamily $family -}}
{{- range $key := keys $config -}}
  {{- if not (has $key $allowed) -}}
    {{- fail (printf "middleware family %q contains unsupported field %q" $family $key) -}}
  {{- end -}}
{{- end -}}
{{- if and (eq $family "ipAllowList") (hasKey $config "ipStrategy") -}}
  {{- range $key := keys $config.ipStrategy -}}
    {{- if not (has $key (list "depth" "excludedIPs" "ipv6Subnet")) -}}
      {{- fail (printf "middleware ipAllowList.ipStrategy contains unsupported field %q" $key) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- if eq $family "basicAuth" -}}
  {{- $secretRef := required "basicAuth.secretBindingRef is required" $config.secretBindingRef -}}
  {{- range $key := keys $secretRef -}}
    {{- if not (has $key (list "bindingId" "name" "key" "version")) -}}
      {{- fail (printf "basicAuth.secretBindingRef contains unsupported field %q" $key) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- with $middleware.profileRef -}}
  {{- range $key := keys . -}}
    {{- if not (has $key (list "profileId" "revision" "specDigest" "assignmentsDigest")) -}}
      {{- fail (printf "middleware.profileRef contains unsupported field %q" $key) -}}
    {{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{/* Direct pod anti-affinity accepts only the exact current-application
selector. Recheck it here so bypassing values.schema.json cannot turn it into
cross-application or arbitrary-label affinity. */}}
{{- define "kuberploy-runtime.validateSameApplicationAntiAffinityTerm" -}}
{{- $term := required "same-application pod anti-affinity term is required" .term -}}
{{- range $key := keys $term -}}
  {{- if not (has $key (list "labelSelector" "topologyKey")) -}}
    {{- fail (printf "podAntiAffinity term contains unsupported field %q" $key) -}}
  {{- end -}}
{{- end -}}
{{- $topologyKey := required "podAntiAffinity topologyKey is required" $term.topologyKey -}}
{{- $selector := required "podAntiAffinity labelSelector is required" $term.labelSelector -}}
{{- range $key := keys $selector -}}
  {{- if ne $key "matchLabels" -}}
    {{- fail (printf "same-application podAntiAffinity selector contains unsupported field %q" $key) -}}
  {{- end -}}
{{- end -}}
{{- $labels := required "podAntiAffinity matchLabels is required" $selector.matchLabels -}}
{{- if or (ne (len (keys $labels)) 1) (ne (get $labels "kuberploy.io/application") .applicationID) -}}
  {{- fail "podAntiAffinity must select only the exact current kuberploy.io/application" -}}
{{- end -}}
{{- end -}}
{{- define "kuberploy-runtime.validateRuntime" -}}
{{- $runtime := .Values.spec.runtime -}}
{{- with $runtime.priorityClassName -}}
  {{- if hasPrefix "system-" . -}}
    {{- fail "Kubernetes system-* PriorityClasses are reserved for cluster-critical workloads" -}}
  {{- end -}}
{{- end -}}
{{- $parents := .Values.values | default (dict) -}}
{{- if gt (len $parents) 256 -}}
  {{- fail "inherited VariableSet supports at most 256 ordinary values" -}}
{{- end -}}
{{- range $name, $value := $parents -}}
  {{- if not (regexMatch "^[A-Za-z_][A-Za-z0-9_]{0,127}$" $name) -}}
    {{- fail (printf "inherited variable name %q is invalid" $name) -}}
  {{- end -}}
  {{- if not (kindIs "string" $value) -}}
    {{- fail (printf "inherited variable %q must be an explicit string" $name) -}}
  {{- end -}}
  {{- if gt (len $value) 4096 -}}
    {{- fail (printf "inherited variable %q exceeds 4096 bytes" $name) -}}
  {{- end -}}
{{- end -}}
{{- with $runtime.affinity -}}
  {{- range $key := keys . -}}
    {{- if not (has $key (list "nodeAffinity" "podAffinity" "podAntiAffinity")) -}}
      {{- fail (printf "workload affinity contains unsupported field %q" $key) -}}
    {{- end -}}
  {{- end -}}
  {{- with .podAntiAffinity -}}
    {{- range $key := keys . -}}
      {{- if not (has $key (list "requiredDuringSchedulingIgnoredDuringExecution" "preferredDuringSchedulingIgnoredDuringExecution")) -}}
        {{- fail (printf "podAntiAffinity contains unsupported field %q" $key) -}}
      {{- end -}}
    {{- end -}}
    {{- $requiredTerms := .requiredDuringSchedulingIgnoredDuringExecution | default (list) -}}
    {{- $preferredTerms := .preferredDuringSchedulingIgnoredDuringExecution | default (list) -}}
    {{- if or (gt (len $requiredTerms) 16) (gt (len $preferredTerms) 16) -}}
      {{- fail "podAntiAffinity supports at most 16 required and 16 preferred terms" -}}
    {{- end -}}
    {{- range $requiredTerms -}}
      {{- include "kuberploy-runtime.validateSameApplicationAntiAffinityTerm" (dict "term" . "applicationID" $.Values.spec.applicationId) -}}
    {{- end -}}
    {{- range $preferredTerms -}}
      {{- range $key := keys . -}}
        {{- if not (has $key (list "weight" "podAffinityTerm")) -}}
          {{- fail (printf "preferred podAntiAffinity contains unsupported field %q" $key) -}}
        {{- end -}}
      {{- end -}}
      {{- $weight := int (required "preferred podAntiAffinity weight is required" .weight) -}}
      {{- if or (lt $weight 1) (gt $weight 100) -}}
        {{- fail "preferred podAntiAffinity weight must be between 1 and 100" -}}
      {{- end -}}
      {{- include "kuberploy-runtime.validateSameApplicationAntiAffinityTerm" (dict "term" .podAffinityTerm "applicationID" $.Values.spec.applicationId) -}}
    {{- end -}}
  {{- end -}}
  {{- with .podAffinity -}}
    {{- range $key := keys . -}}
      {{- if not (has $key (list "requiredDuringSchedulingIgnoredDuringExecution" "preferredDuringSchedulingIgnoredDuringExecution")) -}}
        {{- fail (printf "podAffinity contains unsupported field %q" $key) -}}
      {{- end -}}
    {{- end -}}
    {{- $requiredTerms := .requiredDuringSchedulingIgnoredDuringExecution | default (list) -}}
    {{- $preferredTerms := .preferredDuringSchedulingIgnoredDuringExecution | default (list) -}}
    {{- if or (gt (len $requiredTerms) 16) (gt (len $preferredTerms) 16) -}}
      {{- fail "podAffinity supports at most 16 required and 16 preferred terms" -}}
    {{- end -}}
    {{- range $requiredTerms -}}
      {{- include "kuberploy-runtime.validateSameApplicationAntiAffinityTerm" (dict "term" . "applicationID" $.Values.spec.applicationId) -}}
    {{- end -}}
    {{- range $preferredTerms -}}
      {{- $weight := int (required "preferred podAffinity weight is required" .weight) -}}
      {{- if or (lt $weight 1) (gt $weight 100) -}}
        {{- fail "preferred podAffinity weight must be between 1 and 100" -}}
      {{- end -}}
      {{- include "kuberploy-runtime.validateSameApplicationAntiAffinityTerm" (dict "term" .podAffinityTerm "applicationID" $.Values.spec.applicationId) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- range $runtime.topologySpreadConstraints | default (list) -}}
  {{- $selector := required "topologySpreadConstraint labelSelector is required" .labelSelector -}}
  {{- range $key := keys $selector -}}
    {{- if ne $key "matchLabels" -}}
      {{- fail (printf "same-application topology spread selector contains unsupported field %q" $key) -}}
    {{- end -}}
  {{- end -}}
  {{- $labels := required "topologySpreadConstraint matchLabels is required" $selector.matchLabels -}}
  {{- if or (ne (len (keys $labels)) 1) (ne (get $labels "kuberploy.io/application") $.Values.spec.applicationId) -}}
    {{- fail "topology spread must select only the exact current kuberploy.io/application" -}}
  {{- end -}}
{{- end -}}
{{- $portNames := dict -}}
{{- $portProtocols := dict -}}
{{- range .Values.spec.runtime.ports -}}
  {{- if hasKey $portNames .name -}}
    {{- fail (printf "duplicate runtime port name %q" .name) -}}
  {{- end -}}
  {{- $_ := set $portNames .name true -}}
  {{- $_ := set $portProtocols .name (.protocol | default "TCP") -}}
{{- end -}}
{{- range (.Values.spec.routes | default (list)) -}}
  {{- if not (hasKey $portNames .port) -}}
    {{- fail (printf "route %q references unknown runtime port %q" .host .port) -}}
  {{- end -}}
  {{- if ne (get $portProtocols .port) "TCP" -}}
    {{- fail (printf "route %q references non-TCP runtime port %q" .host .port) -}}
  {{- end -}}
{{- end -}}
{{- end -}}
