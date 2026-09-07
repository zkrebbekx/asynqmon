{{- define "asynqmon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "asynqmon.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "asynqmon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "asynqmon.labels" -}}
helm.sh/chart: {{ include "asynqmon.chart" . }}
{{ include "asynqmon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "asynqmon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "asynqmon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "asynqmon.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "asynqmon.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "asynqmon.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Name of the secret that holds the redis credentials: the user's own secret
when redis.existingSecret is set, otherwise the one this chart renders.
*/}}
{{- define "asynqmon.redisSecretName" -}}
{{- .Values.redis.existingSecret | default (printf "%s-redis" (include "asynqmon.fullname" .)) -}}
{{- end -}}

{{/*
Name of the secret that holds the non-redis application secrets
(prometheus basic auth, hygiene webhook URL).
*/}}
{{- define "asynqmon.appSecretName" -}}
{{- printf "%s-app" (include "asynqmon.fullname" .) -}}
{{- end -}}

{{/*
True when REDIS_URL comes from a secretKeyRef instead of a plain value.
*/}}
{{- define "asynqmon.redisUrlFromSecret" -}}
{{- if and .Values.redis.existingSecret .Values.redis.existingSecretUrlKey -}}true{{- end -}}
{{- end -}}

{{/*
Reject value combinations that render a Deployment which looks correct and
then fails at run time. Every message names the value to change.
*/}}
{{- define "asynqmon.validateValues" -}}
{{- $urlFromSecret := include "asynqmon.redisUrlFromSecret" . -}}
{{- if and .Values.redis.url .Values.redis.password -}}
{{- fail "redis.url and redis.password are mutually exclusive: the binary builds the connection from the URL and never applies REDIS_PASSWORD, so the pod fails with NOAUTH. Put the credentials in redis.url, or use redis.addr with redis.password." -}}
{{- end -}}
{{- if and .Values.redis.url .Values.redis.existingSecret (not $urlFromSecret) -}}
{{- fail "redis.url and redis.existingSecret are mutually exclusive: the binary builds the connection from the URL and never applies REDIS_PASSWORD. Store the whole URL in the secret and set redis.existingSecretUrlKey instead of redis.url." -}}
{{- end -}}
{{- if and .Values.redis.url .Values.redis.existingSecretUrlKey -}}
{{- fail "Set either redis.url or redis.existingSecretUrlKey, not both." -}}
{{- end -}}
{{- if and .Values.redis.existingSecretUrlKey (not .Values.redis.existingSecret) -}}
{{- fail "redis.existingSecretUrlKey needs redis.existingSecret: name the secret that holds the key." -}}
{{- end -}}
{{- if and .Values.redis.existingSecretSentinelPasswordKey (not .Values.redis.existingSecret) -}}
{{- fail "redis.existingSecretSentinelPasswordKey needs redis.existingSecret: name the secret that holds the key." -}}
{{- end -}}
{{- if and .Values.prometheus.existingSecret (not .Values.prometheus.existingSecretBasicAuthKey) -}}
{{- fail "prometheus.existingSecret needs prometheus.existingSecretBasicAuthKey: name the key inside the secret." -}}
{{- end -}}
{{- if and .Values.hygiene.existingSecret (not .Values.hygiene.existingSecretWebhookUrlKey) -}}
{{- fail "hygiene.existingSecret needs hygiene.existingSecretWebhookUrlKey: name the key inside the secret." -}}
{{- end -}}
{{- if and .Values.pdb.enabled .Values.pdb.maxUnavailable .Values.pdb.minAvailable -}}
{{- fail "Set either pdb.minAvailable or pdb.maxUnavailable, not both. Clear pdb.minAvailable to use pdb.maxUnavailable." -}}
{{- end -}}
{{- end -}}
