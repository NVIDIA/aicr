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
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

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
func Render(componentName, image string, testYAML []byte, deployer config.DeployerType) ([]byte, error) {
	if componentName == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "readiness gate: empty component name")
	}

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
  template:
    spec:
      restartPolicy: Never
      serviceAccountName: %[1]s
      containers:
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
		componentClusterRoleRules(componentName))

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
	case config.DeployerHelm:
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
	case config.DeployerFlux, config.DeployerHelmfile:
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
