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

{{/* LEGACY-DB-LAYOUT: remove after consolidation.
     "true" when this release's in-cluster Postgres predates the one-database
     layout (#2049): its Secret carries the per-store chat-url / sched-url keys
     and no url key, so its volume holds the chat and sched databases. Such a
     release keeps its two databases on upgrade (an upgrade never strands
     data); a new install gets ONE database, "fleet", and DATABASE_URL. */}}
{{- define "fleet.postgresLegacyLayout" -}}
{{- $s := lookup "v1" "Secret" .Release.Namespace (printf "%s-postgres" (include "fleet.fullname" .)) -}}
{{- if $s -}}
{{- with $s.data -}}
{{- if and (hasKey . "chat-url") (not (hasKey . "url")) -}}true{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The release tag both images default to (ADR-0098): image.tag when set, else
"v" + the chart's appVersion, which `make helm-package` (and release.yml, for
the chart attached to every release) stamps with the release the chart came
from. A chart installed straight from a checkout is unstamped (appVersion
0.0.0) and names no release, so it must be told which one.
*/}}
{{- define "fleet.releaseTag" -}}
{{- if .Values.image.tag -}}
{{- .Values.image.tag -}}
{{- else if and .Chart.AppVersion (ne .Chart.AppVersion "0.0.0") -}}
{{- printf "v%s" .Chart.AppVersion -}}
{{- else -}}
{{- fail "image.tag is required: this chart is unstamped (installed from a checkout, appVersion 0.0.0). Set image.tag to a release tag (vYYYY.MM.DD.N, see https://github.com/ElcanoTek/fleet/releases), or install the chart attached to that release, which defaults to its own images." -}}
{{- end -}}
{{- end -}}

{{/* The control-plane image: the published ghcr.io/elcanotek/fleet by default. */}}
{{- define "fleet.image" -}}
{{- printf "%s:%s" (required "image.repository must not be empty" .Values.image.repository) (include "fleet.releaseTag" .) -}}
{{- end -}}

{{/*
The sandbox image: sandbox.image verbatim when set (a client bundle's own
image), else the published sandbox repository at the same release tag as the
control plane — the pair release.yml built together.
*/}}
{{- define "fleet.sandboxImage" -}}
{{- if .Values.sandbox.image -}}
{{- .Values.sandbox.image -}}
{{- else -}}
{{- printf "%s:%s" (required "sandbox.imageRepository must not be empty when sandbox.image is unset" .Values.sandbox.imageRepository) (include "fleet.releaseTag" .) -}}
{{- end -}}
{{- end -}}
