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

package flux

import (
	"strings"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// GitRepoSourceData carries data for GitRepository source CRs.
type GitRepoSourceData struct {
	Name   string
	URL    string
	Branch string
}

// collectGitSources collects unique GitRepository sources.
// The default repoURL is always added so manifest components have a source.
func collectGitSources(defaultRepoURL, targetRevision string) map[string]*GitRepoSourceData {
	sources := make(map[string]*GitRepoSourceData)
	sources[defaultRepoURL] = &GitRepoSourceData{
		Name:   sanitizeSourceName(defaultRepoURL),
		URL:    defaultRepoURL,
		Branch: targetRevision,
	}
	return sources
}

// gitSourceName returns the sanitized name for a Git source URL.
func gitSourceName(sourceURL string, sources map[string]*GitRepoSourceData) string {
	return sourceName(sourceURL, sources, func(s *GitRepoSourceData) string { return s.Name })
}

// collectHelmSources collects unique HelmRepository sources from components.
// When vendorCharts is true, components that will be vendored are skipped
// because their HelmRelease CRs reference GitRepository instead.
func collectHelmSources(refs []recipe.ComponentRef, vendorCharts bool) map[string]*HelmRepoSourceData {
	sources := make(map[string]*HelmRepoSourceData)
	for _, ref := range refs {
		if ref.Type != recipe.ComponentTypeHelm || ref.Source == "" {
			continue
		}
		if vendorCharts && isVendorable(ref) {
			continue
		}
		if _, exists := sources[ref.Source]; exists {
			continue
		}
		sources[ref.Source] = &HelmRepoSourceData{
			Name:  sanitizeSourceName(ref.Source),
			URL:   ref.Source,
			IsOCI: strings.HasPrefix(ref.Source, "oci://"),
		}
	}
	return sources
}

// helmSourceName returns the sanitized name for a Helm source URL.
func helmSourceName(sourceURL string, sources map[string]*HelmRepoSourceData) string {
	return sourceName(sourceURL, sources, func(s *HelmRepoSourceData) string { return s.Name })
}
