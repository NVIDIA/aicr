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

package bundler

import (
	"context"
	stderrors "errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// readinessFileName is the per-component convention file (a chainsaw Test)
// that, when present and --readiness-hooks is set, drives emission of a
// standalone readiness gate chart. See #904.
const readinessFileName = "readiness.yaml"

// defaultGateImageRepo is the image (without tag) that runs the readiness
// gate Job. It carries the gate CLI plus an embedded chainsaw binary and is
// published through AICR's standard goreleaser pipeline alongside aicr/aicrd.
// Phase 1 builds this locally and `kind load`s it as :dev; Phase 2 publishes
// release-tagged images. The registry mirrors .settings.yaml build.image_registry.
const defaultGateImageRepo = "ghcr.io/nvidia/aicr-gate"

// readinessManifestKey is the single multi-document manifest path emitted into
// each readiness gate chart's templates/ directory. A single file keeps the
// ServiceAccount, RBAC, ConfigMap, and Job together and ordered.
const readinessManifestKey = "readiness.yaml"

// gateImage returns the fully-qualified gate image reference. The tag tracks
// the bundler version so a bundle pins the gate image to the AICR release that
// produced it; an empty/"dev" version resolves to the locally-built dev tag
// used by the Phase 1 Kind smoke test.
func (b *DefaultBundler) gateImage() string {
	tag := b.Config.Version()
	if tag == "" {
		tag = "dev"
	}
	return defaultGateImageRepo + ":" + tag
}

// collectComponentReadiness gathers per-component readiness gate manifests,
// keyed by component name then manifest path (mirroring the pre/post manifest
// collectors so the localformat writer can treat readiness as another
// auxiliary injection phase). Returns an empty map when --readiness-hooks is
// off, so callers can forward the result unconditionally.
//
// For each component that ships recipes/components/<name>/readiness.yaml, the
// gate manifests (ServiceAccount, read-only ClusterRole + binding, a ConfigMap
// carrying the chainsaw Test, and the gate Job) are synthesized with the
// resolved namespace templated via {{ .Release.Namespace }} — the same
// mechanism the pre/post manifests use — and the gate image baked in.
func (b *DefaultBundler) collectComponentReadiness(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
) (map[string]map[string][]byte, error) {

	result := make(map[string]map[string][]byte)
	if !b.Config.ReadinessHooks() {
		return result, nil
	}

	provider := recipeResult.DataProvider()
	image := b.gateImage()

	for _, ref := range recipeResult.ComponentRefs {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				"context cancelled while collecting component readiness gates", err)
		}

		path := fmt.Sprintf("components/%s/%s", ref.Name, readinessFileName)
		testYAML, err := recipe.GetManifestContentWithProvider(provider, path)
		if err != nil {
			if stderrors.Is(err, fs.ErrNotExist) {
				continue // component ships no readiness gate; skip
			}
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				fmt.Sprintf("failed to load readiness gate %s for component %s", path, ref.Name))
		}

		manifest, genErr := renderReadinessGateManifest(ref.Name, image, testYAML)
		if genErr != nil {
			return nil, genErr
		}
		result[ref.Name] = map[string][]byte{readinessManifestKey: manifest}
	}

	return result, nil
}

// renderReadinessGateManifest builds the multi-document gate chart manifest for
// one component. The namespace is left as a {{ .Release.Namespace }} template
// token (resolved by the localformat writer's manifest.Render against the
// component's resolved namespace); the gate image and the embedded chainsaw
// Test are baked in literally.
func renderReadinessGateManifest(componentName, image string, testYAML []byte) ([]byte, error) {
	if componentName == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "readiness gate: empty component name")
	}

	// Indent the chainsaw Test under the ConfigMap data block scalar. Each
	// source line is indented by 4 spaces (data key at 2, block content at 4).
	indented := indentBlock(string(testYAML), "    ")

	saName := componentName + "-readiness-gate"
	bundleName := componentName + "-readiness-bundle"

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
  name: %[1]s
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
    resources: ["clusterpolicies"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %[1]s
subjects:
  - kind: ServiceAccount
    name: %[1]s
    namespace: {{ .Release.Namespace }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: %[1]s
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
spec:
  backoffLimit: 0
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
		defaults.ReadinessGateMaxWait.String())

	return []byte(sb.String()), nil
}

// indentBlock prefixes every non-empty line of s with prefix. Empty lines are
// left blank (no trailing whitespace) so the rendered ConfigMap block scalar
// stays clean.
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
