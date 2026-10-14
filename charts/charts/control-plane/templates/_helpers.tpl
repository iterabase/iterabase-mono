{{- define "control-plane.name" -}}
{{- printf "%s-control-plane" .Release.Name -}}
{{- end -}}

{{- define "control-plane.managerName" -}}
{{- printf "%s-control-plane-manager" .Release.Name -}}
{{- end -}}

{{- define "control-plane.apiName" -}}
{{- printf "%s-control-plane-api" .Release.Name -}}
{{- end -}}

{{- define "control-plane.gatewayName" -}}
{{- printf "%s-control-plane-gateway" .Release.Name -}}
{{- end -}}

{{- define "control-plane.dispatchName" -}}
{{- printf "%s-control-plane-dispatch" .Release.Name -}}
{{- end -}}

{{- define "control-plane.dispatchTLSSecretName" -}}
{{- default (printf "%s-control-plane-dispatch-tls" .Release.Name) .Values.dispatch.tls.serverSecret -}}
{{- end -}}

{{- define "control-plane.serviceAccountName" -}}
{{- printf "%s-control-plane-manager" .Release.Name -}}
{{- end -}}

{{- define "control-plane.gatewayServiceAccountName" -}}
{{- printf "%s-control-plane-gateway" .Release.Name -}}
{{- end -}}

{{- define "control-plane.dispatchServiceAccountName" -}}
{{- printf "%s-control-plane-dispatch" .Release.Name -}}
{{- end -}}

{{- define "control-plane.toolRunnerName" -}}
{{- printf "%s-tool-runner" .Release.Name -}}
{{- end -}}

{{- define "control-plane.toolRunnerTLSSecretName" -}}
{{- printf "%s-tool-runner-tls" .Release.Name -}}
{{- end -}}

{{- define "control-plane.pgHost" -}}
{{- if .Values.postgresql.host -}}{{- .Values.postgresql.host -}}{{- else -}}{{- printf "%s-postgresql" .Release.Name -}}{{- end -}}
{{- end -}}

{{- define "control-plane.pgSecret" -}}
{{- if .Values.postgresql.passwordSecret -}}{{- .Values.postgresql.passwordSecret -}}{{- else -}}{{- printf "%s-postgresql" .Release.Name -}}{{- end -}}
{{- end -}}

{{- define "control-plane.databaseURL" -}}
{{- $ssl := "disable" -}}
{{- if (or .Values.tls.enabled (dig "internalTLS" "enabled" false (.Values.global | default (dict)))) -}}{{- $ssl = printf "verify-full&sslrootcert=%s" .Values.tls.caMountPath -}}{{- end -}}
postgres://{{ .Values.postgresql.auth.username }}:$(PGPASSWORD)@{{ include "control-plane.pgHost" . }}:{{ .Values.postgresql.port }}/{{ .Values.postgresql.auth.database }}?sslmode={{ $ssl }}
{{- end -}}

{{- define "control-plane.artifactEndpoint" -}}
{{- default (printf "%s-minio:9000" .Release.Name) .Values.artifact.endpoint -}}
{{- end -}}

{{- /* Zone credential Secrets (DES-HOR-460-02). The defaults match the MinIO
     subchart's generated <release>-minio-artifact-<zone> Secrets. */ -}}
{{- define "control-plane.artifactIngestSecretName" -}}
{{- default (printf "%s-minio-artifact-ingest" .Release.Name) .Values.artifact.credentialSecrets.ingest -}}
{{- end -}}

{{- define "control-plane.artifactServeSecretName" -}}
{{- default (printf "%s-minio-artifact-serve" .Release.Name) .Values.artifact.credentialSecrets.serve -}}
{{- end -}}

{{- define "control-plane.artifactLifecycleSecretName" -}}
{{- default (printf "%s-minio-artifact-lifecycle" .Release.Name) .Values.artifact.credentialSecrets.lifecycle -}}
{{- end -}}

{{- /* "true" when the V2 artifact processing pipeline (coordinator, scanner,
     signature updater) renders. */ -}}
{{- define "control-plane.artifactProcessingEnabled" -}}
{{- if and .Values.enabled .Values.artifact.enabled .Values.artifact.processing.enabled -}}true{{- end -}}
{{- end -}}

{{- define "control-plane.artifactCoordinatorName" -}}
{{- printf "%s-control-plane-artifact-coordinator" .Release.Name -}}
{{- end -}}

{{- define "control-plane.artifactScannerName" -}}
{{- printf "%s-control-plane-artifact-scanner" .Release.Name -}}
{{- end -}}

{{- define "control-plane.artifactSignatureUpdaterName" -}}
{{- printf "%s-control-plane-artifact-signature-updater" .Release.Name -}}
{{- end -}}

{{- define "control-plane.artifactSignaturesClaimName" -}}
{{- printf "%s-control-plane-artifact-signatures" .Release.Name -}}
{{- end -}}

{{- define "control-plane.artifactScannerSPIFFEID" -}}
{{- printf "spiffe://%s/control-plane/artifact-scanner" .Values.gateway.trustDomain -}}
{{- end -}}

{{- define "control-plane.artifactCoordinatorSPIFFEID" -}}
{{- printf "spiffe://%s/control-plane/artifact-coordinator" .Values.gateway.trustDomain -}}
{{- end -}}

{{- define "control-plane.artifactCustomSignaturesConfigMap" -}}
{{- if .Values.artifact.processing.signatures.customDatabasesConfigMap -}}
{{- .Values.artifact.processing.signatures.customDatabasesConfigMap -}}
{{- else if .Values.artifact.processing.signatures.customDatabases -}}
{{- printf "%s-control-plane-artifact-custom-signatures" .Release.Name -}}
{{- end -}}
{{- end -}}

{{- /* Shared object-store location for every artifact credential holder. */ -}}
{{- define "control-plane.artifactStoreEnv" -}}
- name: ARTIFACT_ENDPOINT
  value: {{ include "control-plane.artifactEndpoint" . | quote }}
- name: ARTIFACT_BUCKET
  value: {{ .Values.artifact.bucket | quote }}
- name: ARTIFACT_SECURE
  value: {{ .Values.artifact.secure | quote }}
- name: ARTIFACT_QUARANTINE_MAX_BYTES
  value: {{ printf "%d" (int64 .Values.artifact.quarantineMaxBytes) | quote }}
{{- with .Values.artifact.defaultRetention }}
- name: ARTIFACT_DEFAULT_RETENTION
  value: {{ . | quote }}
{{- end }}
{{- end -}}

{{- /* API and gateway: the ingest (quarantine write) and serve (cleared read)
     credentials only. They never receive the lifecycle credential; the
     control-plane config refuses to start if they do. */ -}}
{{- define "control-plane.artifactEnv" -}}
- name: ARTIFACT_ENABLED
  value: {{ .Values.artifact.enabled | quote }}
{{- if .Values.artifact.enabled }}
{{- include "control-plane.artifactStoreEnv" . | nindent 0 }}
- name: ARTIFACT_INGEST_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "control-plane.artifactIngestSecretName" . }}
      key: accessKey
- name: ARTIFACT_INGEST_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "control-plane.artifactIngestSecretName" . }}
      key: secretKey
- name: ARTIFACT_SERVE_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "control-plane.artifactServeSecretName" . }}
      key: accessKey
- name: ARTIFACT_SERVE_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "control-plane.artifactServeSecretName" . }}
      key: secretKey
- name: ARTIFACT_MAX_CONCURRENT_UPLOADS
  value: {{ .Values.artifact.maxConcurrentUploads | quote }}
{{- end }}
{{- end -}}

{{- define "control-plane.gatewayConfig" -}}
gateway:
  inline_limit: {{ .Values.gateway.inlineLimit }}
  {{- if .Values.toolRunner.enabled }}
  approved_runners:
    - namespace: {{ .Release.Namespace | quote }}
      runner_id: {{ .Values.toolRunner.runnerId | quote }}
      spiffe_id: {{ printf "spiffe://%s/tool-runners/%s/%s" .Values.gateway.trustDomain .Release.Namespace .Values.toolRunner.runnerId | quote }}
      allowed_tool_namespaces:
        {{- toYaml (required "toolRunner.allowedToolNamespaces is required" .Values.toolRunner.allowedToolNamespaces) | nindent 8 }}
  {{- end }}
{{- end -}}

{{- define "control-plane.gatewayTLSSecretName" -}}
{{- default (printf "%s-control-plane-gateway-tls" .Release.Name) .Values.gateway.tls.serverSecret -}}
{{- end -}}

{{- define "control-plane.gatewayCASecretName" -}}
{{- default (printf "%s-control-plane-gateway-ca" .Release.Name) .Values.gateway.tls.clientCASecret -}}
{{- end -}}

{{- define "control-plane.spiffeCASecretName" -}}
{{- include "control-plane.gatewayCASecretName" . -}}
{{- end -}}

{{- define "control-plane.apiTLSSecretName" -}}
{{- printf "%s-control-plane-api-tls" .Release.Name -}}
{{- end -}}

{{- /* The internal CA root Secret name. Local override -> global -> the
     <release>-internal-ca-root convention (what cert-issuers creates), so the
     overlay never hardcodes the release name. */ -}}
{{- define "control-plane.tlsCASecretName" -}}
{{- .Values.tls.caSecretName | default (dig "internalTLS" "caSecretName" "" (.Values.global | default (dict))) | default (printf "%s-internal-ca-root" .Release.Name) -}}
{{- end -}}

{{- define "control-plane.authBootstrapSecretName" -}}
{{- default (printf "%s-control-plane-auth-bootstrap" .Release.Name) .Values.auth.bootstrap.existingSecret -}}
{{- end -}}

{{- define "control-plane.authEmailSecretName" -}}
{{- default (printf "%s-control-plane-auth-email" .Release.Name) .Values.auth.email.existingSecret -}}
{{- end -}}

{{- define "control-plane.authSMTPCAMountPath" -}}
/etc/control-plane/smtp-ca
{{- end -}}

{{/*
The authentication-email relay host. The `ses` provider uses the Amazon SES
SMTP interface of the configured region unless a host is set explicitly.
*/}}
{{- define "control-plane.authSMTPHost" -}}
{{- if .Values.auth.email.host -}}
{{- .Values.auth.email.host -}}
{{- else if eq (toString .Values.auth.email.provider) "ses" -}}
{{- printf "email-smtp.%s.amazonaws.com" (required "auth.email.ses.region is required for the ses provider" .Values.auth.email.ses.region) -}}
{{- end -}}
{{- end -}}

{{- define "control-plane.authGeoIPMountPath" -}}
/etc/control-plane/geoip
{{- end -}}

{{- define "control-plane.authGeoIPDatabase" -}}
{{- if .Values.auth.geoip.databasePath -}}
{{- .Values.auth.geoip.databasePath -}}
{{- else if .Values.auth.geoip.existingClaim -}}
{{- printf "%s/%s" (include "control-plane.authGeoIPMountPath" .) (.Values.auth.geoip.databaseFile | default "GeoLite2-City.mmdb") -}}
{{- end -}}
{{- end -}}

# Browser-authentication environment shared by the bootstrap init container and
# the api container. Sign-in is always on (HOR-582): rendering fails closed when
# any approved prerequisite is missing.
{{- define "control-plane.authEnv" -}}
{{- if not (has (toString .Values.auth.email.provider) (list "smtp" "ses")) }}
{{- fail "auth.email.provider must be smtp or ses" }}
{{- end }}
{{- if and (eq (toString .Values.auth.email.provider) "ses") (not .Values.auth.email.username) }}
{{- fail "auth.email.username and auth.email.existingSecret are required for the ses provider: SES SMTP credentials are per environment" }}
{{- end }}
{{- if not (has .Values.auth.email.mode (list "starttls" "tls")) }}
{{- fail "auth.email.mode must be starttls or tls (verified TLS is required)" }}
{{- end }}
{{- if not (regexMatch "^https://[^/?#]+$" (toString .Values.auth.publicOrigin)) }}
{{- fail "auth.publicOrigin must be an absolute https origin without a path, query, or fragment" }}
{{- end }}
- name: AUTH_PUBLIC_ORIGIN
  value: {{ required "auth.publicOrigin is required: sign-in is always on" .Values.auth.publicOrigin | quote }}
- name: AUTH_FORWARDED_HEADER
  value: {{ .Values.auth.forwardedHeader | default "X-Forwarded-For" | quote }}
- name: AUTH_SMTP_HOST
  value: {{ required "auth.email.host is required (or auth.email.provider=ses)" (include "control-plane.authSMTPHost" .) | quote }}
- name: AUTH_SMTP_PORT
  value: {{ .Values.auth.email.port | quote }}
- name: AUTH_SMTP_MODE
  value: {{ .Values.auth.email.mode | quote }}
- name: AUTH_SMTP_FROM
  value: {{ required "auth.email.from is required: sign-in is always on" .Values.auth.email.from | quote }}
{{- if .Values.auth.email.username }}
{{- if not .Values.auth.email.existingSecret }}
{{- fail "auth.email.existingSecret is required when auth.email.username is set: the SMTP relay password is operator-owned and must not be generated" }}
{{- end }}
- name: AUTH_SMTP_USERNAME
  value: {{ .Values.auth.email.username | quote }}
- name: AUTH_SMTP_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "control-plane.authEmailSecretName" . }}
      key: {{ .Values.auth.email.passwordKey | default "smtp-password" }}
{{- end }}
{{- if .Values.auth.email.caSecret }}
- name: AUTH_SMTP_CA_FILE
  value: {{ printf "%s/%s" (include "control-plane.authSMTPCAMountPath" .) (.Values.auth.email.caKey | default "ca.crt") | quote }}
{{- end }}
{{- if .Values.auth.session.idleTTL }}
- name: AUTH_SESSION_IDLE_TTL
  value: {{ .Values.auth.session.idleTTL | quote }}
{{- end }}
{{- if .Values.auth.session.absoluteTTL }}
- name: AUTH_SESSION_ABSOLUTE_TTL
  value: {{ .Values.auth.session.absoluteTTL | quote }}
{{- end }}
{{- if .Values.auth.session.recentAuthTTL }}
- name: AUTH_RECENT_AUTH_TTL
  value: {{ .Values.auth.session.recentAuthTTL | quote }}
{{- end }}
{{- if .Values.auth.trustedProxies }}
- name: AUTH_TRUSTED_PROXIES
  value: {{ join "," .Values.auth.trustedProxies | quote }}
{{- end }}
{{- if or .Values.auth.geoip.databasePath .Values.auth.geoip.existingClaim }}
- name: AUTH_GEOIP_DATABASE
  value: {{ include "control-plane.authGeoIPDatabase" . | quote }}
{{- end }}
- name: AUTH_BOOTSTRAP_ADMIN_EMAIL
  valueFrom:
    secretKeyRef:
      name: {{ include "control-plane.authBootstrapSecretName" . }}
      key: {{ .Values.auth.bootstrap.emailKey | default "admin-email" }}
- name: AUTH_BOOTSTRAP_ADMIN_LOCALE
  valueFrom:
    secretKeyRef:
      name: {{ include "control-plane.authBootstrapSecretName" . }}
      key: {{ .Values.auth.bootstrap.localeKey | default "admin-locale" }}
{{- end -}}

{{- define "control-plane.labels" -}}
app.kubernetes.io/name: control-plane
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
