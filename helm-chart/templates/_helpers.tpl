{{- define "llm-mgmt.name" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "llm-mgmt.selectorLabels" -}}
app.kubernetes.io/name: llm-management-api
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "llm-mgmt.labels" -}}
{{ include "llm-mgmt.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.version | default .Chart.AppVersion | quote }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "llm-mgmt.secretName" -}}
{{- default (include "llm-mgmt.name" .) .Values.existingSecret -}}
{{- end -}}
