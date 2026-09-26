{{- define "cert-manager-substrate.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cert-manager-substrate.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "cert-manager-substrate.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cert-manager-substrate.labels" -}}
app.kubernetes.io/name: {{ include "cert-manager-substrate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "cert-manager-substrate.platformRelease" -}}
{{- $configured := dig "internalTLS" "platformRelease" "" (.Values.global | default (dict)) -}}
{{- $configured | default (trimSuffix "-cert-manager" .Release.Name) -}}
{{- end -}}

{{- define "cert-manager-substrate.internalTLSIssuer" -}}
{{- dig "internalTLS" "issuerName" "internal-ca" (.Values.global | default (dict)) -}}
{{- end -}}

# HOR-528: the internal CA identity is one shared contract, not a per-chart value.
# The ordered companion and the platform's cert-issuers subchart both render the
# root Certificate from the same values files, resolved in this order:
# global.internalTLS.ca.* (the shared contract), then cert-issuers.internal.ca.*
# (its pre-HOR-528 location, still honored so existing overlays keep working),
# then the chart default. Because both writers resolve one identity, the platform
# adopts the bootstrapped root without changing a field, so a reconcile cannot
# make cert-manager re-issue (rotate) it behind running workloads.
{{- define "cert-manager-substrate.caCommonName" -}}
{{- $global := dig "internalTLS" "ca" (dict) (.Values.global | default (dict)) -}}
{{- $legacy := dig "internal" "ca" (dict) (default (dict) (index .Values "cert-issuers")) -}}
{{- dig "commonName" (dig "commonName" "iterabase-internal-ca" $legacy) $global -}}
{{- end -}}

{{- define "cert-manager-substrate.caDuration" -}}
{{- $global := dig "internalTLS" "ca" (dict) (.Values.global | default (dict)) -}}
{{- $legacy := dig "internal" "ca" (dict) (default (dict) (index .Values "cert-issuers")) -}}
{{- dig "duration" (dig "duration" "87600h" $legacy) $global -}}
{{- end -}}

{{- define "cert-manager-substrate.validate" -}}
{{- if dig "internalTLS" "enabled" false (.Values.global | default (dict)) -}}
{{- $platformRelease := include "cert-manager-substrate.platformRelease" . -}}
{{- if eq $platformRelease "" -}}{{- fail "global.internalTLS.platformRelease could not be derived" -}}{{- end -}}
{{- if ne (include "cert-manager-substrate.internalTLSIssuer" .) "internal-ca" -}}{{- fail "the Iterabase internal CA ClusterIssuer name must remain internal-ca" -}}{{- end -}}
{{- if or (eq (include "cert-manager-substrate.caCommonName" .) "") (eq (include "cert-manager-substrate.caDuration" .) "") -}}{{- fail "global.internalTLS.ca.commonName and global.internalTLS.ca.duration must be non-empty" -}}{{- end -}}
{{- if or (ne .Values.internalCABootstrap.image.repository "docker.io/alpine/k8s") (ne (toString .Values.internalCABootstrap.image.tag) "1.34.1") (ne .Values.internalCABootstrap.image.digest "sha256:ec714df3813b5405292860f8a1c55c5727bf8c33c88992f1e981efad8065547f") -}}
{{- fail "internal CA bootstrap image identity is chart-owned" -}}
{{- end -}}
{{- end -}}
{{- end -}}
