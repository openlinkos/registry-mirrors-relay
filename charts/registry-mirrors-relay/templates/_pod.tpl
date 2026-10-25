{{/* The Pod template shared by the Deployment (readonly) and the StatefulSet (writable); its security settings match the kustomize base */}}
{{- define "rmr.podTemplate" -}}
metadata:
  labels:
    {{- include "rmr.selectorLabels" . | nindent 4 }}
    {{- with .Values.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  annotations:
    checksum/config: {{ include "rmr.configChecksum" . }}
    {{- with .Values.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  serviceAccountName: {{ include "rmr.serviceAccountName" . }}
  # the relay does not call the Kubernetes API
  automountServiceAccountToken: false
  enableServiceLinks: false
  {{- with .Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.priorityClassName }}
  priorityClassName: {{ . }}
  {{- end }}
  terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}
  securityContext:
    {{- toYaml .Values.podSecurityContext | nindent 4 }}
  {{- with .Values.topologySpreadConstraints }}
  {{- $constraints := list }}
  {{- range . }}
  {{- $c := deepCopy . }}
  {{- if not (index $c "labelSelector") }}
  {{- $_ := set $c "labelSelector" (dict "matchLabels" (fromYaml (include "rmr.selectorLabels" $))) }}
  {{- end }}
  {{- $constraints = append $constraints $c }}
  {{- end }}
  topologySpreadConstraints:
    {{- toYaml $constraints | nindent 4 }}
  {{- end }}
  {{- with .Values.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  containers:
    - name: relay
      image: {{ include "rmr.image" . }}
      imagePullPolicy: {{ .Values.image.pullPolicy }}
      ports:
        - name: http
          containerPort: {{ .Values.containerPorts.http }}
        {{- if .Values.admin.separatePort }}
        - name: admin
          containerPort: {{ .Values.containerPorts.admin }}
        {{- end }}
      {{- if or .Values.admin.existingSecret .Values.extraEnv }}
      env:
        {{- if .Values.admin.existingSecret }}
        - name: RELAY_ADMIN_PASSWORD_FILE
          value: /run/secrets/relay-admin/password
        - name: RELAY_ADMIN_USERNAME
          value: {{ .Values.admin.username | quote }}
        {{- end }}
        {{- with .Values.extraEnv }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- end }}
      {{- with .Values.extraEnvFrom }}
      envFrom:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.readinessProbe }}
      readinessProbe:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.livenessProbe }}
      livenessProbe:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.startupProbe }}
      startupProbe:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- if .Values.preStopSleepSeconds }}
      lifecycle:
        # wait for the Service to remove this Pod before starting to exit
        preStop:
          sleep:
            seconds: {{ .Values.preStopSleepSeconds }}
      {{- end }}
      resources:
        {{- toYaml .Values.resources | nindent 8 }}
      securityContext:
        {{- toYaml .Values.securityContext | nindent 8 }}
      volumeMounts:
        - name: config
          mountPath: /etc/registry-mirrors-relay
          readOnly: true
        - name: data
          mountPath: /var/lib/registry-mirrors-relay
        {{- if .Values.admin.existingSecret }}
        - name: admin-password
          mountPath: /run/secrets/relay-admin
          readOnly: true
        {{- end }}
        {{- with .Values.extraVolumeMounts }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
  volumes:
    # the whole directory is mounted (no subPath), so the relay reloads automatically when the ConfigMap changes
    - name: config
      configMap:
        name: {{ include "rmr.fullname" . }}-config
    {{- if not (and (eq .Values.mode "writable") .Values.persistence.enabled) }}
    - name: data
      emptyDir:
        sizeLimit: {{ .Values.dataDir.sizeLimit }}
    {{- end }}
    {{- if .Values.admin.existingSecret }}
    # optional: this Secret can be deleted once the first administrator has been created
    - name: admin-password
      secret:
        secretName: {{ .Values.admin.existingSecret }}
        optional: true
        defaultMode: 0440
        items:
          - key: {{ .Values.admin.existingSecretKey }}
            path: password
    {{- end }}
    {{- with .Values.extraVolumes }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end -}}
