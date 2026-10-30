{{/*
Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/}}

{{/* The AICR releases this chart and its operator support. */}}
{{- define "aicr-operator.aicrRange" -}}>=1.0.0, <2.0.0{{- end -}}

{{/* Fails rendering for an unsupported AICR version or an unusable layout. */}}
{{- define "aicr-operator.validate" -}}
{{- $range := include "aicr-operator.aicrRange" . -}}
{{- if not (semverCompare $range (trimPrefix "v" .Values.aicr.version)) -}}
{{- fail (printf "aicr.version %s is outside the supported range %s" .Values.aicr.version $range) -}}
{{- end -}}
{{- $created := list .Values.namespaces.jobs .Values.namespaces.discovery -}}
{{- if .Values.validation.enabled -}}{{- $created = append $created .Values.namespaces.validation -}}{{- end -}}
{{- range .Values.namespaces.components -}}{{- if ne .create false -}}{{- $created = append $created .name -}}{{- end -}}{{- end -}}
{{- if has .Release.Namespace $created -}}
{{- fail (printf "the release namespace %s is the operator namespace; the chart cannot also create it" .Release.Namespace) -}}
{{- end -}}
{{- if ne (len $created) (len (uniq $created)) -}}
{{- fail "namespaces.jobs, namespaces.discovery, namespaces.validation and namespaces.components must be distinct" -}}
{{- end -}}
{{- if and .Values.validation.enabled (not .Values.aicr.validatorImages) -}}
{{- fail "validation.enabled needs aicr.validatorImages: the validator images of aicr.version, pinned by tag and digest" -}}
{{- end -}}
{{- with .Values.validation.signingKey -}}
{{- if not $.Values.validation.enabled -}}
{{- fail "validation.signingKey needs validation.enabled" -}}
{{- end -}}
{{- if not (regexMatch "^(awskms|gcpkms|azurekms|hashivault)://.+" .) -}}
{{- fail "validation.signingKey is not an awskms://, gcpkms://, azurekms:// or hashivault:// key URI" -}}
{{- end -}}
{{- if semverCompare "<1.1.0" (trimPrefix "v" $.Values.aicr.version) -}}
{{- fail (printf "validation.signingKey needs AICR v1.1.0 or later; aicr.version is %s" $.Values.aicr.version) -}}
{{- end -}}
{{- end -}}
{{- if eq .Values.registry.mode "inCluster" -}}
{{- if .Values.registry.repository -}}
{{- fail "registry.repository is set but registry.mode is inCluster, the default: set registry.mode: external to keep using that registry" -}}
{{- end -}}
{{- if .Values.registry.auth.staticSecretRef -}}
{{- fail "registry.auth.staticSecretRef applies to registry.mode: external only; the in-cluster registry's login is generated" -}}
{{- end -}}
{{- if not .Values.registry.inCluster.image -}}
{{- fail "registry.mode inCluster needs registry.inCluster.image" -}}
{{- end -}}
{{- else -}}
{{- if not .Values.registry.repository -}}
{{- fail "registry.mode external needs registry.repository (oci://host/path)" -}}
{{- end -}}
{{- if and (eq .Values.registry.auth.provider "aws") .Values.registry.auth.staticSecretRef -}}
{{- fail "registry.auth.staticSecretRef applies to the static provider only" -}}
{{- end -}}
{{- end -}}
{{- /* The installed project names the registry the running plans were
  mirrored to; values alone cannot tell an upgrade that drops them from one
  that means to switch. */ -}}
{{- with lookup "argoproj.io/v1alpha1" "AppProject" .Values.engine.namespace .Values.engine.project -}}
{{- $installed := .spec.sourceRepos | default list -}}
{{- $rendered := printf "%s/charts/*" (include "aicr-operator.registry" $) -}}
{{- if and $installed (not (has $rendered $installed)) -}}
{{- fail (printf "the installed %s AppProject admits charts from %s, but these values select %s: switching an installation's registry is not supported; set registry.mode and registry.repository to match it" $.Values.engine.project (first $installed) $rendered) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The in-cluster registry's Service host, fully qualified: a shorter name is
tried against the pods' DNS search path first, where a namespace named "svc"
could answer for it.
*/}}
{{- define "aicr-operator.registryHost" -}}
aicr-registry.{{ .Release.Namespace }}.svc.{{ .Values.registry.inCluster.clusterDomain }}:5000
{{- end -}}

{{/* The runtime registry prefix the operator, Argo CD and the policies use. */}}
{{- define "aicr-operator.registry" -}}
{{- if eq .Values.registry.mode "inCluster" -}}
oci://{{ include "aicr-operator.registryHost" . }}/aicr
{{- else -}}
{{ .Values.registry.repository }}
{{- end -}}
{{- end -}}

{{/*
The docker config Secret the operator and pushing Jobs read: the chart's for
the in-cluster registry, the administrator's for the static provider, none
for aws or an anonymous registry.
*/}}
{{- define "aicr-operator.credentialsSecret" -}}
{{- if eq .Values.registry.mode "inCluster" -}}
aicr-registry-auth
{{- else if eq .Values.registry.auth.provider "static" -}}
{{ .Values.registry.auth.staticSecretRef }}
{{- end -}}
{{- end -}}

{{/* "true" when the operator refreshes an ECR token into repo-creds. */}}
{{- define "aicr-operator.ecr" -}}
{{- and (eq .Values.registry.mode "external") (eq .Values.registry.auth.provider "aws") -}}
{{- end -}}

{{/* The registry's pod labels, for its Service and NetworkPolicy. */}}
{{- define "aicr-operator.registrySelector" -}}
app.kubernetes.io/name: aicr-registry
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "aicr-operator.labels" -}}
app.kubernetes.io/name: aicr-operator
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Resources helm uninstall leaves in place. */}}
{{- define "aicr-operator.keep" -}}
helm.sh/resource-policy: keep
{{- end -}}

{{- define "aicr-operator.operatorUser" -}}
system:serviceaccount:{{ .Release.Namespace }}:aicr-operator
{{- end -}}

{{/* The component namespace names, comma-separated, for the operator. */}}
{{- define "aicr-operator.componentNames" -}}
{{- $names := list -}}
{{- range .Values.namespaces.components -}}{{- $names = append $names .name -}}{{- end -}}
{{- join "," $names -}}
{{- end -}}

{{/* Namespaces owned Applications may target: every component namespace. */}}
{{- define "aicr-operator.destinations" -}}
{{- $names := list -}}
{{- range .Values.namespaces.components -}}{{- $names = append $names .name -}}{{- end -}}
{{- toJson $names -}}
{{- end -}}

{{/* Pod Security labels for a namespace at one level. */}}
{{- define "aicr-operator.podSecurity" -}}
pod-security.kubernetes.io/enforce: {{ . }}
pod-security.kubernetes.io/audit: {{ . }}
pod-security.kubernetes.io/warn: {{ . }}
{{- end -}}
