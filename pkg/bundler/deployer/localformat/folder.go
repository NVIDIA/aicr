// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package localformat

// FolderKind classifies a written folder by the presence/absence of Chart.yaml.
type FolderKind int

const (
	// KindUpstreamHelm: folder contains no Chart.yaml; install.sh references
	// an upstream Helm chart via upstream.env.
	KindUpstreamHelm FolderKind = iota
	// KindLocalHelm: folder contains a generated Chart.yaml + templates/;
	// install.sh installs ./ as a local chart.
	KindLocalHelm
)

// String returns the stable textual name for the kind. Used by logs and
// golden-file diagnostics so diffs show kind names rather than integers.
func (k FolderKind) String() string {
	switch k {
	case KindUpstreamHelm:
		return "upstream-helm"
	case KindLocalHelm:
		return "local-helm"
	default:
		return "unknown"
	}
}

// Upstream holds upstream chart reference fields written to upstream.env.
type Upstream struct {
	Chart   string
	Repo    string
	Version string
}

// Folder describes one written folder. Returned by Write so callers
// (deployers) can generate orchestration files without re-classifying.
type Folder struct {
	Index    int    // 1-based; rendered as zero-padded 3-digit prefix in Dir
	Dir      string // e.g. "001-nfd"
	Kind     FolderKind
	Name     string    // component name, or "<name>-post" for injected
	Parent   string    // component this folder belongs to (== Name for primary)
	Upstream *Upstream // set iff Kind == KindUpstreamHelm
	Files    []string  // relative paths (to OutputDir) of files written in this folder
}
