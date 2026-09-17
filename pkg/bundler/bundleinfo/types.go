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

// BundleInfo is the bundle-root build record. Every bundle carries one,
// for every deployer, unconditionally.
//
// It answers three questions a bundle could not answer for itself: which
// deployer produced it, which aicr binary produced it, and which release
// landed in which directory. It deliberately does NOT restate component
// inventory — recipe.yaml sits beside it and is the source of truth for
// what the recipe resolved to.
type BundleInfo struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind" yaml:"kind"`
	Metadata   Metadata `json:"metadata" yaml:"metadata"`
	Build      Build    `json:"build" yaml:"build"`
	Layout     Layout   `json:"layout" yaml:"layout"`
}

// Metadata carries the identity of the build itself. Version is the aicr
// binary that ran `bundle`, which is not necessarily the one that resolved
// the recipe — see Recipe.Version.
//
// There is deliberately no timestamp. The record feeds checksums.txt, which
// is the subject of the bundle attestation, so a wall-clock field would make
// every bundle irreproducible.
type Metadata struct {
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

// Build records how the bundle was produced.
type Build struct {
	Deployer string   `json:"deployer" yaml:"deployer"`
	Recipe   Recipe   `json:"recipe" yaml:"recipe"`
	Settings Settings `json:"settings" yaml:"settings"`
}

// Recipe binds this record to the recipe.yaml beside it. Version is the aicr
// binary that resolved that recipe, read from its metadata.version; it is
// stamped by the recipe builder and is not restamped at bundle time.
type Recipe struct {
	Path    string `json:"path" yaml:"path"`
	Digest  string `json:"digest" yaml:"digest"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

// Settings records resolved effective bundler settings, admitted by one rule:
// a setting appears here only when its effect is already observable in the
// bundle's own files. Endpoints and security posture that never shape bundle
// content — Fulcio and Rekor URLs, the certificate identity pattern, registry
// TLS posture, the output target, the --config path — are excluded by
// construction. Free-form --set overrides are excluded too: values.yaml
// already carries their effect, and they are the one input that could be
// anything.
//
// Components is the positive component-name filter, reachable only through
// the REST ?bundlers= query parameter. It is named for what it holds rather
// than for Config.Bundlers behind it. It has the strongest claim of anything
// here: the bundle's recipe.yaml is written POST-filter, so without this a
// subset bundle is indistinguishable from an unfiltered bundle of a smaller
// recipe.
type Settings struct {
	Checksums          bool            `json:"checksums" yaml:"checksums"`
	Attested           bool            `json:"attested" yaml:"attested"`
	VendorCharts       bool            `json:"vendorCharts" yaml:"vendorCharts"`
	ReadinessHooks     bool            `json:"readinessHooks" yaml:"readinessHooks"`
	Serial             bool            `json:"serial" yaml:"serial"`
	Components         []string        `json:"components,omitempty" yaml:"components,omitempty"`
	RepoURL            string          `json:"repoURL,omitempty" yaml:"repoURL,omitempty"`
	TargetRevision     string          `json:"targetRevision,omitempty" yaml:"targetRevision,omitempty"`
	AppName            string          `json:"appName,omitempty" yaml:"appName,omitempty"`
	StorageClass       string          `json:"storageClass,omitempty" yaml:"storageClass,omitempty"`
	SharedStorageClass string          `json:"sharedStorageClass,omitempty" yaml:"sharedStorageClass,omitempty"`
	NodeScheduling     *NodeScheduling `json:"nodeScheduling,omitempty" yaml:"nodeScheduling,omitempty"`
}

// NodeScheduling groups the two placement classes the bundler pins.
type NodeScheduling struct {
	System      *Scheduling `json:"system,omitempty" yaml:"system,omitempty"`
	Accelerated *Scheduling `json:"accelerated,omitempty" yaml:"accelerated,omitempty"`
}

// Scheduling is one placement class.
type Scheduling struct {
	Selector    map[string]string `json:"selector,omitempty" yaml:"selector,omitempty"`
	Tolerations []Toleration      `json:"tolerations,omitempty" yaml:"tolerations,omitempty"`
}

// Toleration mirrors the fields of corev1.Toleration that the bundler sets.
// It is declared here rather than reused because corev1 types carry json tags
// only, and this artifact is serialized by yaml.v3, which reads yaml tags —
// reusing corev1 would emit Go field names into the record.
type Toleration struct {
	Key               string `json:"key,omitempty" yaml:"key,omitempty"`
	Operator          string `json:"operator,omitempty" yaml:"operator,omitempty"`
	Value             string `json:"value,omitempty" yaml:"value,omitempty"`
	Effect            string `json:"effect,omitempty" yaml:"effect,omitempty"`
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty" yaml:"tolerationSeconds,omitempty"`
}

// Layout indexes what the bundle emitted and where, so automation reads one
// key instead of branching on the deployer. Entrypoint is the file a consumer
// invokes or applies: deploy.sh, helmfile.yaml, app-of-apps.yaml, Chart.yaml,
// or kustomization.yaml.
type Layout struct {
	Entrypoint string    `json:"entrypoint" yaml:"entrypoint"`
	Provenance string    `json:"provenance,omitempty" yaml:"provenance,omitempty"`
	Releases   []Release `json:"releases" yaml:"releases"`
}

// Release is one Helm release the bundle installs.
//
// The index is keyed on releases rather than components because the injected
// -pre, -post and -readiness folders are releases with no component of their
// own; each names its parent in Component.
//
// Releases is emitted in deployment order and that ordering is normative:
// consumers read sequence from list position. There is deliberately no
// ordinal field — it would restate list position, restate the NNN- path
// prefix on the four deployers that have one, and imply a sequencing flux
// does not perform (flux orders by dependsOn, a graph rather than a line).
type Release struct {
	Name      string `json:"name" yaml:"name"`
	Component string `json:"component" yaml:"component"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Path      string `json:"path" yaml:"path"`
	Manifest  string `json:"manifest,omitempty" yaml:"manifest,omitempty"`
}
