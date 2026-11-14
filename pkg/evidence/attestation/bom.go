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

package attestation

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"sort"
	"strings"

	cdx "github.com/CycloneDX/cyclonedx-go"

	"github.com/NVIDIA/aicr/pkg/bom"
	k8scollector "github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/internal/boundedio"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// LoadOrGenerateBOM returns the CycloneDX BOM bytes to embed. When bomPath
// is non-empty the path wins; otherwise BuildAutoBOM synthesizes a
// recipe-bound BOM. Helm charts are not rendered at validate time (would
// require the helm binary and a 60s+ budget); observed snapshot images
// cover the same information for the typical post-deployment flow.
//
// Deprecated: prefer LoadOrGenerateBOMContext. This form derives its own
// defaults.FileReadTimeout-bounded context; retained for source compatibility.
func LoadOrGenerateBOM(bomPath string, rec *recipe.RecipeResult, snap *snapshotter.Snapshot, validatorImages []ValidatorImage, version string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	return LoadOrGenerateBOMContext(ctx, bomPath, rec, snap, validatorImages, version)
}

// LoadOrGenerateBOMContext is LoadOrGenerateBOM bounded by the caller's
// context, so an operator-supplied --bom path on a dead mount cannot hang emit.
func LoadOrGenerateBOMContext(ctx context.Context, bomPath string, rec *recipe.RecipeResult, snap *snapshotter.Snapshot, validatorImages []ValidatorImage, version string) ([]byte, error) {
	if bomPath != "" {
		return readBOMFile(ctx, bomPath)
	}
	return BuildAutoBOM(rec, snap, validatorImages, version)
}

// readBOMFile reads bomPath into memory, bounded by defaults.MaxBOMBytes
// so an attacker-influenced path (e.g., /proc symlink, NFS mount) can't
// OOM the process before the body is parsed.
func readBOMFile(ctx context.Context, bomPath string) ([]byte, error) {
	return boundedio.ReadFile(ctx, bomPath, "BOM", defaults.MaxBOMBytes)
}

// BuildAutoBOM synthesizes a CycloneDX 1.6 BOM from the recipe's enabled
// component refs, validatorImages (as returned by
// ValidatorImagesForPredicate), and cluster-observed images. Observed images
// are registry-stripped because the constraint-evaluation collector strips
// them for mirror stability. A full-ref BOM still requires the --bom path.
func BuildAutoBOM(rec *recipe.RecipeResult, snap *snapshotter.Snapshot, validatorImages []ValidatorImage, version string) ([]byte, error) {
	if rec == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe is required for auto BOM")
	}
	results := make([]bom.ComponentResult, 0, len(rec.ComponentRefs)+1)
	for _, c := range rec.ComponentRefs {
		if !c.IsEnabled() {
			continue
		}
		// The pinned version lives in Version for Helm and Tag for Kustomize;
		// select the active field so a Kustomize component's tag is recorded
		// (and marked pinned) rather than dropped. pkg/bom names the CycloneDX
		// property by effective type.
		pin := c.Version
		if c.Type == recipe.ComponentTypeKustomize {
			pin = c.Tag
		}
		// Record the chart the component actually deploys: a source-only
		// Helm ref falls back to the component name (EffectiveChart, the
		// deployers' rule), so the digest-bound evidence identifies the
		// deployed chart instead of omitting aicr:helm:chart. Manifest-only
		// Helm refs and Kustomize refs stay chartless.
		chart := c.Chart
		if c.HasExternalChart() {
			chart = c.EffectiveChart()
		}
		results = append(results, bom.ComponentResult{
			Name:        c.Name,
			DisplayName: c.Name,
			Type:        string(c.Type),
			Repository:  c.Source,
			Chart:       chart,
			Version:     pin,
			Namespace:   c.Namespace,
			Pinned:      pin != "",
		})
	}

	if images := bomValidatorRefs(validatorImages); len(images) > 0 {
		results = append(results, bom.ComponentResult{
			Name:        "validators",
			DisplayName: "AICR validators",
			Type:        "validators",
			Images:      images,
		})
	}

	if observed := ObservedImagesFromSnapshot(snap); len(observed) > 0 {
		results = append(results, bom.ComponentResult{
			Name:        "observed-images",
			DisplayName: "Cluster-observed container images",
			Type:        "snapshot",
			Images:      observed,
		})
	}

	doc := bom.BuildBOM(bom.Metadata{
		Name:        RecipeNameFor(rec),
		Version:     version,
		Description: "Recipe-bound CycloneDX BOM auto-generated by aicr validate",
		ToolName:    "aicr",
		ToolVersion: version,
	}, results)

	var buf bytes.Buffer
	enc := cdx.NewBOMEncoder(&buf, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	if encErr := enc.EncodeVersion(doc, cdx.SpecVersion1_6); encErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to encode auto-generated BOM", encErr)
	}
	return buf.Bytes(), nil
}

// bomValidatorRefs returns one ref per distinct image and digest in images,
// in order. A recorded digest replaces any digest in the ref, so the ref
// names what ran.
func bomValidatorRefs(images []ValidatorImage) []string {
	var out []string
	seen := map[string]bool{}
	for _, img := range images {
		ref := img.Image
		if img.Digest != "" {
			name, _, _ := strings.Cut(ref, "@")
			ref = name + "@" + img.Digest
		}
		if ref != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}

// CatalogVersion returns the catalog metadata version, or "" when the
// catalog has no metadata block (legacy catalogs predate the field).
func CatalogVersion(cat *catalog.ValidatorCatalog) string {
	if cat == nil || cat.Metadata == nil {
		return ""
	}
	return cat.Metadata.Version
}

// UnpinnedValidatorImages returns, in the order given, the refs of images that
// have neither a Digest nor an immutable ref (see catalog.IsImmutableRef).
func UnpinnedValidatorImages(images []ValidatorImage) []string {
	var out []string
	for _, img := range images {
		if img.Image == "" || img.Digest != "" {
			continue
		}
		if !catalog.IsImmutableRef(img.Image) {
			out = append(out, img.Image)
		}
	}
	return out
}

// ValidatorImagesForPredicate returns the predicate's validatorImages: the
// images in cat of each phase in results with a test that was not skipped,
// and every other image a test in results reports. Each appears once per
// digest and platform recorded for it, sorted by platform and digest. It
// appears first with an empty Digest when no digest was recorded for it, when
// any record of it has none, or when a test of a check in cat ran with no
// digest recorded for that check's image, so UnpinnedValidatorImages still
// sees a run of a mutable ref with unknown provenance. Records failing
// ctrf.PodImage.Valid are ignored.
func ValidatorImagesForPredicate(cat *catalog.ValidatorCatalog, results []*validator.PhaseResult) []ValidatorImage {
	notSkipped := func(tr ctrf.TestResult) bool { return tr.Status != ctrf.StatusSkipped }
	ran := map[string]bool{}
	for _, pr := range results {
		if pr != nil && pr.Report != nil && slices.ContainsFunc(pr.Report.Results.Tests, notSkipped) {
			ran[string(pr.Phase)] = true
		}
	}
	type check struct{ phase, name string }
	var refs []string
	listed := map[string]bool{}
	checkImage := map[check]string{}
	if cat != nil {
		for _, v := range cat.Validators {
			if v.Image == "" {
				continue
			}
			checkImage[check{v.Phase, v.Name}] = v.Image
			if ran[v.Phase] && !listed[v.Image] {
				listed[v.Image] = true
				refs = append(refs, v.Image)
			}
		}
	}
	digested := map[string][]ValidatorImage{}
	undigested := map[string]bool{}
	seen := map[ValidatorImage]bool{}
	for _, pr := range results {
		if pr == nil || pr.Report == nil {
			continue
		}
		for _, tr := range pr.Report.Results.Tests {
			catalogImage := checkImage[check{string(pr.Phase), tr.Name}]
			catalogDigested := false
			for _, podImg := range tr.Images {
				if !podImg.Valid() {
					continue
				}
				if !listed[podImg.Image] {
					listed[podImg.Image] = true
					refs = append(refs, podImg.Image)
				}
				img := ValidatorImage(podImg)
				switch {
				case img.Digest == "":
					undigested[img.Image] = true
				case !seen[img]:
					seen[img] = true
					digested[img.Image] = append(digested[img.Image], img)
				}
				if img.Digest != "" && img.Image == catalogImage {
					catalogDigested = true
				}
			}
			// A check that ran with no digest for its own image, including
			// one whose pod could not be read, leaves that image unidentified.
			if catalogImage != "" && notSkipped(tr) && !catalogDigested {
				undigested[catalogImage] = true
			}
		}
	}

	if len(refs) == 0 {
		return nil
	}
	out := make([]ValidatorImage, 0, len(refs))
	for _, ref := range refs {
		imgs := digested[ref]
		slices.SortFunc(imgs, func(a, b ValidatorImage) int {
			return cmp.Or(strings.Compare(a.Platform, b.Platform), strings.Compare(a.Digest, b.Digest))
		})
		if len(imgs) == 0 || undigested[ref] {
			out = append(out, ValidatorImage{Image: ref})
		}
		out = append(out, imgs...)
	}
	return out
}

// ObservedImagesFromSnapshot returns cluster-observed image refs in
// "<name>:<tag>" form, deduplicated and sorted by name for deterministic
// output. Refs lack a registry because the collector strips registries
// for measurement-key stability across mirrors; auditors comparing the
// BOM against a specific registry should ship an explicit --bom path
// instead.
//
// Deterministic order matters: the auto-BOM bytes are signed via the
// predicate's bom.digest, so non-deterministic map iteration would break
// reproducible-build invariants (same recipe + same snapshot → same
// signed digest).
func ObservedImagesFromSnapshot(snap *snapshotter.Snapshot) []string {
	if snap == nil {
		return nil
	}
	var (
		seen   = map[string]struct{}{}
		images []string
	)
	for _, m := range snap.Measurements {
		if m == nil || m.Type != measurement.TypeK8s {
			continue
		}
		for _, st := range m.Subtypes {
			if st.Name != k8scollector.SubtypeImage {
				continue
			}
			names := make([]string, 0, len(st.Data))
			for name := range st.Data {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				reading := st.Data[name]
				if name == "" || reading == nil {
					continue
				}
				ref := name + ":" + reading.String()
				if _, dup := seen[ref]; dup {
					continue
				}
				seen[ref] = struct{}{}
				images = append(images, ref)
			}
		}
	}
	return images
}
