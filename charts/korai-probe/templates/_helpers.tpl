{{- define "korai-probe.labels" -}}
app.kubernetes.io/name: korai-probe
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
{{- define "korai-probe.redisSecret" -}}
{{- default (printf "%s-redis" .Release.Name) .Values.redisSlowlog.existingSecret -}}
{{- end -}}
{{- define "korai-probe.secret" -}}
{{- default (printf "%s-token" .Release.Name) .Values.existingSecret -}}
{{- end -}}
