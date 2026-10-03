{{/* Use the same resource list for watch flags and permissions. */}}
{{- define "lifecycle-controller.watchResources" -}}
{{- $scope := .Values.controllerManager.scope -}}
{{- if not (kindIs "slice" $scope.watchResources) -}}
  {{- fail "controllerManager.scope.watchResources must be a list" -}}
{{- end -}}
{{- $resources := $scope.watchResources -}}
{{- if not (empty $scope.watchNamespaces) -}}
  {{- $resources = without $resources "persistentvolumes" -}}
  {{- if and (not (empty $scope.watchResources)) (empty $resources) -}}
    {{- fail "namespace filters exclude PersistentVolumes, leaving no resources to watch" -}}
  {{- end -}}
{{- end -}}
{{- toJson $resources -}}
{{- end -}}

{{/* Derive permission scope from namespace filters. */}}
{{- define "lifecycle-controller.rbacScope" -}}
{{- $scope := .Values.controllerManager.scope -}}
{{- $exact := not (empty $scope.watchNamespaces) -}}
{{- range $scope.watchNamespaces -}}
  {{- if regexMatch "[*?\\[\\]\\\\]" . -}}
    {{- $exact = false -}}
  {{- end -}}
{{- end -}}
{{- if $exact -}}
  {{- $namespaces := include "lifecycle-controller.rbacNamespaces" . -}}
{{- end -}}
{{- ternary "namespaced" "cluster" $exact -}}
{{- end -}}

{{/* Do not silently broaden grants when an ignore pattern cannot be represented. */}}
{{- define "lifecycle-controller.rbacNamespaces" -}}
{{- $scope := .Values.controllerManager.scope -}}
{{- range concat $scope.watchNamespaces $scope.ignoreNamespaces -}}
  {{- if or (gt (len .) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .)) -}}
    {{- fail (printf "generated namespaced RBAC requires exact namespace names, got %q; use rbac.create: false for supplied RBAC" .) -}}
  {{- end -}}
{{- end -}}
{{- $allowed := list -}}
{{- range uniq $scope.watchNamespaces -}}
  {{- if not (has . $scope.ignoreNamespaces) -}}
    {{- $allowed = append $allowed . -}}
  {{- end -}}
{{- end -}}
{{- if empty $allowed -}}
  {{- fail "namespaced RBAC requires at least one namespace after ignore rules" -}}
{{- end -}}
{{- toJson $allowed -}}
{{- end -}}

{{/* Generate resource rules from exact names or a lone wildcard. */}}
{{- define "lifecycle-controller.resourceRules" -}}
{{- $scope := .Values.controllerManager.scope -}}
{{- $resources := include "lifecycle-controller.watchResources" . | fromJsonArray -}}
{{- $namespaced := eq (include "lifecycle-controller.rbacScope" .) "namespaced" -}}
{{- range $scope.ignoreResources -}}
  {{- if regexMatch "[?\\[\\]\\\\]" . -}}
    {{- fail "chart-generated RBAC requires exact ignoreResources entries or *; use rbac.create: false for supplied RBAC" -}}
  {{- end -}}
  {{- if and (contains "*" .) (ne . "*") -}}
    {{- fail "chart-generated RBAC requires exact ignoreResources entries or *; use rbac.create: false for supplied RBAC" -}}
  {{- end -}}
{{- end -}}
{{- if $resources -}}
  {{- range uniq $resources -}}
    {{- $entry := . -}}
    {{- $parts := splitList "." . -}}
    {{- $res := index $parts 0 -}}
    {{- if eq $res "" -}}
      {{- fail (printf "invalid controllerManager.scope.watchResources entry %q: resource segment cannot be empty" .) -}}
    {{- end -}}
    {{- $grp := "" -}}
    {{- if gt (len $parts) 1 -}}
      {{- $rest := rest $parts -}}
      {{- if has "" $rest -}}
        {{- fail (printf "invalid controllerManager.scope.watchResources entry %q: apiGroup segments cannot be empty" .) -}}
      {{- end -}}
      {{- $grp = join "." $rest -}}
    {{- end -}}
    {{- if or (and (ne $entry "*") (contains "*" $entry)) (not (regexMatch "^([a-z0-9-]+|\\*)$" $res)) (and (ne $grp "") (not (regexMatch "^[a-z0-9.-]+$" $grp))) -}}
      {{- fail (printf "invalid controllerManager.scope.watchResources entry %q: chart-generated RBAC requires exact names or a lone *" .) -}}
    {{- end -}}
    {{- if eq $entry "*" -}}{{- $grp = "*" -}}{{- end -}}
    {{- if and (not (has $entry $scope.ignoreResources)) (not (has "*" $scope.ignoreResources)) (not (and $namespaced (eq $entry "namespaces"))) }}
- apiGroups: [{{ $grp | quote }}]
  resources: [{{ $res | quote }}]
  verbs: ["delete", "get", "list", "patch", "update", "watch"]
    {{- end -}}
  {{- end -}}
{{- else if not (has "*" $scope.ignoreResources) }}
- apiGroups: ["*"]
  resources: ["*"]
  verbs: ["delete", "get", "list", "patch", "update", "watch"]
{{- end }}
{{- include "lifecycle-controller.eventRules" . | nindent 0 }}
{{- end -}}

{{- define "lifecycle-controller.eventRules" -}}
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: ["events.k8s.io"]
  resources: ["events"]
  verbs: ["create", "patch"]
{{- end -}}
