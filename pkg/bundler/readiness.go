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
	"log/slog"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/gatemanifest"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"sigs.k8s.io/yaml"
)

// readinessFileName is the per-component convention file (a chainsaw Test)
// that, when present and --readiness-hooks is set, drives emission of a
// standalone readiness gate chart. See #904.
const readinessFileName = "readiness.yaml"

// networkOperatorComponentName is the ref name of the Helm-deployed
// network-operator component whose gate asserts a NicClusterPolicy CR.
// The pinned upstream chart (26.4.1) does not template NCP even with
// deployCR=true, so overlays that do not attach one explicitly (kind,
// Talos base) have nothing to gate on — the readiness gate must skip
// itself in that case, or it will poll to --max-wait timeout on every
// deploy (#2337).
const networkOperatorComponentName = "network-operator"

const (
	networkOperatorOCPComponentName = "network-operator-ocp"
	gpuDriverEnabledValuePath       = "driver.enabled"
	gpuDriverRDMAEnabledValuePath   = "driver.rdma.enabled"
)

// peermemManagedOFEDPairs names the GPU/Network Operator component pairs that
// install the legacy nvidia-peermem path and its required OFED/DOCA stack.
// Presence in componentValues means both components survived recipe and
// bundle-time filtering; the NCP manifest probe below separately confirms that
// this recipe actually asks Network Operator to reconcile the fabric.
var peermemManagedOFEDPairs = []struct {
	gpuOperator     string
	networkOperator string
}{
	{gpuOperatorComponentName, networkOperatorComponentName},
	{"gpu-operator-ocp", networkOperatorOCPComponentName},
}

// ncpKindRE / ncpAPIVersionRE match a NicClusterPolicy top-level manifest
// via line-anchored regexes rather than YAML unmarshalling. Attached NCP
// manifests are Helm templates ({{ .Release.Service }} etc.), which
// strict YAML parsers reject; a probe just needs to detect presence.
// Both patterns are pinned to column 0 so nested list items or comments
// (which start with an indent or `#`) never trigger a false positive.
// The apiVersion pattern intentionally uses a group prefix so a future
// chart version that graduates NCP past v1alpha1 still matches without
// a code change. TestNCPRegexNearMiss pins these invariants so a future
// edit that drops (?m), the ^ anchor, or the AND between the two patterns
// fails a test rather than silently re-introducing the #2337 false-emit
// regression.
var (
	ncpKindRE       = regexp.MustCompile(`(?m)^kind:\s*NicClusterPolicy\s*$`)
	ncpAPIVersionRE = regexp.MustCompile(`(?m)^apiVersion:\s*mellanox\.com/`)
)

// defaultGateImageRepo is the image (without tag) that runs the readiness
// gate Job. It carries only the gate CLI — assertions run in-process through
// pkg/chainsaw; the embedded `chainsaw` binary this image used to ship was
// removed in #2038 as the sole source of its HIGH CVEs. Published through
// AICR's standard goreleaser pipeline alongside aicr/aicrd.
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
//
// The gate image is published ONLY on release tags (on-tag.yaml). Goreleaser
// snapshot builds stamp the binary with a `<version>-next` string (see
// .goreleaser.yaml snapshot.version_template) for which no image exists in
// ghcr, so those — like empty/"dev" — must fall back to the :dev tag rather
// than fabricating an unpublished `aicr-gate:vX.Y.Z-next` ref that would
// ImagePullBackOff. Mirrors the snapshot guard in pkg/cli and validator/catalog.
func (b *DefaultBundler) gateImage() string {
	tag := b.Config.Version()
	switch {
	case tag == "" || tag == "dev" || strings.Contains(tag, "-next"):
		tag = "dev" // preserve Phase-1 kind-smoke :dev; snapshots have no published image
	case !strings.HasPrefix(tag, "v"):
		tag = "v" + tag // release contract: 0.13.0 -> v0.13.0
	}
	return defaultGateImageRepo + ":" + tag
}

// wrapCtxErr renders ctx.Err() as a StructuredError whose ErrorCode and
// message distinguish explicit cancellation from deadline expiration
// while preserving the underlying context sentinel through Unwrap so
// upstream callers can still branch on stderrors.Is(err, context.Canceled)
// and stderrors.Is(err, context.DeadlineExceeded). Cancellation maps to
// ErrCodeCanceled (operator abort), deadline expiration to ErrCodeTimeout
// (time budget exhausted); the two are distinct exit paths in
// pkg/errors/exitcode.go. Callers should have already confirmed
// ctx.Err() != nil.
func wrapCtxErr(ctx context.Context, activity string) error {
	cause := ctx.Err()
	code := errors.ErrCodeCanceled
	verb := "cancelled"
	if stderrors.Is(cause, context.DeadlineExceeded) {
		code = errors.ErrCodeTimeout
		verb = "deadline exceeded"
	}
	return errors.Wrap(code,
		fmt.Sprintf("context %s while %s", verb, activity), cause)
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

	// The RDMA-fabric probe is only run when a network-operator ref is
	// encountered; keep the result cached so multi-component recipes
	// don't reload every ref's ManifestFiles per iteration.
	var (
		fabricProbed  bool
		fabricPresent bool
	)

	for _, ref := range recipeResult.ComponentRefs {
		if ctx.Err() != nil {
			return nil, wrapCtxErr(ctx, "collecting component readiness gates")
		}

		// The network-operator gate asserts a NicClusterPolicy the
		// pinned Helm chart does not create; skip when no ref attaches
		// one. Otherwise the gate polls to timeout on kind + Talos-base
		// recipes that include network-operator without an NCP (#2337).
		if ref.Name == networkOperatorComponentName {
			if !fabricProbed {
				present, err := recipeAttachesNicClusterPolicy(ctx, provider, recipeResult.ComponentRefs)
				if err != nil {
					return nil, err
				}
				fabricProbed = true
				fabricPresent = present
			}
			if !fabricPresent {
				slog.Info("skipping readiness gate for network-operator: recipe attaches no NicClusterPolicy CR",
					"component", ref.Name)
				continue
			}
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

// warnPeermemReadinessDisabled surfaces the partial-guarantee boundary of a
// dependencyRefs edge for the legacy nvidia-peermem route (#2499). The edge
// orders deployment units, but without readiness gates it does not wait for
// Network Operator to report NicClusterPolicy.status.state=ready before GPU
// Operator starts its driver pods.
//
// Detection uses the final filtered component inventory and effective values,
// so it stays silent for DMA-BUF recipes, AKS azure-managed mode
// (driver.enabled=false), readiness-enabled bundles, and subset bundles that
// intentionally leave either operator to an external deployment. The NCP
// manifest probe is the same one readiness-gate emission uses, avoiding a
// warning for kind/Talos recipes where Network Operator installs no OFED/DOCA
// policy.
func (b *DefaultBundler) warnPeermemReadinessDisabled(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	componentValues map[string]map[string]any,
) error {

	if b.Config == nil || b.Config.ReadinessHooks() || recipeResult == nil {
		return nil
	}

	for _, pair := range peermemManagedOFEDPairs {
		gpuValues, gpuPresent := componentValues[pair.gpuOperator]
		_, networkPresent := componentValues[pair.networkOperator]
		if !gpuPresent || !networkPresent ||
			!componentBoolValue(gpuValues, gpuDriverEnabledValuePath) ||
			!componentBoolValue(gpuValues, gpuDriverRDMAEnabledValuePath) {

			continue
		}

		ncpPresent, err := recipeAttachesNicClusterPolicy(
			ctx, recipeResult.DataProvider(), recipeResult.ComponentRefs)
		if err != nil {
			return err
		}
		if !ncpPresent {
			continue
		}

		warning := fmt.Sprintf(
			"%s installs a GPU-managed driver with %s=true and %s=true while %s installs "+
				"OFED/DOCA, but component readiness gating is disabled. dependencyRefs only orders "+
				"manifest submission; it does not wait for NicClusterPolicy.status.state=ready before "+
				"deploying %s. Use --readiness-hooks with Helm or Argo CD, or provide an equivalent "+
				"external barrier; Flux, Helmfile, and the bundle REST API cannot request this gate "+
				"yet (#2499).",
			pair.gpuOperator, gpuDriverEnabledValuePath, gpuDriverRDMAEnabledValuePath,
			pair.networkOperator, pair.gpuOperator)
		slog.Warn(warning,
			"gpu_operator", pair.gpuOperator,
			"network_operator", pair.networkOperator,
			"readiness_hooks", false)
		b.appendWarning(warning)
		return nil
	}

	return nil
}

func componentBoolValue(values map[string]any, valuePath string) bool {
	value, found := component.GetValueByPath(values, valuePath)
	if !found {
		return false
	}
	enabled, ok := value.(bool)
	return ok && enabled
}

// recipeAttachesNicClusterPolicy reports whether any component in the
// resolved recipe attaches a NicClusterPolicy CR via PreManifestFiles or
// ManifestFiles. The Helm network-operator gate depends on that CR: the
// pinned upstream chart does not template it, so a recipe that includes
// network-operator without attaching an NCP has nothing for the gate to
// assert on and the gate would poll to --max-wait timeout (#2337).
//
// Sibling predicate: validators/deployment/expected_resources.go's
// recipeDeclaresRDMAFabric encodes the same "does this recipe stand up
// an NCP?" question for the RDMA-fabric-readiness check, but decides via
// a filename substring (nicClusterPolicyManifestMarker) over a single
// ref's ManifestFiles rather than by scanning manifest content across
// every ref. Package layering blocks direct reuse, so the two functions
// have deliberately different names and must be kept in sync when a
// future overlay changes how an NCP is attached (a new marker filename,
// an attachment via PreManifestFiles, a differently-scoped ref). Update
// both — and their cross-reference comments — together.
//
// Iterates every ref because the NCP is usually attached by an
// overlay-owned ref (e.g., aks.yaml attaches
// components/network-operator/manifests/nic-cluster-policy-aks.yaml
// under the network-operator ref), not by a base component definition.
// Splits on the canonical "\n---\n" separator that loadManifestFiles
// emits; each doc is regex-scanned, so unrelated manifest content is
// cheap. Same-doc match is best-effort: the split does not normalize
// CRLF, a leading `---` at byte 0, or a separator with trailing
// whitespace, in which case the two (?m) patterns fall back to
// whole-file matching. That is safe today because every embedded NCP
// manifest is LF-terminated and no other embedded manifest ships an
// `apiVersion: mellanox.com/` line, but the split alone is not a
// same-doc guarantee — the (?m)^ anchors carry that load.
func recipeAttachesNicClusterPolicy(ctx context.Context, provider recipe.DataProvider, refs []recipe.ComponentRef) (bool, error) {
	for _, ref := range refs {
		paths := make([]string, 0, len(ref.PreManifestFiles)+len(ref.ManifestFiles))
		paths = append(paths, ref.PreManifestFiles...)
		paths = append(paths, ref.ManifestFiles...)
		for _, path := range paths {
			if ctx.Err() != nil {
				return false, wrapCtxErr(ctx, "probing NicClusterPolicy manifests")
			}
			content, err := recipe.GetManifestContentWithContext(ctx, provider, path)
			if err != nil {
				return false, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
					fmt.Sprintf("failed to read manifest %s while probing NicClusterPolicy", path))
			}
			// Per-doc ctx check so a large multi-doc manifest cannot
			// outlive a mid-scan cancellation before the next path
			// iteration observes it. The strings.Split is O(bytes),
			// the regex matches are O(bytes-per-doc), so this branch
			// is dominated by memory access rather than CPU.
			for _, doc := range strings.Split(string(content), "\n---\n") {
				if ctx.Err() != nil {
					return false, wrapCtxErr(ctx, fmt.Sprintf("scanning docs in manifest %s", path))
				}
				if ncpKindRE.MatchString(doc) && ncpAPIVersionRE.MatchString(doc) {
					return true, nil
				}
			}
		}
	}
	return false, nil
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
