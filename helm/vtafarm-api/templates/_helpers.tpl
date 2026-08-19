{{- define "app.labels" -}}
app: {{ .Values.name }}
{{- end }}

{{- define "app.frontendHost" -}}
{{ required "frontendHost is required - WebAuthn and CORS both derive from it" .Values.frontendHost }}
{{- end }}

{{- define "app.frontendOrigin" -}}
https://{{ include "app.frontendHost" . }}
{{- end }}
