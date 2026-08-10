{{- define "kuberploy-local-dependencies.labels" -}}
app.kubernetes.io/name: kuberploy-local-dependencies
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: kuberploy
app.kubernetes.io/managed-by: {{ .Release.Service }}
kuberploy.io/test-run: {{ .Values.testRun | quote }}
{{- end -}}
