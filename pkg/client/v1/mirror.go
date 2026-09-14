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

package aicr

import (
	"context"

	bundlerconfig "github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/mirror"
)

// Mirror inventory is the last SDK parity gap under #2016 (#2025). Air-gap
// tooling needs the set of images and charts a recipe references, and that was
// reachable only by importing pkg/mirror directly.
//
// Rendering is deliberately NOT part of this surface. YAML, JSON, Hauler and
// Zarf output stay in pkg/cli: they are presentation formats aimed at specific
// downstream tools, and freezing them here would commit the SDK to third-party
// schemas it does not control. Callers receive the data and render it however
// their pipeline needs.
//
// Trust maintenance (`aicr trust update`) stays CLI-only for the same kind of
// reason, recorded on #2025: the underlying TUF refresh cannot stop its
// background work when a caller cancels, so publishing it would hand SDK
// consumers a context that does not mean what contexts mean everywhere else in
// this package.

// MirrorInventory is the set of artifacts a recipe references.
//
// Facade-owned rather than an alias: pkg/mirror.MirrorList carries JSON and
// YAML struct tags that define the shape of the CLI's published output, so
// aliasing it would freeze that serialization as SDK contract.
type MirrorInventory struct {
	// Images is the global sorted, deduplicated set of container images.
	Images []string

	// Charts lists the Helm charts the recipe references.
	Charts []MirrorChart

	// Components breaks the images down by the component that references
	// them, including any non-fatal discovery warnings.
	Components []MirrorComponent

	// RecipeVersion is the CLI version that generated the recipe.
	RecipeVersion string

	// Criteria is a human-readable summary of the recipe criteria.
	Criteria string
}

// MirrorChart describes a Helm chart artifact a recipe needs.
type MirrorChart struct {
	Name       string
	Repository string
	Chart      string
	Version    string
	Namespace  string
}

// MirrorComponent groups discovered images by the component referencing them.
type MirrorComponent struct {
	Component string

	// Type is "helm" or "kustomize".
	Type string

	Images []string

	// Warnings records non-fatal problems found while discovering this
	// component, such as a chart that rendered with warnings. Discovery
	// succeeded; these are reported rather than raised so one noisy chart
	// does not fail an otherwise usable inventory.
	Warnings []string
}

// MirrorValueOverride sets one component value before discovery.
//
// Facade-owned rather than pkg/bundler/config.ComponentPath: exposing that type
// in a public signature would put an internal package in the frozen SDK
// surface, so every field it gains or loses becomes an SDK semver event for a
// package the SDK does not own.
type MirrorValueOverride struct {
	// Component is the value-override key for the component, matching what
	// `--set <component>:<path>=<value>` accepts.
	Component string

	// Path is the dotted path within that component's values.
	Path string

	// Value is the override. Nil marks the path dynamic (the `--dynamic`
	// form) rather than setting it, which is why this is a pointer: an empty
	// string is a legitimate value distinct from "no value".
	Value *string
}

// MirrorInventoryOption configures a mirror inventory request.
type MirrorInventoryOption func(*mirrorInventoryOptions)

type mirrorInventoryOptions struct {
	valueOverrides []MirrorValueOverride
	kubeVersion    string
}

// WithMirrorValueOverrides applies component value overrides before discovery.
//
// Overrides matter here because they change the answer: disabling a
// sub-component removes its images from the inventory, so a caller mirroring
// for an air-gapped install must pass the same overrides they will bundle with
// or they will mirror the wrong set.
func WithMirrorValueOverrides(overrides []MirrorValueOverride) MirrorInventoryOption {
	return func(o *mirrorInventoryOptions) {
		o.valueOverrides = overrides
	}
}

// WithMirrorKubeVersion pins the Kubernetes version charts render against.
//
// Charts branch on .Capabilities.KubeVersion, so an unset version can discover
// a different image set than the cluster will actually pull. When omitted, the
// version is derived from the recipe's own constraints.
func WithMirrorKubeVersion(version string) MirrorInventoryOption {
	return func(o *mirrorInventoryOptions) {
		o.kubeVersion = version
	}
}

// MirrorInventory discovers every container image and Helm chart a recipe
// references, by rendering its charts and scanning the resulting manifests.
//
// This performs real work: it renders every component's chart. Callers should
// pass a context with a deadline appropriate to the recipe's size.
func (c *Client) MirrorInventory(
	ctx context.Context,
	rec *RecipeResult,
	opts ...MirrorInventoryOption,
) (*MirrorInventory, error) {

	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if rec == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"recipe is required to discover mirror inventory")
	}

	// Discovery renders every component's chart, so a closed Client must be
	// rejected before that work starts, and an in-flight run must keep Close
	// from returning underneath it.
	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"aicr client not initialized (or already closed)")
	}
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	settings := &mirrorInventoryOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(settings)
		}
	}

	internal := rec.Resolved()
	if internal == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe could not be read")
	}

	// An explicit version wins; otherwise fall back to the recipe's own
	// constraints, which is what the CLI has always done.
	kubeVersion := settings.kubeVersion
	if kubeVersion == "" {
		kubeVersion = mirror.KubeVersionFromConstraints(internal.Constraints)
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()

	lister := mirror.NewLister(
		mirror.WithVersion(c.version),
		mirror.WithValueOverrides(toInternalOverrides(settings.valueOverrides)),
		mirror.WithKubeVersion(kubeVersion),
	)

	discovered, err := lister.Discover(ctx, internal)
	if err != nil {
		return nil, err
	}
	return wrapMirrorList(discovered), nil
}

// toInternalOverrides projects facade overrides onto the bundler's shape.
func toInternalOverrides(overrides []MirrorValueOverride) []bundlerconfig.ComponentPath {
	if overrides == nil {
		return nil
	}
	internal := make([]bundlerconfig.ComponentPath, 0, len(overrides))
	for _, override := range overrides {
		internal = append(internal, bundlerconfig.ComponentPath{
			Component: override.Component,
			Path:      override.Path,
			Value:     override.Value,
		})
	}
	return internal
}

// wrapMirrorList projects the internal discovery result onto facade types.
func wrapMirrorList(list *mirror.MirrorList) *MirrorInventory {
	if list == nil {
		return nil
	}

	inventory := &MirrorInventory{
		Images:        append([]string(nil), list.Images...),
		RecipeVersion: list.Metadata.RecipeVersion,
		Criteria:      list.Metadata.Criteria,
	}

	for _, chart := range list.Charts {
		inventory.Charts = append(inventory.Charts, MirrorChart{
			Name:       chart.Name,
			Repository: chart.Repository,
			Chart:      chart.Chart,
			Version:    chart.Version,
			Namespace:  chart.Namespace,
		})
	}

	for _, component := range list.Components {
		inventory.Components = append(inventory.Components, MirrorComponent{
			Component: component.Component,
			Type:      component.Type,
			Images:    append([]string(nil), component.Images...),
			Warnings:  append([]string(nil), component.Warnings...),
		})
	}
	return inventory
}
