{{- define "kuberploy-postgresql.labels" -}}
app.kubernetes.io/name: kuberploy-postgresql
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kuberploy-postgresql.selectorLabels" -}}
app.kubernetes.io/name: kuberploy-postgresql
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kuberploy-postgresql.validate" -}}
{{- $pg := .Values.postgresqlFoundation -}}
{{- if ne .Release.Namespace "kuberploy-system" -}}{{ fail "kuberploy-postgresql must use the shared protected kuberploy-system namespace" }}{{- end -}}
{{- if eq $pg.managed $pg.adoptExisting -}}{{ fail "exactly one of managed PostgreSQL or adopted PostgreSQL must be selected" }}{{- end -}}
{{- if not $pg.networkPolicy.enabled -}}{{ fail "PostgreSQL NetworkPolicy cannot be disabled" }}{{- end -}}
{{- if ne $pg.networkPolicy.controlPlaneNamespace "kuberploy-system" -}}{{ fail "PostgreSQL control-plane namespace is locked" }}{{- end -}}
{{- if $pg.managed -}}
  {{- if ne $pg.image.reference "docker.io/library/postgres:18.4-alpine3.24@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15" -}}{{ fail "PostgreSQL image is locked by multi-platform digest" }}{{- end -}}
  {{- if or (empty $pg.auth.existingSecret) (ne $pg.auth.usernameKey "username") (ne $pg.auth.passwordKey "password") (ne $pg.auth.databaseKey "database") -}}{{ fail "PostgreSQL requires the exact existing Secret key contract" }}{{- end -}}
  {{- if ne (int $pg.service.port) 5432 -}}{{ fail "PostgreSQL service port is locked to 5432" }}{{- end -}}
  {{- if or (not $pg.storage.keepPVC) (not (deepEqual $pg.storage.accessModes (list "ReadWriteOnce"))) (empty $pg.storage.requestedSize) -}}{{ fail "PostgreSQL requires a retained RWO PVC" }}{{- end -}}
  {{- if or (empty $pg.resources.requests.cpu) (empty $pg.resources.requests.memory) (empty $pg.resources.limits.cpu) (empty $pg.resources.limits.memory) -}}{{ fail "PostgreSQL CPU and memory requests and limits are required" }}{{- end -}}
  {{- if or (lt (int $pg.database.maxConnections) 20) (gt (int $pg.database.maxConnections) 500) (ne $pg.database.checkpointCompletionTarget "0.9") (ne $pg.database.walCompression "pglz") -}}{{ fail "PostgreSQL managed configuration is outside the bounded profile" }}{{- end -}}
{{- end -}}
{{- end -}}
