{{/*
Expand the name of the chart.
*/}}
{{- define "lifecycle-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "lifecycle-controller.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "lifecycle-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "lifecycle-controller.labels" -}}
helm.sh/chart: {{ include "lifecycle-controller.chart" . }}
{{ include "lifecycle-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "lifecycle-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lifecycle-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller-manager
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "lifecycle-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "lifecycle-controller.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Extract a numeric container port from a listener address.
*/}}
{{- define "lifecycle-controller.bindPort" -}}
{{- if not (kindIs "string" .address) -}}
{{- fail (printf "%s must be a string" .key) -}}
{{- end -}}
{{- if not (regexMatch `^(\[[^\[\][:space:]]+\]|[^:\[\][:space:]]*):[0-9]+$` .address) -}}
{{- fail (printf "%s must use :port, host:port, or [IPv6]:port with a numeric port" .key) -}}
{{- end -}}
{{- $digits := regexFind `[0-9]+$` .address -}}
{{- $digits = regexReplaceAll `^0+` $digits "" -}}
{{- if gt (len $digits) 5 -}}
{{- fail (printf "%s port must be between 1 and 65535" .key) -}}
{{- end -}}
{{- $port := int $digits -}}
{{- if or (lt $port 1) (gt $port 65535) -}}
{{- fail (printf "%s port must be between 1 and 65535" .key) -}}
{{- end -}}
{{- $port -}}
{{- end -}}
