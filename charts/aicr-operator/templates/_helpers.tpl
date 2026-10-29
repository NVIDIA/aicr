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
{{- if and (eq .Values.registry.auth.provider "aws") .Values.registry.auth.staticSecretRef -}}
{{- fail "registry.auth.staticSecretRef applies to the static provider only" -}}
{{- end -}}
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
