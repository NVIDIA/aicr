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

package helmfile

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Helmfile is the top-level document marshaled to helmfile.yaml.
// Field order in this struct controls field order in the rendered YAML
// (gopkg.in/yaml.v3 walks struct fields in declaration order), so the
// rendered file follows the layout used in helmfile community examples:
// repositories → helmDefaults → releases.
type Helmfile struct {
	Repositories []Repository `yaml:"repositories,omitempty"`
	HelmDefaults HelmDefaults `yaml:"helmDefaults"`
	Releases     []Release    `yaml:"releases"`
}

// Repository is one entry in helmfile's top-level repositories: list.
// Helmfile resolves release.chart values of the form "<name>/<chart>"
// against this list at apply time.
type Repository struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	OCI  bool   `yaml:"oci,omitempty"`
}

// HelmDefaults carries the cluster-wide helm flags applied to every release
// unless the release overrides them. Mirrors HELM_TIMEOUT="10m" in
// pkg/bundler/deployer/helm/templates/deploy.sh.tmpl.
type HelmDefaults struct {
	Wait            bool `yaml:"wait"`
	Timeout         int  `yaml:"timeout"`
	CreateNamespace bool `yaml:"createNamespace"`
}

// Release is one entry in helmfile's releases: list. One Release is
// emitted per NNN-<name>/ folder produced by localformat.Write —
// including injected -pre and -post folders.
type Release struct {
	Name      string   `yaml:"name"`
	Namespace string   `yaml:"namespace"`
	Chart     string   `yaml:"chart"`
	Version   string   `yaml:"version,omitempty"`
	Values    []string `yaml:"values,omitempty"`
	Needs     []string `yaml:"needs,omitempty"`
	// Wait is rendered only when explicitly set to false (an async
	// component); helmDefaults.wait: true applies otherwise.
	Wait *bool `yaml:"wait,omitempty"`
	// Timeout is rendered only when the release overrides
	// helmDefaults.timeout (e.g., kai-scheduler 20m).
	Timeout int `yaml:"timeout,omitempty"`
	// CreateNamespace is rendered only when explicitly set to false —
	// the chart ships its own Namespace template, so helm must not
	// create the namespace out-of-band. helmDefaults.createNamespace
	// (true) applies otherwise. Pointer so a deliberate false survives
	// YAML marshaling and a nil value omits the field entirely.
	CreateNamespace *bool `yaml:"createNamespace,omitempty"`
}

// overrides carries per-component helm flag overrides.
type overrides struct {
	wait    bool
	timeout int // seconds; 0 means "use helmDefaults.timeout"
}

// componentOverrides mirrors the special cases hardcoded in
// pkg/bundler/deployer/helm/templates/deploy.sh.tmpl
// (ASYNC_COMPONENTS and the COMPONENT_HELM_TIMEOUT case block).
// The two files must be updated together; promoting these into the recipe
// schema is tracked as a follow-up to issue #632.
var componentOverrides = map[string]overrides{
	"kai-scheduler": {wait: false, timeout: 20 * 60},
}

// defaultHelmDefaults returns the cluster-wide defaults applied to every
// release. Matches HELM_TIMEOUT="10m" + the helm deployer's implicit
// --wait behavior.
func defaultHelmDefaults() HelmDefaults {
	return HelmDefaults{
		Wait:            true,
		Timeout:         10 * 60,
		CreateNamespace: true,
	}
}

// buildHelmfile walks the folders produced by localformat.Write in
// deployment order and emits one Release per folder, plus the deduplicated
// repositories: list referenced by upstream-helm releases.
// namespaceByComponent maps the recipe ComponentRef.Name → namespace, so
// injected -pre / -post folders inherit the parent component's namespace.
// dynamicValues maps ComponentRef.Name → dynamic value paths; presence
// drives whether cluster-values.yaml is referenced in the release values:.
func buildHelmfile(folders []localformat.Folder, namespaceByComponent map[string]string, dynamicValues map[string][]string) (Helmfile, error) {
	releases := make([]Release, 0, len(folders))
	repoSet := make(map[string]Repository) // keyed by URL for dedupe

	// Carry the predecessor's namespace alongside its name so cross-namespace
	// needs: entries are emitted as "<namespace>/<name>". Helmfile resolves a
	// bare "<name>" only within the dependent release's own namespace, so a
	// release in cert-manager/ pointing at agentgateway-crds-post (which
	// lives in agentgateway-system/) silently fails to resolve.
	var prevName, prevNamespace string
	for _, f := range folders {
		ns := namespaceByComponent[f.Parent]
		rel := Release{
			Name:      f.Name,
			Namespace: ns,
			Values:    valuesFilesForFolder(f, dynamicValues[f.Parent]),
		}
		if prevName != "" {
			rel.Needs = []string{needsRef(ns, prevNamespace, prevName)}
		}

		switch f.Kind {
		case localformat.KindUpstreamHelm:
			if f.Upstream == nil {
				return Helmfile{}, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("folder %s is KindUpstreamHelm but Upstream is nil", f.Dir))
			}
			repo, alias := repositoryFor(f.Upstream)
			repoSet[repo.URL] = repo
			rel.Chart = alias + "/" + f.Upstream.Chart
			rel.Version = f.Upstream.Version
		case localformat.KindLocalHelm:
			rel.Chart = "./" + f.Dir
		default:
			return Helmfile{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unsupported folder kind %v for %s", f.Kind, f.Dir))
		}

		// Per-component overrides (async / longer timeout). Keyed by
		// f.Parent so primary + injected -pre / -post inherit the same
		// override as their parent component.
		if ov, ok := componentOverrides[f.Parent]; ok {
			if !ov.wait {
				waitFalse := false
				rel.Wait = &waitFalse
			}
			if ov.timeout > 0 {
				rel.Timeout = ov.timeout
			}
		}

		// Override the global helmDefaults.createNamespace when this
		// folder's chart owns the target Namespace (Talos privileged-
		// namespace pre-injection). Without this, helmfile passes
		// --create-namespace and helm creates the namespace out-of-band;
		// the chart's later Namespace template then collides because
		// the existing namespace lacks the release's ownership labels.
		// Only emit the override when false — the global default
		// already covers the true case and we want helmfile.yaml
		// minimal.
		if !f.CreateNamespace {
			falseVal := false
			rel.CreateNamespace = &falseVal
		}

		releases = append(releases, rel)
		prevName = rel.Name
		prevNamespace = ns
	}

	// Stable repository ordering: by alias name.
	repos := make([]Repository, 0, len(repoSet))
	for _, r := range repoSet {
		repos = append(repos, r)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })

	return Helmfile{
		Repositories: repos,
		HelmDefaults: defaultHelmDefaults(),
		Releases:     releases,
	}, nil
}

// needsRef formats a release reference for the `needs:` list. Helmfile
// resolves a bare "<name>" only within the dependent release's own
// namespace, so cross-namespace edges must be qualified as
// "<namespace>/<name>". Matching namespaces are emitted as bare names to
// keep the helmfile.yaml minimal and to match the helmfile community
// convention for in-namespace dependency chains.
func needsRef(dependentNS, depNS, depName string) string {
	if depNS == "" || depNS == dependentNS {
		return depName
	}
	return depNS + "/" + depName
}

// valuesFilesForFolder returns the list of values files (relative to
// helmfile.yaml's parent dir) for f. Always includes values.yaml; appends
// cluster-values.yaml when the parent component has dynamic value paths
// configured (matching the localformat split that wrote the file).
//
// Injected -pre / -post folders inherit the parent's DynamicValues lookup
// by design: localformat.writeLocalHelmFolder copies the parent's split
// cluster-values.yaml into each auxiliary folder so all three releases
// (pre, primary, post) see the same per-cluster overrides. The dynamic
// keys are usually only meaningful to the primary chart, but referencing
// the file uniformly keeps a partial-cluster-values edit from being
// silently ignored by the wrapped releases.
//
// Paths are written with an explicit "./" prefix to match the chart: form
// used for KindLocalHelm folders. Helmfile accepts both forms; the prefix
// is purely a stylistic choice for visual consistency in golden files.
func valuesFilesForFolder(f localformat.Folder, dynamicPaths []string) []string {
	values := []string{"./" + f.Dir + "/values.yaml"}
	if len(dynamicPaths) > 0 {
		values = append(values, "./"+f.Dir+"/cluster-values.yaml")
	}
	return values
}

// repositoryFor builds a Repository entry for the given upstream reference,
// returning the entry plus the helmfile alias used in release.chart.
// Aliases are derived from the URL so the same chart repository always
// produces the same alias even when referenced by multiple components.
//
// Helmfile's OCI repository convention takes a bare host+path in `url` and
// a separate `oci: true` flag — passing `oci://host/path` along with
// `oci: true` causes helmfile to prepend the scheme a second time
// (`oci://oci://...`), which fails chart resolution. Strip the scheme
// when the repository is OCI so the rendered document matches what
// `helmfile build` expects.
func repositoryFor(u *localformat.Upstream) (Repository, string) {
	alias := sanitizeRepoAlias(u.Repo)
	oci := strings.HasPrefix(u.Repo, "oci://")
	url := u.Repo
	if oci {
		url = strings.TrimPrefix(url, "oci://")
	}
	return Repository{Name: alias, URL: url, OCI: oci}, alias
}

// nonAlphanumericRe collapses runs of non-DNS characters into a single
// hyphen for the alias slug.
var nonAlphanumericRe = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeRepoAlias converts a chart-repo URL into a stable, helmfile-safe
// alias. Mirrors the convention in pkg/bundler/deployer/flux
// (sanitizeSourceName) so a helmfile bundle and a flux bundle for the
// same recipe produce comparable identifiers.
func sanitizeRepoAlias(rawURL string) string {
	s := strings.ToLower(rawURL)
	for _, prefix := range []string{"oci://", "https://", "http://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	s = strings.Trim(nonAlphanumericRe.ReplaceAllString(s, "-"), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	if s == "" {
		return "default-repo"
	}
	return s
}
