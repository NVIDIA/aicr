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

package plan

import (
	"fmt"
	"regexp"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// SchemaVersion is the plan document schema this operator writes. It reads
// every version in SupportedSchemaVersions (N and N-1).
const SchemaVersion = 1

// SupportedSchemaVersions are the plan schemas this operator can read.
var SupportedSchemaVersions = map[int]bool{1: true}

// Bounds from docs/operator/api.md#plan-configmap.
const (
	// MaxReleases bounds the releases in one plan.
	MaxReleases = 128
	// MaxReleaseIDLength is the Helm release-name limit.
	MaxReleaseIDLength = 53
	// MaxWarnings bounds recorded AICR bundle warnings.
	MaxWarnings = 64
)

// Engine names the deployment engine a plan targets.
type Engine string

// Engines.
const (
	EngineArgoCD Engine = "ArgoCD"
	EngineFlux   Engine = "Flux"
)

// Source records how a plan was produced.
type Source string

// Plan sources.
const (
	// SourceResolved plans come from AICR Jobs.
	SourceResolved Source = "Resolved"
	// SourceRebuilt plans are rebuilt from live engine objects.
	SourceRebuilt Source = "Rebuilt"
)

// Phase is a release's place in AICR's normative release layout.
type Phase string

// Release phases.
const (
	PhasePre       Phase = "Pre"
	PhasePrimary   Phase = "Primary"
	PhasePost      Phase = "Post"
	PhaseReadiness Phase = "Readiness"
)

// ChartSource distinguishes mirrored upstream charts from wrapper charts.
type ChartSource string

// Chart sources.
const (
	ChartMirrored ChartSource = "Mirrored"
	ChartWrapper  ChartSource = "Wrapper"
)

// Document is the plan: rendered desired state for one desiredStateDigest.
// It is operator-internal and stored in an immutable
// ConfigMap, never as an API kind.
type Document struct {
	SchemaVersion int    `json:"schemaVersion"`
	RuntimeUID    string `json:"runtimeUID"`
	Engine        Engine `json:"engine"`
	// EnginePolicy names the version of the policy fields the engine adapter
	// sets on every engine object; it is part of plan identity.
	EnginePolicy       string     `json:"enginePolicy"`
	DesiredStateDigest string     `json:"desiredStateDigest"`
	InputDigest        string     `json:"inputDigest,omitempty"`
	Provenance         Provenance `json:"provenance"`
	Recipe             *Recipe    `json:"recipe,omitempty"`
	Releases           []Release  `json:"releases"`
}

// Provenance records where a plan came from. It is evidence, not identity.
type Provenance struct {
	Source            Source `json:"source"`
	OperatorVersion   string `json:"operatorVersion,omitempty"`
	AICRVersion       string `json:"aicrVersion,omitempty"`
	AICRImage         string `json:"aicrImage,omitempty"`
	SnapshotDigest    string `json:"snapshotDigest,omitempty"`
	Bundle            string `json:"bundle,omitempty"`
	RuntimeGeneration int64  `json:"runtimeGeneration,omitempty"`
	// RenderFlags are the `aicr bundle` flags the render ran with, besides
	// the recipe and the output: with the recipe ConfigMap, the AICR CLI
	// renders the same bundle. Absent from a Rebuilt plan.
	RenderFlags []string `json:"renderFlags,omitempty"`
}

// Recipe identifies the AICR recipe a resolved plan was rendered from.
// Rebuilt plans have none. ConfigMapRef is the recipe Job's output, which
// upgrade-check and reproduce read; ValidationConfigMapRef is the recipe
// the bundle records after AICR filtered it by the render flags, which
// deployment validation reads. Plans written before it was recorded have
// no ValidationConfigMapRef. Neither ref is part of the plan's digest.
type Recipe struct {
	Name                   string            `json:"name"`
	Criteria               map[string]string `json:"criteria,omitempty"`
	ConfigMapRef           DigestedRef       `json:"configMapRef"`
	ValidationConfigMapRef *DigestedRef      `json:"validationConfigMapRef,omitempty"`
	Warnings               []string          `json:"warnings,omitempty"`
}

// Validation is the recipe deployment validation runs against: the
// bundle's, or the render recipe for a plan that records none.
func (r *Recipe) Validation() DigestedRef {
	if r.ValidationConfigMapRef != nil {
		return *r.ValidationConfigMapRef
	}
	return r.ConfigMapRef
}

// DigestedRef names a ConfigMap and the digest of its content.
type DigestedRef struct {
	ConfigMap string `json:"configMap"`
	Digest    string `json:"digest"`
}

// Release is one plan release.
type Release struct {
	ID              string      `json:"id"`
	Component       string      `json:"component"`
	Phase           Phase       `json:"phase"`
	ReadinessFor    string      `json:"readinessFor,omitempty"`
	DependsOn       []string    `json:"dependsOn,omitempty"`
	TargetNamespace string      `json:"targetNamespace"`
	Chart           Chart       `json:"chart"`
	Values          DigestedRef `json:"values"`
	OwnsCRDs        bool        `json:"ownsCRDs"`
}

// Chart is a chart served from the runtime registry, pinned by digest.
type Chart struct {
	Source     ChartSource    `json:"source"`
	Repository string         `json:"repository"`
	Version    string         `json:"version,omitempty"`
	Digest     string         `json:"digest"`
	Upstream   *UpstreamChart `json:"upstream,omitempty"`
}

// UpstreamChart records the verified upstream archive of a mirrored chart.
type UpstreamChart struct {
	Repository    string `json:"repository"`
	ArchiveDigest string `json:"archiveDigest"`
}

var (
	digestPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	uidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	releaseIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// Validate checks a document's shape and bounds. It does not check graph
// semantics beyond references (the planner owns ordering).
func (d *Document) Validate() error {
	invalid := func(format string, args ...any) error {
		return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(format, args...))
	}
	switch {
	case !SupportedSchemaVersions[d.SchemaVersion]:
		return invalid("unsupported plan schemaVersion %d", d.SchemaVersion)
	case !uidPattern.MatchString(d.RuntimeUID):
		return invalid("runtimeUID %q is not a UID", d.RuntimeUID)
	case d.Engine != EngineArgoCD && d.Engine != EngineFlux:
		return invalid("unknown engine %q", d.Engine)
	case d.EnginePolicy == "":
		return invalid("plan records no engine policy")
	case !digestPattern.MatchString(d.DesiredStateDigest):
		return invalid("desiredStateDigest %q is not a sha256 digest", d.DesiredStateDigest)
	case d.InputDigest != "" && !digestPattern.MatchString(d.InputDigest):
		return invalid("inputDigest %q is not a sha256 digest", d.InputDigest)
	case d.Provenance.Source != SourceResolved && d.Provenance.Source != SourceRebuilt:
		return invalid("unknown provenance source %q", d.Provenance.Source)
	case d.Provenance.Source == SourceResolved && d.Recipe == nil:
		return invalid("a resolved plan must record its recipe")
	case len(d.Releases) == 0 || len(d.Releases) > MaxReleases:
		return invalid("plan has %d releases; want 1 to %d", len(d.Releases), MaxReleases)
	}
	if d.Recipe != nil && len(d.Recipe.Warnings) > MaxWarnings {
		return invalid("plan records %d warnings; at most %d", len(d.Recipe.Warnings), MaxWarnings)
	}
	return d.validateReleases(invalid)
}

func (d *Document) validateReleases(invalid func(string, ...any) error) error {
	ids := make(map[string]bool, len(d.Releases))
	for i := range d.Releases {
		r := &d.Releases[i]
		if len(r.ID) > MaxReleaseIDLength || !releaseIDPattern.MatchString(r.ID) {
			return invalid("release ID %q is not a valid Helm release name", r.ID)
		}
		if ids[r.ID] {
			return invalid("release ID %q appears twice", r.ID)
		}
		ids[r.ID] = true
		switch r.Phase {
		case PhasePre, PhasePrimary, PhasePost:
		case PhaseReadiness:
			if r.ReadinessFor == "" {
				return invalid("readiness release %q names no parent", r.ID)
			}
		default:
			return invalid("release %q has unknown phase %q", r.ID, r.Phase)
		}
		if r.Chart.Source != ChartMirrored && r.Chart.Source != ChartWrapper {
			return invalid("release %q has unknown chart source %q", r.ID, r.Chart.Source)
		}
		if !digestPattern.MatchString(r.Chart.Digest) || !digestPattern.MatchString(r.Values.Digest) {
			return invalid("release %q must pin its chart and values by sha256 digest", r.ID)
		}
		// A rebuilt plan cannot know the upstream archive; a resolved one
		// verified it.
		if r.Chart.Source == ChartMirrored && d.Provenance.Source == SourceResolved &&
			(r.Chart.Upstream == nil || !digestPattern.MatchString(r.Chart.Upstream.ArchiveDigest)) {

			return invalid("mirrored release %q must record its verified upstream archive digest", r.ID)
		}
	}
	for i := range d.Releases {
		r := &d.Releases[i]
		for _, dep := range r.DependsOn {
			if !ids[dep] {
				return invalid("release %q depends on unknown release %q", r.ID, dep)
			}
		}
		if r.ReadinessFor != "" && !ids[r.ReadinessFor] {
			return invalid("readiness release %q names unknown parent %q", r.ID, r.ReadinessFor)
		}
	}
	if cycle := d.cycle(); cycle != "" {
		return invalid("release dependencies form a cycle through %q", cycle)
	}
	return nil
}

// cycle returns a release on a dependency cycle, or "".
func (d *Document) cycle() string {
	deps := map[string][]string{}
	for i := range d.Releases {
		deps[d.Releases[i].ID] = d.Releases[i].DependsOn
	}
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(id string) string
	visit = func(id string) string {
		switch state[id] {
		case visiting:
			return id
		case done:
			return ""
		}
		state[id] = visiting
		for _, dep := range deps[id] {
			if c := visit(dep); c != "" {
				return c
			}
		}
		state[id] = done
		return ""
	}
	for i := range d.Releases {
		if c := visit(d.Releases[i].ID); c != "" {
			return c
		}
	}
	return ""
}
