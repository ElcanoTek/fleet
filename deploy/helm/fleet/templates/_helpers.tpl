{{/* Chart name + fullname, standard helpers. */}}
{{- define "fleet.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fleet.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "fleet.labels" -}}
app.kubernetes.io/name: {{ include "fleet.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "fleet.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fleet.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "fleet.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "fleet.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Namespace sandbox pods run in: explicit value or the release namespace. */}}
{{- define "fleet.sandboxNamespace" -}}
{{- default .Release.Namespace .Values.sandbox.kubernetes.namespace -}}
{{- end -}}

{{/* In-cluster Postgres endpoints (postgres.enabled=true only). */}}
{{- define "fleet.postgresHost" -}}
{{- printf "%s-postgres" (include "fleet.fullname" .) -}}
{{- end -}}
