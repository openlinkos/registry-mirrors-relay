{{/* 名字：和常见 chart 一样，release 名里已经含有 chart 名时不再重复 */}}
{{- define "rmr.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "rmr.fullname" -}}
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

{{/* selector 只用 name 和 instance：Deployment、StatefulSet 的 selector 建好后不能改 */}}
{{- define "rmr.selectorLabels" -}}
app.kubernetes.io/name: {{ include "rmr.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "rmr.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "rmr.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* readonly 模式的副本数：没写时是 2 */}}
{{- define "rmr.replicas" -}}
{{- .Values.replicaCount | default 2 | int -}}
{{- end -}}

{{- define "rmr.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "rmr.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
镜像：image.digest 优先；其次 image.tag；都没写时用 appVersion，并带上发布时记在 Chart 注解里的 digest。
用户自己写了 tag 时不附加 digest，免得改了 tag 却悄悄拉旧镜像。
*/}}
{{- define "rmr.image" -}}
{{- $repo := .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repo .Values.image.digest -}}
{{- else if .Values.image.tag -}}
{{- printf "%s:%s" $repo .Values.image.tag -}}
{{- else -}}
{{- $digest := index (.Chart.Annotations | default dict) "openlinkos.org/image-digest" | default "" -}}
{{- if $digest -}}
{{- printf "%s:%s@%s" $repo .Chart.AppVersion $digest -}}
{{- else -}}
{{- printf "%s:%s" $repo .Chart.AppVersion -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* 拒绝 chart 管不了或不安全的写法 */}}
{{- define "rmr.validate" -}}
{{- $c := .Values.config | default dict -}}
{{- if not (has .Values.mode (list "readonly" "writable")) -}}
{{- fail (printf "mode must be readonly or writable, not %q" .Values.mode) -}}
{{- end -}}
{{- if not (has .Values.configRollout (list "always" "restartOnly")) -}}
{{- fail (printf "configRollout must be always or restartOnly, not %q" .Values.configRollout) -}}
{{- end -}}
{{- if and (eq .Values.mode "writable") .Values.replicaCount (gt (int .Values.replicaCount) 1) -}}
{{- fail "mode writable runs a single replica: its operational database cannot be shared; set replicaCount to 1 or use mode readonly" -}}
{{- end -}}
{{- range $k := list "data_dir" "listen" "tls" -}}
{{- if hasKey $c $k -}}
{{- fail (printf "config.%s is set by the chart (or not supported in Kubernetes); remove it from config" $k) -}}
{{- end -}}
{{- end -}}
{{- $admin := index $c "admin" | default dict -}}
{{- if hasKey $admin "store" -}}
{{- fail "config.admin.store is set by mode; remove it from config" -}}
{{- end -}}
{{- with index $admin "initial_admin" -}}
{{- if $.Values.admin.existingSecret -}}
{{- fail "set the first administrator either with admin.existingSecret or with config.admin.initial_admin, not both" -}}
{{- end -}}
{{- if hasKey (index . "password" | default dict) "value" -}}
{{- fail "config.admin.initial_admin.password must come from env or file, not value" -}}
{{- end -}}
{{- end -}}
{{- range $t := index $admin "api_tokens" | default list -}}
{{- if hasKey (index $t "token" | default dict) "value" -}}
{{- fail "config.admin.api_tokens[].token must come from env or file, not value" -}}
{{- end -}}
{{- end -}}
{{- range $name, $u := index $c "upstreams" | default dict -}}
{{- with index ($u | default dict) "credentials" -}}
{{- if hasKey (index . "password" | default dict) "value" -}}
{{- fail (printf "config.upstreams.%s.credentials.password must come from env or file, not value" $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range $e := index $c "entrances" | default list -}}
{{- if hasKey $e "listen" -}}
{{- fail "entrances[].listen was removed in 0.2.0; route by host name instead" -}}
{{- end -}}
{{- end -}}
{{- if hasKey .Values "adminService" -}}
{{- fail "adminService was removed in 0.2.0: the admin console is on the pull port; set admin.separatePort: true (and admin.service.port) for its own Service" -}}
{{- end -}}
{{- if hasKey .Values.metrics "service" -}}
{{- fail "metrics.service was removed in 0.2.0: set metrics.enabled; metrics are scraped from the pull port, or the admin port with admin.separatePort" -}}
{{- end -}}
{{- if hasKey .Values.containerPorts "metrics" -}}
{{- fail "containerPorts.metrics was removed in 0.2.0: metrics are served on the management entrance" -}}
{{- end -}}
{{- $m := index $c "metrics" | default dict -}}
{{- if and .Values.metrics.serviceMonitor.enabled (hasKey $m "enabled") (not (index $m "enabled")) -}}
{{- fail "metrics.serviceMonitor.enabled needs metrics; config.metrics.enabled is false" -}}
{{- end -}}
{{- if and (not .Values.admin.separatePort) (has .Values.service.type (list "NodePort" "LoadBalancer")) (ne .Values.service.externalTrafficPolicy "Local") -}}
{{- fail (printf "service.type %s without externalTrafficPolicy: Local makes outside clients look like cluster addresses, and the admin console shares the pull port; set service.externalTrafficPolicy: Local or admin.separatePort: true" .Values.service.type) -}}
{{- end -}}
{{- end -}}

{{/* 管理入口（后台、API、指标）在哪个端口：共用拉取端口时是 http */}}
{{- define "rmr.mgmtPort" -}}
{{- ternary "admin" "http" .Values.admin.separatePort -}}
{{- end -}}

{{/* 生效的配置：用户的 config，加上 chart 定下的监听、数据目录、运营数据的存放方式和指标开关 */}}
{{- define "rmr.configMap" -}}
{{- include "rmr.validate" . -}}
{{- $cfg := deepCopy (.Values.config | default dict) -}}
{{- $admin := deepCopy (index $cfg "admin" | default dict) -}}
{{- $_ := set $admin "store" (dict "driver" (ternary "bolt" "none" (eq .Values.mode "writable"))) -}}
{{- $listen := dict "http" (printf ":%v" .Values.containerPorts.http) -}}
{{- if .Values.admin.separatePort -}}
{{- $_ := set $listen "admin" (printf ":%v" .Values.containerPorts.admin) -}}
{{- end -}}
{{- $owned := dict "data_dir" "/var/lib/registry-mirrors-relay" "listen" $listen "admin" $admin -}}
{{- if or .Values.metrics.enabled .Values.metrics.serviceMonitor.enabled -}}
{{- $metrics := deepCopy (index $cfg "metrics" | default dict) -}}
{{- $_ := set $metrics "enabled" true -}}
{{- $_ := set $owned "metrics" $metrics -}}
{{- end -}}
{{- toYaml (mustMergeOverwrite $cfg $owned) -}}
{{- end -}}

{{/*
滚动更新用的 checksum。restartOnly 只看要重启才能生效的字段，和 internal/app/app.go 的 restartNeeded 对应
（listen、tls、data_dir、admin.store、log.access、log.level、timeouts 的 read_header/idle/shutdown_grace、
proxy.buffer_size、limits.tracked_clients）。
*/}}
{{- define "rmr.configChecksum" -}}
{{- $cfg := fromYaml (include "rmr.configMap" .) -}}
{{- if eq .Values.configRollout "restartOnly" -}}
{{- $log := index $cfg "log" | default dict -}}
{{- $limits := index $cfg "limits" | default dict -}}
{{- $timeouts := index $cfg "timeouts" | default dict -}}
{{- $proxy := index $cfg "proxy" | default dict -}}
{{- $keys := dict
  "listen" (index $cfg "listen")
  "data_dir" (index $cfg "data_dir")
  "store" (index (index $cfg "admin") "store")
  "access_log" (index $log "access")
  "log_level" (index $log "level")
  "read_header" (index $timeouts "read_header")
  "idle" (index $timeouts "idle")
  "shutdown_grace" (index $timeouts "shutdown_grace")
  "buffer_size" (index $proxy "buffer_size")
  "tracked_clients" (index $limits "tracked_clients") -}}
{{- toYaml $keys | sha256sum -}}
{{- else -}}
{{- toYaml $cfg | sha256sum -}}
{{- end -}}
{{- end -}}
