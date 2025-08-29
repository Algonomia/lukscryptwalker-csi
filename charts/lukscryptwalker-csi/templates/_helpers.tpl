{{/*
Expand the name of the chart.
*/}}
{{- define "lukscryptwalker-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "lukscryptwalker-csi.fullname" -}}
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
{{- define "lukscryptwalker-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "lukscryptwalker-csi.labels" -}}
helm.sh/chart: {{ include "lukscryptwalker-csi.chart" . }}
{{ include "lukscryptwalker-csi.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "lukscryptwalker-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lukscryptwalker-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Controller labels
*/}}
{{- define "lukscryptwalker-csi.controller.labels" -}}
{{ include "lukscryptwalker-csi.labels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Controller selector labels
*/}}
{{- define "lukscryptwalker-csi.controller.selectorLabels" -}}
{{ include "lukscryptwalker-csi.selectorLabels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Node labels
*/}}
{{- define "lukscryptwalker-csi.node.labels" -}}
{{ include "lukscryptwalker-csi.labels" . }}
app.kubernetes.io/component: node
{{- end }}

{{/*
Node selector labels
*/}}
{{- define "lukscryptwalker-csi.node.selectorLabels" -}}
{{ include "lukscryptwalker-csi.selectorLabels" . }}
app.kubernetes.io/component: node
{{- end }}

{{/*
Create the name of the controller service account to use
*/}}
{{- define "lukscryptwalker-csi.controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.controller.create }}
{{- default (printf "%s-controller" (include "lukscryptwalker-csi.fullname" .)) .Values.serviceAccount.controller.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.controller.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the node service account to use
*/}}
{{- define "lukscryptwalker-csi.node.serviceAccountName" -}}
{{- if .Values.serviceAccount.node.create }}
{{- default (printf "%s-node" (include "lukscryptwalker-csi.fullname" .)) .Values.serviceAccount.node.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.node.name }}
{{- end }}
{{- end }}

{{/*
Image pull secrets
*/}}
{{- define "lukscryptwalker-csi.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}