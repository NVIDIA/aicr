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

// Package catalog provides the declarative validator catalog.
// The catalog defines which validator containers exist, what phase they belong to,
// and how they should be executed as Kubernetes Jobs.
package catalog

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"gopkg.in/yaml.v3"
)

// Re-exported types from pkg/api/validator/v1 for backward compatibility.
type (
	ValidatorCatalog     = v1.ValidatorCatalog
	CatalogMetadata      = v1.CatalogMetadata
	ValidatorEntry       = v1.ValidatorEntry
	ResourceRequirements = v1.ResourceRequirements
	EnvVar               = v1.EnvVar
)

// LoadWithDataProvider reads and parses the validator catalog from dp. A nil
// dp defaults to the embedded recipe data; callers that want a layered
// `--data` overlay must pass their own layered provider. When the catalog
// file is present, the external catalog is merged with the embedded one
// using merge-by-name semantics: external validators override embedded
// by name, and new validators are appended.
//
// Image tag resolution (applied in order):
//  1. If a catalog entry uses :latest and version looks like a release tag
//     published by on-tag.yaml (vX.Y.Z or vX.Y.Z-<prerelease>, but not the
//     goreleaser snapshot suffix -next), the tag is replaced with the CLI
//     version for reproducibility.
//  2. If version is a non-release dev build and commit is a valid short SHA,
//     the tag is replaced with :sha-<commit> to match on-push.yaml image tags.
//  3. If AICR_VALIDATOR_IMAGE_TAG is set, the resolved tag is overridden.
//     Useful for feature-branch dev builds whose commit SHA has no published
//     image (on-push.yaml only pushes SHA tags for commits merged to main).
//     Common value: `latest`.
//  4. If AICR_VALIDATOR_IMAGE_REGISTRY is set, the registry prefix is replaced.
//
// Entries with explicit version tags (e.g., :v1.2.3) are never modified by
// steps 1-2 but are replaced by step 3 if that env var is set.
func LoadWithDataProvider(dp recipe.DataProvider, version, commit string) (*ValidatorCatalog, error) {
	if dp == nil {
		dp = recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	}
	data, err := dp.ReadFile("validators/catalog.yaml")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read catalog", err)
	}

	cat, err := Parse(data)
	if err != nil {
		return nil, err
	}

	for i := range cat.Validators {
		cat.Validators[i].Image = ResolveImage(cat.Validators[i].Image, version, commit)
	}

	return cat, nil
}

// ResolveImage applies the same image rewriting that Load uses for catalog
// entries, exposed for external callers that hold image references outside the
// catalog (for example the inner AIPerf benchmark image referenced by the
// inference-perf validator). Applies, in order:
//
//  1. :latest tag replacement with version if version looks like a release
//     tag published by on-tag.yaml — strict (vX.Y.Z) or a pre-release
//     suffix (vX.Y.Z-rc1, vX.Y.Z-beta, vX.Y.Z-alpha.1). Goreleaser snapshot
//     strings (suffix -next) are NOT releases and fall through to step 2.
//  2. If non-release and commit is a valid SHA, :latest → :sha-<commit>.
//  3. Tag override if AICR_VALIDATOR_IMAGE_TAG is set (overrides steps 1-2
//     AND explicit catalog tags). Intended for feature-branch dev builds
//     where no :sha-<commit> image has been published; typical value:
//     `latest`.
//  4. Registry prefix override if AICR_VALIDATOR_IMAGE_REGISTRY is set.
//
// Images with explicit version tags are not modified by steps 1-2.
func ResolveImage(image, version, commit string) string {
	commit = strings.ToLower(commit)
	if isReleaseVersion(version) {
		image = replaceLatestTag(image, version)
	} else if isValidCommit(commit) {
		image = replaceLatestWithSHA(image, commit)
	}
	if tag := os.Getenv("AICR_VALIDATOR_IMAGE_TAG"); tag != "" {
		image = replaceTag(image, tag)
	}
	if override := os.Getenv("AICR_VALIDATOR_IMAGE_REGISTRY"); override != "" {
		image = replaceRegistry(image, override)
	}
	return image
}

// releaseVersionPattern matches the version strings on-tag.yaml turns into
// validator image tags: strict semver (vX.Y.Z) or a single pre-release
// suffix (vX.Y.Z-rc1, vX.Y.Z-beta, vX.Y.Z-alpha.1). The suffix is one
// segment of alphanumerics and dots — multi-segment forms like the git
// describe snapshot v0.0.0-12-gabc1234 contain an internal dash and do
// not match.
//
// Mirrors the on-tag.yaml trigger filter `v[0-9]+.[0-9]+.[0-9]+*`: any
// reference that filter accepts produces an image manifest tagged
// `:<github.ref_name>`, and the binary's version string is the same
// `.Tag` value goreleaser stamps in, so the two move in lockstep.
var releaseVersionPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[A-Za-z0-9.]+)?$`)

// snapshotSuffixPattern matches goreleaser's snapshot.version_template
// (`{{ .Tag }}-next` in .goreleaser.yaml). Snapshot builds produced by
// on-push.yaml stamp the binary with versions like `v0.13.0-next` —
// shape-equivalent to a pre-release tag but with NO corresponding image
// in ghcr. Excluding them keeps the main-development flow on the
// :sha-<commit> path (which on-push.yaml does publish).
//
// Do NOT remove this guard without also moving the main-push CI to
// publish `v<version>-next` image tags — the two coordinated changes
// must land together or :validate goes back to ImagePullBackOff on main.
var snapshotSuffixPattern = regexp.MustCompile(`-next$`)

// isReleaseVersion returns true when the version string matches a tag
// on-tag.yaml would publish (strict semver or pre-release) AND is not a
// goreleaser snapshot string. Dev builds, empty strings, and snapshots
// fall through to the :sha-<commit> resolution path.
func isReleaseVersion(version string) bool {
	if snapshotSuffixPattern.MatchString(version) {
		return false
	}
	return releaseVersionPattern.MatchString(version)
}

// replaceLatestTag replaces :latest with the given version tag.
// Images with explicit version tags are not modified.
// Ensures the tag has a "v" prefix to match the on-tag release workflow
// (GoReleaser strips the "v" from the version but tags keep it).
func replaceLatestTag(image, version string) string {
	if strings.HasSuffix(image, ":latest") {
		tag := version
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		return strings.TrimSuffix(image, ":latest") + ":" + tag
	}
	return image
}

// isValidCommit returns true for non-empty strings that look like a git short
// or full SHA (7-40 hex characters). The sentinel value "unknown" (set by
// ldflags default) is explicitly rejected.
func isValidCommit(commit string) bool {
	if commit == "" || commit == "unknown" {
		return false
	}
	if len(commit) < 7 || len(commit) > 40 {
		return false
	}
	for _, c := range commit {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// replaceTag forces the image's tag to newTag, regardless of what tag (if
// any) the image currently carries. Unlike replaceLatestTag / replaceLatestWithSHA,
// which only rewrite :latest, this helper supports the AICR_VALIDATOR_IMAGE_TAG
// env-var escape hatch: a user running a feature-branch dev build (where no
// :sha-<commit> image was published by on-push.yaml) can set the env var
// to `latest` and force every validator image to a published tag.
//
// Digest-pinned references (`name@sha256:…`) are cryptographic pins and are
// intentionally left untouched — a tag override is meaningless against a
// content-addressable ref, and naively rewriting would corrupt the digest.
// For non-digest refs, the tag separator is found as the last ':' that sits
// after the last '/' to avoid colliding with the registry port (`:5001` in
// `localhost:5001/...`).
func replaceTag(image, newTag string) string {
	if strings.Contains(image, "@") {
		// Digest-pinned ref (e.g. ghcr.io/foo/bar@sha256:deadbeef, or the
		// mixed form name:tag@sha256:…). The digest is the authoritative
		// pin; preserve it verbatim.
		return image
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash {
		// No tag on the image (just an image reference) — append one.
		return image + ":" + newTag
	}
	return image[:colon] + ":" + newTag
}

// replaceLatestWithSHA replaces :latest with :sha-<commit> to match the
// image tags pushed by the on-push CI workflow.
// Images with explicit version tags are not modified.
func replaceLatestWithSHA(image, commit string) string {
	if rest, ok := strings.CutSuffix(image, ":latest"); ok {
		return rest + ":sha-" + commit
	}
	return image
}

// Parse parses a catalog from raw YAML bytes. Exported for testing with
// inline catalogs without depending on the embedded file.
func Parse(data []byte) (*ValidatorCatalog, error) {
	var catalog ValidatorCatalog
	if err := yaml.Unmarshal(data, &catalog); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse catalog YAML", err)
	}

	if err := validate(&catalog); err != nil {
		return nil, err
	}

	return &catalog, nil
}

// validate checks the catalog for structural correctness.
// When Metadata is nil (embedded usage), APIVersion and Kind are optional.
// When Metadata is present (standalone file), APIVersion and Kind are required.
func validate(c *ValidatorCatalog) error {
	// Standalone file usage requires APIVersion and Kind
	if c.Metadata != nil {
		if c.APIVersion != v1.CatalogAPIVersion {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unsupported apiVersion %q, expected %q", c.APIVersion, v1.CatalogAPIVersion))
		}
		if c.Kind != v1.CatalogKind {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unsupported kind %q, expected %q", c.Kind, v1.CatalogKind))
		}
	}

	validPhases := map[string]bool{
		"deployment":  true,
		"performance": true,
		"conformance": true,
	}

	seen := make(map[string]bool)
	for i, v := range c.Validators {
		if v.Name == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator[%d]: name is required", i))
		}
		if seen[v.Name] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator[%d]: duplicate name %q", i, v.Name))
		}
		seen[v.Name] = true

		if !validPhases[v.Phase] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator %q: invalid phase %q, must be one of: deployment, performance, conformance", v.Name, v.Phase))
		}
		if v.Image == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("validator %q: image is required", v.Name))
		}
		for j, dep := range v.DependencyAffinity {
			if err := dep.Validate(); err != nil {
				return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
					fmt.Sprintf("validator %q: dependencyAffinity[%d]", v.Name, j))
			}
		}
	}

	return nil
}

// replaceRegistry replaces the registry prefix of an image reference.
// Example: replaceRegistry("ghcr.io/nvidia/aicr-validators/deployment:latest", "localhost:5001")
// returns "localhost:5001/aicr-validators/deployment:latest".
func replaceRegistry(image, newRegistry string) string {
	// Find the first path segment after the registry.
	// Registry is everything before the first "/" that contains a "." or ":"
	// (e.g., "ghcr.io/nvidia" or "localhost:5001").
	parts := strings.SplitN(image, "/", 3)
	if len(parts) < 3 {
		// Simple image like "registry/image:tag" — replace registry
		if len(parts) == 2 {
			return newRegistry + "/" + parts[1]
		}
		return image
	}
	// parts[0] = "ghcr.io", parts[1] = "nvidia", parts[2] = "aicr-validators/deployment:latest"
	// We want: newRegistry + "/" + parts[2]
	return newRegistry + "/" + parts[2]
}
