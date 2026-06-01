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

	"github.com/NVIDIA/aicr/pkg/bundler/gatemanifest"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"sigs.k8s.io/yaml"
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
	if tag == "" || tag == "dev" {
		tag = "dev" // preserve Phase-1 kind-smoke :dev
	} else if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag // release contract: 0.13.0 -> v0.13.0
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
		testYAML, err := recipe.GetManifestContentWithContext(ctx, provider, path)
		if err != nil {
			if stderrors.Is(err, fs.ErrNotExist) {
				continue // component ships no readiness gate; skip
			}
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				fmt.Sprintf("failed to load readiness gate %s for component %s", path, ref.Name))
		}

		if err := validateReadinessTestYAML(ref.Name, testYAML); err != nil {
			return nil, err
		}

		manifest, genErr := gatemanifest.Render(ref.Name, image, testYAML, b.Config.Deployer())
		if genErr != nil {
			return nil, genErr
		}
		result[ref.Name] = map[string][]byte{readinessManifestKey: manifest}
	}

	return result, nil
}

// validateReadinessTestYAML fails fast when readiness.yaml is not a chainsaw Test.
func validateReadinessTestYAML(componentName string, testYAML []byte) error {
	var head struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(testYAML, &head); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("readiness gate for %s: invalid YAML", componentName), err)
	}
	if !strings.Contains(head.APIVersion, "chainsaw.kyverno.io") {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("readiness gate for %s: apiVersion must be chainsaw.kyverno.io/*, got %q",
				componentName, head.APIVersion))
	}
	if head.Kind != "Test" {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("readiness gate for %s: kind must be Test, got %q", componentName, head.Kind))
	}
	return nil
}
