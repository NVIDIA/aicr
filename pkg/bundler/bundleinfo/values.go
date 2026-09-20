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

package bundleinfo

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// valuesFileName and clusterValuesFileName are the two files four of the five
// deployers write per release, layered in the order install.sh applies them
// (`-f values.yaml -f cluster-values.yaml`). The second carries install-time
// values and is usually empty.
const (
	valuesFileName        = "values.yaml"
	clusterValuesFileName = "cluster-values.yaml"

	// chartFileName is the wrapper chart a --vendor-charts bundle writes
	// beside those two. Its dependency names the subchart the values are
	// nested under.
	chartFileName = "Chart.yaml"
)

// injectedSuffixes name a release the bundler injected around a component
// rather than the component's own chart. Such a release carries its PARENT in
// Component, so counting its values would attribute them to a chart that never
// saw them.
var injectedSuffixes = []string{"-pre", "-post", "-readiness"}

// ReadReleaseValues returns the merged Helm values each release in dir was
// built with, keyed by component name.
//
// The values are read back rather than re-derived because a resolved recipe
// records valuesFile as a PATH, resolved against whichever binary reads it.
// Re-resolving would therefore read today's file for an artifact built months
// ago, so two artifacts would compare the same current values twice and a
// change between them would be invisible. What the bundle wrote is the only
// faithful record of what it installed.
//
// Locations come from bundle-info.yaml's releases, so this reads paths the
// bundle recorded about itself. That is not the deployer-layout fingerprinting
// upgrade-check refuses to do: nothing here guesses a filename from the shape
// of the directory. Four deployers write <path>/values.yaml with
// cluster-values.yaml layered over it; flux inlines the same values under
// spec.values in the HelmRelease named by <manifest>.
//
// A release with neither is omitted rather than reported: a manifest-only
// component has no chart values at all, and an injected -pre, -post or
// -readiness release is not its component's chart.
//
// Fails closed everywhere Read does, and for the same reason: the bundle
// arrived from an OCI registry or a GitOps clone. A missing bundle-info.yaml
// propagates Read's ErrCodeNotFound unchanged, so a caller can tell "this
// bundle predates build-record stamping" from "this bundle pinned no values".
func ReadReleaseValues(ctx context.Context, dir string) (map[string]map[string]any, error) {
	info, err := Read(ctx, dir)
	if err != nil {
		return nil, err
	}

	out := make(map[string]map[string]any, len(info.Layout.Releases))
	for i := range info.Layout.Releases {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		r := &info.Layout.Releases[i]
		if isInjectedRelease(r) {
			continue
		}
		values, found, valErr := releaseValues(dir, r, info.Build.Settings.VendorCharts)
		if valErr != nil {
			return nil, valErr
		}
		// Keyed on whether the release stated values at all, not on whether
		// they turned out to be empty. A component whose values file is
		// comments-only states an empty map, and that is a fact: it deployed
		// and pinned nothing. Collapsing it into "absent" would tell
		// inheritance this is a first deploy, so a name the next release adds
		// would apply instead of being held back.
		if !found {
			continue
		}
		out[r.Component] = values
	}
	return out, nil
}

// isInjectedRelease reports whether r is a wrapper the bundler placed around
// its component rather than the component's own chart.
func isInjectedRelease(r *Release) bool {
	for _, suffix := range injectedSuffixes {
		if r.Name == r.Component+suffix {
			return true
		}
	}
	return false
}

// releaseValues reads one release's merged values, preferring the values-file
// layout over the inlined one.
//
// The preference is not arbitrary. Argo writes BOTH a values.yaml and an
// Application that references it through spec.source.helm.valueFiles, so the
// file is the live document there and the manifest merely points at it.
// Reading the manifest first would work for flux and silently read a pointer
// for Argo.
// The bool reports whether the release stated values at all, which is a
// different question from whether they were empty; see ReadReleaseValues.
func releaseValues(dir string, r *Release, vendored bool) (map[string]any, bool, error) {
	values, found, err := readValuesFile(dir, r.Path, valuesFileName)
	if err != nil {
		return nil, false, err
	}
	if found {
		cluster, clusterFound, clusterErr := readValuesFile(dir, r.Path, clusterValuesFileName)
		if clusterErr != nil {
			return nil, false, clusterErr
		}
		if clusterFound {
			// Merged before un-nesting: both files nest under the same key,
			// so the two orders agree, and merging first keeps one code path.
			mergeValues(values, cluster)
		}
		if vendored {
			values, err = unnestVendoredValues(dir, r, values)
			if err != nil {
				return nil, false, err
			}
		}
		return values, true, nil
	}
	if r.Manifest == "" {
		return nil, false, nil
	}
	values, err = readManifestValues(dir, r.Manifest)
	if err != nil {
		return nil, false, err
	}
	if values == nil {
		// The manifest exists and declares no values, which is a statement.
		return map[string]any{}, true, nil
	}
	return values, true, nil
}

// unnestVendoredValues strips the subchart key a vendored bundle wraps its
// values in.
//
// --vendor-charts emits a wrapper chart per component and nests the values one
// level under the vendored chart's name, because Helm forwards a wrapper's
// values to a subchart only under that name. Read verbatim, every value path
// would therefore be one level too deep — "gpu-operator.fullnameOverride"
// where a resolved recipe says "fullnameOverride" — and comparing the two
// would report the real name as dropped and a phantom one as added.
//
// The key is read from the wrapper's own Chart.yaml dependency rather than
// guessed from the component name: the two differ whenever a component's chart
// is not named after it. A release with no wrapper dependency is not a
// vendored folder (an injected local-helm folder is not), so its values are
// already flat and are returned unchanged.
func unnestVendoredValues(dir string, r *Release, values map[string]any) (map[string]any, error) {
	subchart, err := vendoredSubchartName(dir, r.Path)
	if err != nil {
		return nil, err
	}
	if subchart == "" {
		return values, nil
	}
	nested, wrapped := values[subchart].(map[string]any)
	if !wrapped {
		// Nothing was nested under it, which is what an empty values file
		// produces: nestUnderSubchart emits no key at all for one.
		return values, nil
	}
	return nested, nil
}

// vendoredSubchartName returns the chart a vendored wrapper forwards its
// values to, or "" when the folder carries no wrapper Chart.yaml.
func vendoredSubchartName(dir, relDir string) (string, error) {
	data, found, err := readBounded(dir, filepath.Join(relDir, chartFileName))
	if err != nil || !found {
		return "", err
	}
	var chart struct {
		Dependencies []struct {
			Name string `yaml:"name"`
		} `yaml:"dependencies"`
	}
	if unmarshalErr := yaml.Unmarshal(data, &chart); unmarshalErr != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse %s in %s", chartFileName, relDir), unmarshalErr)
	}
	if len(chart.Dependencies) == 0 {
		return "", nil
	}
	return chart.Dependencies[0].Name, nil
}

// readValuesFile reads dir/relDir/name, reporting whether it existed. Absence
// is a state rather than a failure: three of the five layouts write no
// cluster-values.yaml, and flux writes no values.yaml at all.
func readValuesFile(dir, relDir, name string) (map[string]any, bool, error) {
	data, found, err := readBounded(dir, filepath.Join(relDir, name))
	if err != nil || !found {
		return nil, found, err
	}
	var values map[string]any
	if unmarshalErr := yaml.Unmarshal(data, &values); unmarshalErr != nil {
		return nil, false, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse %s in %s", name, relDir), unmarshalErr)
	}
	if values == nil {
		// An AICR-generated cluster-values.yaml is a header and a document
		// separator with nothing under it, which decodes to nil.
		values = make(map[string]any)
	}
	return values, true, nil
}

// readManifestValues pulls spec.values out of the HelmRelease (flux) the
// record names. Only that one path is read: the manifest is a deployer
// resource whose remaining fields this package has no business interpreting.
func readManifestValues(dir, manifest string) (map[string]any, error) {
	data, found, err := readBounded(dir, manifest)
	if err != nil || !found {
		return nil, err
	}
	var doc struct {
		Spec struct {
			Values map[string]any `yaml:"values"`
		} `yaml:"spec"`
	}
	if unmarshalErr := yaml.Unmarshal(data, &doc); unmarshalErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse %s", manifest), unmarshalErr)
	}
	return doc.Spec.Values, nil
}

// readBounded opens dir/rel under the same guards as Read: lexically joined,
// never followed through a symlink, regular files only, and size-capped.
func readBounded(dir, rel string) ([]byte, bool, error) {
	path, joinErr := deployer.SafeJoin(dir, rel)
	if joinErr != nil {
		return nil, false, errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest,
			"unsafe bundle values path")
	}

	f, err := os.OpenFile( //nolint:gosec // path validated by SafeJoin
		path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if stderrors.Is(err, syscall.ELOOP) {
			return nil, false, errors.Wrap(errors.ErrCodeInvalidRequest,
				"refusing to follow file symlink "+path, err)
		}
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, errors.Wrap(errors.ErrCodeInternal, "failed to open bundle values", err)
	}
	defer func() { _ = f.Close() }()

	opened, err := f.Stat()
	if err != nil {
		return nil, false, errors.Wrap(errors.ErrCodeInternal, "failed to inspect opened bundle values", err)
	}
	if !opened.Mode().IsRegular() {
		return nil, false, errors.New(errors.ErrCodeInvalidRequest, "bundle values is not a regular file: "+path)
	}

	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxBundleValuesBytes+1))
	if err != nil {
		return nil, false, errors.Wrap(errors.ErrCodeInternal, "failed to read bundle values", err)
	}
	if int64(len(data)) > defaults.MaxBundleValuesBytes {
		return nil, false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s exceeds the %d-byte limit", rel, defaults.MaxBundleValuesBytes))
	}
	return data, true, nil
}

// mergeValues layers src over dst the way Helm coalesces a second -f file:
// maps merge key by key, everything else replaces, and an explicit null
// removes the key.
func mergeValues(dst, src map[string]any) {
	for key, srcVal := range src {
		if srcVal == nil {
			delete(dst, key)
			continue
		}
		if srcMap, srcOK := srcVal.(map[string]any); srcOK {
			if dstMap, dstOK := dst[key].(map[string]any); dstOK {
				mergeValues(dstMap, srcMap)
				continue
			}
		}
		dst[key] = srcVal
	}
}
