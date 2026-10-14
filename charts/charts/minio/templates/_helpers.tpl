{{- define "minio.name" -}}
{{- printf "%s-minio" .Release.Name -}}
{{- end -}}

{{- define "minio.secretName" -}}
{{- printf "%s-minio" .Release.Name -}}
{{- end -}}

{{- /* Zone credential Secret names (DES-HOR-460-02). The defaults are a
     cross-chart contract: the control-plane chart derives the same
     <release>-minio-artifact-<zone> names. */ -}}
{{- define "minio.artifactIngestSecretName" -}}
{{- default (printf "%s-minio-artifact-ingest" .Release.Name) .Values.artifactService.credentialSecrets.ingest -}}
{{- end -}}

{{- define "minio.artifactServeSecretName" -}}
{{- default (printf "%s-minio-artifact-serve" .Release.Name) .Values.artifactService.credentialSecrets.serve -}}
{{- end -}}

{{- define "minio.artifactLifecycleSecretName" -}}
{{- default (printf "%s-minio-artifact-lifecycle" .Release.Name) .Values.artifactService.credentialSecrets.lifecycle -}}
{{- end -}}

{{- /* Canonical zone policy (files/policies/artifact-<zone>.json, shared with
     the control-plane MinIO integration test) bound to the configured bucket. */ -}}
{{- define "minio.artifactPolicy" -}}
{{- $bucket := .root.Values.artifactService.bucket -}}
{{- if not (regexMatch "^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$" $bucket) -}}
{{- fail "artifactService.bucket must be a valid S3 bucket name" -}}
{{- end -}}
{{- .root.Files.Get (printf "files/policies/artifact-%s.json" .zone) | replace "__BUCKET__" $bucket -}}
{{- end -}}

{{- define "minio.artifactLabels" -}}
app.kubernetes.io/name: minio
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: artifact-provisioner
{{- end -}}

{{- define "minio.labels" -}}
app.kubernetes.io/name: minio
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: object-store
{{- end -}}
