// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package gatemanifest synthesizes the Kubernetes manifests for a component
// readiness gate Job (ServiceAccount, RBAC, ConfigMap, Job). Kept separate
// from pkg/bundler so deployer golden tests can import it without an import
// cycle through the bundler's deployer wiring.
package gatemanifest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Scheduling is the bundle's system-node placement, used for a gate whose
// component declares no scheduling paths.
type Scheduling struct {
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
}

// Placement is a validated gate pod placement built by NewPlacement or
// FromValues. The zero value renders no placement fields.
type Placement struct {
	nodeSelector string
	tolerations  string
}

// podSpecIndent is the column of the gate Job's pod spec fields.
const podSpecIndent = 6

// NewPlacement validates sched and renders it for the gate Job's pod spec.
// Keyless tolerations are dropped after validation: an empty key matches every
// taint, including the node lifecycle controller's not-ready, unreachable and
// unschedulable taints, so a gate carrying one could bind to an unhealthy or
// cordoned node and would lose the default eviction from unreachable nodes.
func NewPlacement(sched Scheduling) (Placement, error) {
	if err := sched.validate(); err != nil {
		return Placement{}, err
	}
	var keyed []corev1.Toleration
	for _, t := range sched.Tolerations {
		if t.Key != "" {
			keyed = append(keyed, t)
		}
	}
	var p Placement
	var err error
	if len(sched.NodeSelector) > 0 {
		if p.nodeSelector, err = literalField("nodeSelector", sched.NodeSelector); err != nil {
			return Placement{}, err
		}
	}
	if len(keyed) > 0 {
		if p.tolerations, err = literalField("tolerations", keyed); err != nil {
			return Placement{}, err
		}
	}
	return p, nil
}

func literalField(name string, v any) (string, error) {
	out, err := yaml.Marshal(map[string]any{name: v})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "readiness gate: failed to marshal scheduling", err)
	}
	return indentBlock(string(out), strings.Repeat(" ", podSpecIndent)) + "\n", nil
}

// ValuesPaths names the dot-notation paths in the gated component's values
// that hold its own node selector and tolerations.
type ValuesPaths struct {
	NodeSelector string
	Tolerations  string
}

// valuesPathRE matches the values paths that are safe to embed in the
// generated template.
var valuesPathRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// FromValues returns p with each field that has a non-empty path in paths
// read from the component's values when Helm installs the gate chart.
// Tolerations with an empty key are skipped, as in NewPlacement. A field
// with an empty path keeps p's placement. FromValues returns an
// ErrCodeInvalidRequest error if a path does not match valuesPathRE.
func (p Placement) FromValues(paths ValuesPaths) (Placement, error) {
	if paths.NodeSelector != "" {
		top, nested, err := valuesLookup(paths.NodeSelector)
		if err != nil {
			return Placement{}, err
		}
		// Helm does not merge the chart-name nesting into the top-level
		// path, so the template merges the two maps itself.
		p.nodeSelector = valuesField("nodeSelector", paths.NodeSelector,
			`{{- $gateSelector := `+top+` }}{{- $gateNestedSelector := `+nested+` }}`+
				`{{- if and (kindIs "map" $gateSelector) (kindIs "map" $gateNestedSelector) }}{{- $gateSelector = mergeOverwrite (deepCopy $gateSelector) $gateNestedSelector }}`+
				`{{- else if $gateNestedSelector }}{{- $gateSelector = $gateNestedSelector }}{{- end }}`+
				`{{- with $gateSelector }}{{ dict "nodeSelector" . | toYaml | nindent `+strconv.Itoa(podSpecIndent)+` }}{{- end }}`)
	}
	if paths.Tolerations != "" {
		top, nested, err := valuesLookup(paths.Tolerations)
		if err != nil {
			return Placement{}, err
		}
		// Install-time values never pass through NewPlacement, so the
		// template drops keyless tolerations itself.
		p.tolerations = valuesField("tolerations", paths.Tolerations,
			`{{- $gateTolerations := list }}`+
				`{{- range `+nested+` | default `+top+` | default list }}{{- if .key }}{{- $gateTolerations = append $gateTolerations . }}{{- end }}{{- end }}`+
				`{{- with $gateTolerations }}{{ dict "tolerations" . | toYaml | nindent `+strconv.Itoa(podSpecIndent)+` }}{{- end }}`)
	}
	return p, nil
}

// valuesField returns a pod spec line that Helm renders as the named field,
// or as nothing when the value is empty.
func valuesField(name, path, action string) string {
	// The YAML comment survives the bundle-time YAML round-trip, and the
	// backtick escape keeps the bundle-time render from executing the action.
	return fmt.Sprintf("%s# %s from %s {{`%s`}}\n",
		strings.Repeat(" ", podSpecIndent), name, path, action)
}

// valuesLookup returns Helm expressions for the value at path and for the
// same path under the chart name, where a vendored parent nests its values.
// valuesLookup returns an ErrCodeInvalidRequest error if path does not match
// valuesPathRE.
func valuesLookup(path string) (top, nested string, err error) {
	if !valuesPathRE.MatchString(path) {
		return "", "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("readiness gate: invalid values path %q", path))
	}
	segments := strings.Split(path, ".")
	keys := make([]string, len(segments))
	for i, s := range segments {
		keys[i] = strconv.Quote(s)
	}
	digKeys := strings.Join(keys, " ")
	// dig rejects Helm's Values type, so it walks .Values.AsMap. The
	// bundle-time render fills in the chart name.
	return "(dig " + digKeys + " nil .Values.AsMap)",
		"(dig `}}{{ .Chart.Name | quote }}{{` " + digKeys + " nil .Values.AsMap)", nil
}

// validate returns an error for scheduling the API server would reject.
func (s Scheduling) validate() error {
	// Render executes the manifest as a Go template, and no key or value these
	// rules admit can carry template syntax.
	if err := config.ValidateNodeSelector(s.NodeSelector); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "readiness gate: invalid node selector", err)
	}
	if err := config.ValidateTolerations(s.Tolerations); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "readiness gate: invalid toleration", err)
	}
	return nil
}

// Render builds the multi-document gate chart manifest for one component. The
// namespace is left as a {{ .Release.Namespace }} template token (resolved by
// the localformat writer's manifest.Render against the component's resolved
// namespace); the gate image and the embedded chainsaw Test are baked in
// literally.
//
// The cluster-scoped ClusterRole and ClusterRoleBinding are namespace-qualified
// (name suffixed with the resolved namespace) so two bundles deploying the same
// component to different namespaces (the multi-tenant --app-name case, #1011)
// do not overwrite each other's cluster-scoped objects. The namespaced
// ServiceAccount, ConfigMap, and Job keep the bare component name — identical
// names in distinct namespaces never collide.
//
// placement sets the gate pod's node selector and tolerations.
func Render(componentName, image string, testYAML []byte, deployer config.DeployerType, placement Placement) ([]byte, error) {
	if componentName == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "readiness gate: empty component name")
	}
	scheduling := placement.nodeSelector + placement.tolerations

	indented := indentBlock(string(testYAML), "    ")

	saName := componentName + "-readiness-gate"
	bundleName := componentName + "-readiness-bundle"
	jobAnnotations := jobMetadataAnnotations(deployer)

	var sb strings.Builder
	fmt.Fprintf(&sb, `apiVersion: v1
kind: ServiceAccount
metadata:
  name: %[1]s
  namespace: {{ .Release.Namespace }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: %[1]s-{{ .Release.Namespace }}
rules:
  - apiGroups: [""]
    resources: ["pods", "nodes", "namespaces", "services", "configmaps", "events"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["deployments", "daemonsets", "statefulsets", "replicasets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["nvidia.com"]
    resources: ["*"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["operators.coreos.com"]
    resources: ["clusterserviceversions"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["get", "list", "watch"]
%[12]s---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %[1]s-{{ .Release.Namespace }}
subjects:
  - kind: ServiceAccount
    name: %[1]s
    namespace: {{ .Release.Namespace }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: %[1]s-{{ .Release.Namespace }}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[2]s
  namespace: {{ .Release.Namespace }}
data:
  %[3]s.yaml: |
%[4]s
---
apiVersion: batch/v1
kind: Job
metadata:
  name: %[1]s
  namespace: {{ .Release.Namespace }}
%[10]s
spec:
  backoffLimit: %[11]d
  activeDeadlineSeconds: %[14]d
  template:
    spec:
      restartPolicy: Never
      serviceAccountName: %[1]s
%[13]s      containers:
        - name: gate
          image: %[5]s
          imagePullPolicy: IfNotPresent
          args:
            - --bundle-dir=/bundle
            - --namespace={{ .Release.Namespace }}
            - --timeout=%[6]s
            - --poll-interval=%[7]s
            - --stability-window=%[8]s
            - --max-wait=%[9]s
          volumeMounts:
            - name: bundle
              mountPath: /bundle
              readOnly: true
      volumes:
        - name: bundle
          configMap:
            name: %[2]s
`, saName, bundleName, componentName, indented, image,
		defaults.ReadinessGateExecTimeout.String(),
		defaults.ReadinessGatePollInterval.String(),
		defaults.ReadinessGateStabilityWindow.String(),
		defaults.ReadinessGateMaxWait.String(),
		jobAnnotations,
		defaults.ReadinessGateBackoffLimit,
		componentClusterRoleRules(componentName),
		scheduling,
		int64((defaults.ReadinessGateMaxWait + defaults.ReadinessGateActiveDeadlineBuffer).Seconds()))

	return []byte(sb.String()), nil
}

// componentClusterRoleRules returns any component-specific ClusterRole rules
// appended to the uniform base rules above. Emitting a rule only for the
// component whose readiness Test actually reads its API group keeps unused
// permissions off gate ServiceAccounts of unrelated components (PR #2337
// review). Kept as a switch rather than a data-driven scan of testYAML so
// the emitter's RBAC surface stays statically auditable — the trade-off is
// that a new readiness gate for a new API group must be registered here.
func componentClusterRoleRules(componentName string) string {
	switch componentName { //nolint:gocritic // one permission family today; kept as a switch so future readiness API groups remain explicitly allowlisted.
	case "network-operator", "network-operator-ocp":
		// See recipes/components/network-operator*/readiness.yaml — both
		// gates assert on mellanox.com/v1alpha1 NicClusterPolicy.
		return `  - apiGroups: ["mellanox.com"]
    resources: ["nicclusterpolicies"]
    verbs: ["get", "list", "watch"]
`
	}
	return ""
}

func jobMetadataAnnotations(deployer config.DeployerType) string {
	switch deployer {
	case config.DeployerHelm, config.DeployerTerraform:
		// Both install the gate through Helm itself, so the hook is what
		// makes it re-assert: a Job's spec.template is immutable, and
		// before-hook-creation is the only thing that deletes and recreates
		// it on upgrade. Helm also blocks on hook completion as part of the
		// release, which is what the terraform module's dependents depend on.
		return `  annotations:
    helm.sh/hook: post-install,post-upgrade
    helm.sh/hook-delete-policy: before-hook-creation`
	case config.DeployerArgoCD, config.DeployerArgoCDHelm:
		// The Job's spec.selector and spec.template.metadata.labels are
		// server-generated/immutable, and the rendered manifest correctly
		// omits them. Plain Replace=true maps to `kubectl replace`, which the
		// API server rejects on any upgrade that changes the Job spec (e.g.
		// an image tag bump), leaving the Application permanently
		// OutOfSync. Force=true is ArgoCD's documented option to
		// delete-and-recreate when a replace fails. This alone would
		// delete-and-recreate the Job on EVERY sync, including no-op
		// resyncs where nothing changed — see the ApplyOutOfSyncOnly=true
		// entry this deployer adds to the readiness folder's
		// Application-level spec.syncPolicy.syncOptions
		// (pkg/bundler/deployer/argocd/argocd.go's buildApplicationData),
		// which excludes already-in-sync resources from a sync operation
		// and stops the needless rerun. The two mechanisms are
		// complementary: Job-level Replace+Force handles genuine spec
		// diffs (e.g. an image tag bump); Application-level
		// ApplyOutOfSyncOnly prevents unnecessary reruns when there is no
		// diff at all. Deliberately NOT using a Helm-style sync hook
		// (helm.sh/hook) here: per
		// pkg/bundler/deployer/localformat/hooks.go's stripHelmHooks doc,
		// hook-annotated resources are excluded from ArgoCD's normal drift
		// detection, so an image-tag-only bump could silently go undetected.
		return `  annotations:
    argocd.argoproj.io/sync-options: Replace=true,Force=true`
	case config.DeployerFlux, config.DeployerHelmfile, config.DeployerFleet:
		return ""
	default:
		return ""
	}
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
