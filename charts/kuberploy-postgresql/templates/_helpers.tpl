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
{{- if ne $pg.networkPolicy.controlPlaneNamespace "kuberploy-system" -}}{{ fail "PostgreSQL control-plane namespace is locked" }}{{- end -}}
{{- if $pg.managed -}}
  {{- if not (regexMatch "(?:^|/)postgres:18(?:[.-][^[:space:]]+)?$" $pg.image.reference) -}}{{ fail "managed PostgreSQL requires a PostgreSQL 18 image" }}{{- end -}}
  {{- if or (empty $pg.auth.existingSecret) (ne $pg.auth.usernameKey "username") (ne $pg.auth.passwordKey "password") (ne $pg.auth.databaseKey "database") -}}{{ fail "PostgreSQL requires the exact existing Secret key contract" }}{{- end -}}
  {{- if ne (int $pg.service.port) 5432 -}}{{ fail "PostgreSQL service port is locked to 5432" }}{{- end -}}
  {{- if or (not $pg.storage.keepPVC) (not (deepEqual $pg.storage.accessModes (list "ReadWriteOnce"))) (empty $pg.storage.requestedSize) -}}{{ fail "PostgreSQL requires a retained RWO PVC" }}{{- end -}}
  {{- if or (lt (int $pg.database.maxConnections) 20) (gt (int $pg.database.maxConnections) 500) (ne $pg.database.checkpointCompletionTarget "0.9") (ne $pg.database.walCompression "pglz") -}}{{ fail "PostgreSQL managed configuration is outside the bounded profile" }}{{- end -}}
{{- end -}}
{{- end -}}
