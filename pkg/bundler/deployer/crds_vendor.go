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

package deployer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"aicr.run/pkg/artifact"
	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/recipe"
	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
)

// chartsDir is the directory a Helm chart keeps its subcharts in.
const chartsDir = "charts"

// CheckVendoredCRDOwnership verifies retained ownership against the chart
// archives the bundle vendors. Archives maps selected component names to the
// archive bytes written to the bundle.
func CheckVendoredCRDOwnership(ctx context.Context, result *recipe.RecipeResult, archives map[string][]byte) error {
	provenance := result.Metadata.Artifacts
	if provenance == nil || provenance.CRDOwnership == nil || provenance.CRDOwnership.Policy != artifact.CRDPolicyRetained || len(archives) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()
	owners, exclusions, err := ResolveCRDPolicy(ctx, result.DataProvider(), result.ComponentRefs)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(archives))
	for name := range archives {
		names = append(names, name)
	}
	slices.Sort(names)
	sets := map[string]map[string]bool{}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "check vendored CRDs")
		}
		set, err := readVendoredCRDs(ctx, archives[name], exclusions[name])
		if err != nil {
			return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "read vendored CRDs for "+name)
		}
		sets[name] = set
	}
	for _, owner := range names {
		if !owners[owner] {
			continue
		}
		crds := make([]string, 0, len(sets[owner]))
		for crd := range sets[owner] {
			crds = append(crds, crd)
		}
		slices.Sort(crds)
		for _, crd := range crds {
			if err := ctx.Err(); err != nil {
				return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "compare vendored CRDs")
			}
			if sets[owner][crd] {
				return crdOwnershipConflict(crd, owner, owner, "uses webhook conversion")
			}
			for _, other := range names {
				if _, found := sets[other][crd]; other != owner && found {
					return crdOwnershipConflict(crd, owner, other, "is shipped by both components")
				}
			}
		}
	}
	return nil
}

func crdOwnershipConflict(crd, owner, other, detail string) error {
	return errors.NewWithContext(errors.ErrCodeConflict,
		fmt.Sprintf("crd-ownership-conflict: CRD %s owned by %s %s (%s)", crd, owner, detail, other),
		map[string]any{"reason": "crd-ownership-conflict", "crd": crd, "owner": owner, "component": other})
}

func readVendoredCRDs(ctx context.Context, data []byte, exclusions []string) (map[string]bool, error) {
	if int64(len(data)) > defaults.HelmChartArtifactLimit {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "vendored chart exceeds size limit")
	}
	set := map[string]bool{}
	budget := defaults.HelmChartArtifactLimit
	if err := collectVendoredCRDs(ctx, data, exclusions, set, &budget, 0); err != nil {
		return nil, err
	}
	return set, nil
}

func collectVendoredCRDs(ctx context.Context, data []byte, exclusions []string, set map[string]bool, budget *int64, depth int) error {
	if depth > defaults.ArtifactDocumentDepth {
		return errors.New(errors.ErrCodeInvalidRequest, "chart nesting exceeds limit")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "decode chart gzip", err)
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: *budget + 1}
	tr := tar.NewReader(limited)
	for {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "read chart CRDs")
		}
		before := limited.N
		header, nextErr := tr.Next()
		*budget -= before - limited.N
		if *budget < 0 {
			return errors.New(errors.ErrCodeInvalidRequest, "expanded charts exceed size limit")
		}
		if stderrors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "read chart archive", nextErr)
		}
		// Helm skips a pax global header, which git archive writes.
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return errors.New(errors.ErrCodeInvalidRequest, "chart archive contains a non-regular entry: "+header.Name)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		crd, nested := chartRootEntry(header.Name, exclusions)
		if !nested && !crd {
			continue
		}
		limit := defaults.MaxExternalDataFileBytes
		if nested {
			limit = defaults.HelmChartArtifactLimit
		}
		if header.Size > limit || header.Size > *budget {
			return errors.New(errors.ErrCodeInvalidRequest, "chart entry exceeds size limit")
		}
		before = limited.N
		content, readErr := io.ReadAll(tr)
		*budget -= before - limited.N
		if readErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "read chart entry", readErr)
		}
		if nested {
			if err := collectVendoredCRDs(ctx, content, exclusions, set, budget, depth+1); err != nil {
				return err
			}
			// Nested expansion consumes the same total budget as the parent archive.
			limited.N = *budget + 1
		} else if err := decodeVendoredCRDs(ctx, content, set); err != nil {
			return err
		}
	}
}

// chartRootEntry classifies an archive entry by where the bundle reads it: a
// CRD file under a chart root's crds/ directory, or a packaged chart under a
// charts/ directory. Paths such as templates/crds/ are neither, since Helm
// renders those as templates. Entries of an excluded subchart are neither.
//
// The result covers what Helm installs and what apply-crds.sh applies on
// upgrade. The script reads more than Helm: it keeps charts/ entries whose
// names start with "_" or ".", and unpacks every .tgz below a charts/
// directory wherever that directory sits.
func chartRootEntry(name string, exclusions []string) (crd, nested bool) {
	// As Helm does: a name with a backslash is split on backslashes, and the
	// raw first segment is dropped before cleaning.
	delimiter := "/"
	if strings.Contains(name, `\`) {
		delimiter = `\`
	}
	root, name, _ := strings.Cut(name, delimiter)
	parts := strings.Split(path.Clean(strings.ReplaceAll(name, delimiter, "/")), "/")
	last := parts[len(parts)-1]
	packaged := strings.HasSuffix(last, ".tgz") && (root == chartsDir || slices.Contains(parts[:len(parts)-1], chartsDir))
	for len(parts) >= 2 && parts[0] == chartsDir {
		if excludedSubchart(parts[1], exclusions) {
			return false, false
		}
		if len(parts) == 2 {
			break
		}
		parts = parts[2:]
	}
	if packaged {
		return false, !excludedSubchart(last, exclusions)
	}
	if len(parts) < 2 || parts[0] != "crds" {
		return false, false
	}
	ext := strings.ToLower(path.Ext(name))
	return ext == ".yaml" || ext == ".yml" || ext == ".json", false
}

// excludedSubchart reports whether a charts/ entry, an unpacked directory or
// a packaged <name>-<version>.tgz, is one of the excluded subcharts.
func excludedSubchart(entry string, exclusions []string) bool {
	for _, excluded := range exclusions {
		if entry == excluded {
			return true
		}
		version, ok := strings.CutPrefix(entry, excluded+"-")
		if !ok || !strings.HasSuffix(version, ".tgz") {
			continue
		}
		// A full version, so mongodb-store-2-1.0.0.tgz is chart mongodb-store-2.
		// Helm also accepts the abbreviated x and x.y forms.
		version = strings.TrimSuffix(version, ".tgz")
		for _, pad := range []string{"", ".0", ".0.0"} {
			if _, err := semver.StrictNewVersion(version + pad); err == nil {
				return true
			}
		}
	}
	return false
}

// vendoredCRDDocument is the part of a crds/ manifest the check reads. Items
// carries the members of a List, which Helm flattens on install.
type vendoredCRDDocument struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Conversion struct {
			Strategy string `yaml:"strategy"`
		} `yaml:"conversion"`
	} `yaml:"spec"`
	Items []vendoredCRDDocument `yaml:"items"`
}

func (d *vendoredCRDDocument) record(set map[string]bool) error {
	for i := range d.Items {
		if err := d.Items[i].record(set); err != nil {
			return err
		}
	}
	if d.Kind != "CustomResourceDefinition" {
		return nil
	}
	if d.Metadata.Name == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "chart CRD has no metadata.name")
	}
	set[d.Metadata.Name] = set[d.Metadata.Name] || d.Spec.Conversion.Strategy == "Webhook"
	return nil
}

func decodeVendoredCRDs(ctx context.Context, data []byte, set map[string]bool) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "decode chart CRDs")
		}
		var document vendoredCRDDocument
		err := decoder.Decode(&document)
		if stderrors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "decode chart CRD", err)
		}
		if err := document.record(set); err != nil {
			return err
		}
	}
}
