{{/* Standard name helpers. */}}

{{- define "webhook-relay.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "webhook-relay.fullname" -}}
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

{{- define "webhook-relay.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "webhook-relay.labels" -}}
helm.sh/chart: {{ include "webhook-relay.chart" . }}
{{ include "webhook-relay.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: webhook-relay
{{- end }}

{{- define "webhook-relay.selectorLabels" -}}
app.kubernetes.io/name: {{ include "webhook-relay.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Component labels. The component label is what the two Deployments' selectors
differ on, and what the NetworkPolicies use to tell api from worker — they
share every other label because they are the same application.
*/}}
{{- define "webhook-relay.componentLabels" -}}
{{ include "webhook-relay.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "webhook-relay.componentSelectorLabels" -}}
{{ include "webhook-relay.selectorLabels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "webhook-relay.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "webhook-relay.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The image reference.

Fails loudly when no tag is set rather than silently defaulting to "latest".
A mutable tag means two pods claiming the same version can be running different
code, and a rollback has nothing to roll back to — so an unset tag is a
deployment bug, not something to paper over.
*/}}
{{- define "webhook-relay.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- if or (not $tag) (eq $tag "latest") }}
{{- fail "image.tag must be set to an immutable tag (a git SHA). 'latest' is refused: it makes rollback impossible and lets two pods claiming one version run different code." }}
{{- end }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
The Postgres connection string, assembled from the secret at runtime.

Built in the container's shell from environment variables rather than baked
into a single DATABASE_URL secret key, so that rotating the password is a
secret update rather than a re-templating of a composite string — and so the
password never appears in the pod spec, only in the env var reference.
*/}}
{{- define "webhook-relay.databaseUrlEnv" -}}
- name: DB_USER
  valueFrom:
    secretKeyRef:
      name: {{ .Values.config.database.existingSecret }}
      key: {{ .Values.config.database.usernameKey }}
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.config.database.existingSecret }}
      key: {{ .Values.config.database.passwordKey }}
- name: DATABASE_URL
  value: "postgres://$(DB_USER):$(DB_PASSWORD)@{{ .Values.config.database.host }}:{{ .Values.config.database.port }}/{{ .Values.config.database.name }}?sslmode={{ .Values.config.database.sslMode }}"
{{- end }}

{{/* Environment shared by api and worker. */}}
{{- define "webhook-relay.commonEnv" -}}
{{ include "webhook-relay.databaseUrlEnv" . }}
- name: VALKEY_URL
  value: "redis://{{ .Values.config.valkey.host }}:{{ .Values.config.valkey.port }}"
- name: LOG_LEVEL
  value: {{ .Values.config.logLevel | quote }}
- name: SERVICE_VERSION
  value: {{ .Values.config.serviceVersion | default (.Values.image.tag | default .Chart.AppVersion) | quote }}
- name: OTLP_ENDPOINT
  value: {{ .Values.config.otlp.endpoint | quote }}
- name: TRACING_ENABLED
  value: {{ .Values.config.otlp.tracingEnabled | quote }}
- name: TRACE_SAMPLE_RATIO
  value: {{ .Values.config.otlp.sampleRatio | quote }}
- name: DELIVERY_TIMEOUT
  value: {{ .Values.config.delivery.timeout | quote }}
- name: MAX_ATTEMPTS
  value: {{ .Values.config.delivery.maxAttempts | quote }}
- name: RETRY_BASE_DELAY
  value: {{ .Values.config.delivery.retryBaseDelay | quote }}
- name: RETRY_MAX_DELAY
  value: {{ .Values.config.delivery.retryMaxDelay | quote }}
- name: STALE_CLAIM_TIMEOUT
  value: {{ .Values.config.delivery.staleClaimTimeout | quote }}
- name: DELIVERY_LEASE
  value: {{ .Values.config.delivery.deliveryLease | quote }}
- name: DELIVERY_DEDUP_ENABLED
  value: {{ .Values.config.delivery.dedupEnabled | quote }}
- name: RATE_LIMIT_ENABLED
  value: {{ .Values.config.resilience.rateLimitEnabled | quote }}
- name: BREAKER_ENABLED
  value: {{ .Values.config.resilience.breakerEnabled | quote }}
- name: BREAKER_THRESHOLD
  value: {{ .Values.config.resilience.breakerThreshold | quote }}
- name: BREAKER_COOLDOWN
  value: {{ .Values.config.resilience.breakerCooldown | quote }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
{{- end }}

{{/*
terminationGracePeriodSeconds.

Derived, never hand-set, because it must exceed preStop delay + drain timeout.
Getting this wrong is silent: the pod looks like it shut down cleanly and the
kubelet SIGKILLs it mid-delivery, leaving an unacked queue entry that is
redelivered later as a duplicate.
*/}}
{{- define "webhook-relay.terminationGracePeriod" -}}
{{- $drain := .Values.worker.drainTimeout | trimSuffix "s" | int -}}
{{- $preStop := .Values.lifecycle.preStopDelaySeconds | int -}}
{{- add $drain $preStop 10 -}}
{{- end }}
