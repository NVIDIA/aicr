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

package recipe

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

// ComponentSizingKind is the kind expected on a ComponentSizing document (ADR-030).
const ComponentSizingKind = "ComponentSizing"

// SizingBasisGPUNodes keys tiers on the GPU node count, the meaning of --nodes.
const SizingBasisGPUNodes = "gpuNodes"

// Sizing field names, as tiers set them and measurements name them in sets.
const (
	SizingFieldRequests = "requests"
	SizingFieldLimits   = "limits"
)

// Measurement kinds. An instantaneous reading is a single sample, so it
// understates the peak a windowed one would have seen.
const (
	SizingKindWindowed      = "windowed"
	SizingKindInstantaneous = "instantaneous"
)

const sizingDateLayout = "2006-01-02"

// SizingConfig references a component's ComponentSizing document.
type SizingConfig struct {
	// File is relative to the data directory. Empty means the component is
	// never sized.
	File string `yaml:"file,omitempty"`
}

// ComponentSizing lowers a component's memory resources by node-count tier.
// Values above the highest tier, or with no count, are the values files'
// own, so the document holds only lower tiers.
type ComponentSizing struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Basis      string           `yaml:"basis"`
	Resources  []SizingResource `yaml:"resources"`
}

// SizingResource is the resources block at Path, written as --set writes it.
// Its Tiers ascend by MaxNodes.
type SizingResource struct {
	Path  string       `yaml:"path"`
	Tiers []SizingTier `yaml:"tiers"`
}

// SizingTier covers counts above the next lower tier's MaxNodes, up to its
// own. A tier with neither Requests nor Limits marks a bound only.
type SizingTier struct {
	MaxNodes     int                 `yaml:"maxNodes"`
	Requests     *SizingQuantities   `yaml:"requests,omitempty"`
	Limits       *SizingQuantities   `yaml:"limits,omitempty"`
	Measurements []SizingMeasurement `yaml:"measurements,omitempty"`
}

// SizingQuantities holds Kubernetes quantities; only memory is sized.
type SizingQuantities struct {
	Memory string `yaml:"memory"`
}

// SizingMeasurement is one reading behind a tier. Sets is empty for a reading
// that only constrained the result. TruncatedAt is the limit in force while
// the container was being OOM-killed, which caps what Max could show. Nodes
// is required on a windowed reading that sets a limit. Version, optional, is
// the component's chart version on the measured cluster. Ref, optional,
// links the raw data.
type SizingMeasurement struct {
	Sets        []string `yaml:"sets"`
	Kind        string   `yaml:"kind"`
	Window      string   `yaml:"window,omitempty"`
	GPUNodes    int      `yaml:"gpuNodes"`
	Nodes       int      `yaml:"nodes,omitempty"`
	Pods        int      `yaml:"pods,omitempty"`
	P90         string   `yaml:"p90,omitempty"`
	Max         string   `yaml:"max"`
	TruncatedAt string   `yaml:"truncatedAt,omitempty"`
	Date        string   `yaml:"date"`
	Version     string   `yaml:"version,omitempty"`
	Ref         string   `yaml:"ref,omitempty"`
}

// TierFor returns the tier covering n GPU nodes. It reports false for a
// non-positive n and for n above the highest bound, which keep the values
// files' own values.
func (r *SizingResource) TierFor(n int) (*SizingTier, bool) {
	if n <= 0 {
		return nil, false
	}
	for i := range r.Tiers {
		if n <= r.Tiers[i].MaxNodes {
			return &r.Tiers[i], true
		}
	}
	return nil, false
}

// LoadComponentSizing reads and validates the sizing document at file for
// component. It fails closed: a document that cannot be read or breaks a rule
// is an error, never treated as absent.
func LoadComponentSizing(ctx context.Context, dp DataProvider, component, file string) (*ComponentSizing, error) {
	if dp == nil {
		dp = defaultEmbeddedProvider
	}
	readCtx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()
	data, err := dp.ReadFile(readCtx, file)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			fmt.Sprintf("failed to read sizing file for component %q from %q", component, file))
	}
	return parseComponentSizing(data, file)
}

func parseComponentSizing(data []byte, file string) (*ComponentSizing, error) {
	var s ComponentSizing
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse sizing file %q", file), err)
	}
	switch err := dec.Decode(new(ComponentSizing)); {
	case stderrors.Is(err, io.EOF):
	case err == nil:
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s contains more than one YAML document; a %s file holds exactly one", file, ComponentSizingKind))
	default:
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse %s past its first document", file), err)
	}
	if err := s.validate(file); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *ComponentSizing) validate(file string) error {
	if s.Kind != ComponentSizingKind {
		return sizingErr(file, "has kind %q, expected %q", s.Kind, ComponentSizingKind)
	}
	if s.APIVersion != header.GroupVersionV1Beta1 {
		return sizingErr(file, "has apiVersion %q, expected %q", s.APIVersion, header.GroupVersionV1Beta1)
	}
	if s.Basis != SizingBasisGPUNodes {
		return sizingErr(file, "has basis %q, expected %q", s.Basis, SizingBasisGPUNodes)
	}
	if len(s.Resources) == 0 {
		return sizingErr(file, "declares no resources")
	}
	for i := range s.Resources {
		r := &s.Resources[i]
		if !isWellFormedValuePath(r.Path) {
			return sizingErr(file, "resource %d path %q is not a well-formed dotted path", i, r.Path)
		}
		for _, prev := range s.Resources[:i] {
			if PathsIntersect(prev.Path, r.Path) {
				return sizingErr(file, "resource paths %q and %q overlap", prev.Path, r.Path)
			}
		}
		if err := r.validate(file); err != nil {
			return err
		}
	}
	return nil
}

func (r *SizingResource) validate(file string) error {
	if len(r.Tiers) == 0 {
		return sizingErr(file, "resource %q declares no tiers", r.Path)
	}
	for i := range r.Tiers {
		t := &r.Tiers[i]
		where := fmt.Sprintf("resource %q tier maxNodes %d", r.Path, t.MaxNodes)
		if t.MaxNodes <= 0 {
			return sizingErr(file, "%s: maxNodes must be positive", where)
		}
		if i > 0 && t.MaxNodes <= r.Tiers[i-1].MaxNodes {
			return sizingErr(file, "%s: maxNodes must ascend strictly", where)
		}
		if err := t.validate(file, where); err != nil {
			return err
		}
	}
	// A tier may only lower what the tier above it sets.
	above := map[string]resource.Quantity{}
	for i := len(r.Tiers) - 1; i >= 0; i-- {
		t := &r.Tiers[i]
		for field, q := range t.quantities() {
			if prev, ok := above[field]; ok && q.Cmp(prev) > 0 {
				return sizingErr(file, "resource %q tier maxNodes %d: %s memory %s is above the next higher tier's %s",
					r.Path, t.MaxNodes, field, q.String(), prev.String())
			}
			above[field] = q
		}
	}
	return nil
}

func (t *SizingTier) validate(file, where string) error {
	for _, f := range []struct {
		name string
		q    *SizingQuantities
	}{{SizingFieldRequests, t.Requests}, {SizingFieldLimits, t.Limits}} {
		if f.q == nil {
			continue
		}
		if _, err := resource.ParseQuantity(f.q.Memory); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s %s: %s memory %q is not a quantity", file, where, f.name, f.q.Memory), err)
		}
	}
	qs := t.quantities()
	req, hasReq := qs[SizingFieldRequests]
	lim, hasLim := qs[SizingFieldLimits]
	if hasReq && hasLim && lim.Cmp(req) < 0 {
		return sizingErr(file, "%s: limits memory %s is below requests memory %s", where, lim.String(), req.String())
	}

	named := map[string]bool{}
	for j := range t.Measurements {
		m := &t.Measurements[j]
		if err := m.validate(file, fmt.Sprintf("%s measurement %d", where, j)); err != nil {
			return err
		}
		for _, s := range m.Sets {
			if _, ok := qs[s]; !ok {
				return sizingErr(file, "%s measurement %d: sets %q, which the tier does not set", where, j, s)
			}
			named[s] = true
		}
		// A limit covers clusters only up to the total nodes of its readings.
		if m.Kind == SizingKindWindowed && m.Nodes <= 0 && slices.Contains(m.Sets, SizingFieldLimits) {
			return sizingErr(file, "%s measurement %d: a windowed measurement that sets limits needs nodes", where, j)
		}
		if hasLim && m.TruncatedAt != "" {
			if killed := resource.MustParse(m.TruncatedAt); lim.Cmp(killed) <= 0 {
				return sizingErr(file, "%s: limits memory %s is not above measurement %d's truncatedAt %s",
					where, lim.String(), j, killed.String())
			}
		}
	}
	for field := range qs {
		if !named[field] {
			return sizingErr(file, "%s: sets %s with no measurement naming it in sets", where, field)
		}
	}
	return nil
}

func (m *SizingMeasurement) validate(file, where string) error {
	for _, s := range m.Sets {
		if s != SizingFieldRequests && s != SizingFieldLimits {
			return sizingErr(file, "%s: sets entry %q is not %q or %q", where, s, SizingFieldRequests, SizingFieldLimits)
		}
	}
	switch m.Kind {
	case SizingKindWindowed:
		if m.Window == "" {
			return sizingErr(file, "%s: a windowed measurement needs window", where)
		}
	case SizingKindInstantaneous:
	default:
		return sizingErr(file, "%s: kind %q is not %q or %q", where, m.Kind, SizingKindWindowed, SizingKindInstantaneous)
	}
	if m.GPUNodes <= 0 {
		return sizingErr(file, "%s: gpuNodes must be positive", where)
	}
	if m.Max == "" {
		return sizingErr(file, "%s: max is required", where)
	}
	for _, f := range [][2]string{{"p90", m.P90}, {"max", m.Max}, {"truncatedAt", m.TruncatedAt}} {
		if f[1] == "" {
			continue
		}
		if _, err := resource.ParseQuantity(f[1]); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s %s: %s %q is not a quantity", file, where, f[0], f[1]), err)
		}
	}
	if _, err := time.Parse(sizingDateLayout, m.Date); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s %s: date %q is not YYYY-MM-DD", file, where, m.Date), err)
	}
	return nil
}

// quantities returns the parsed memory quantities the tier sets, keyed by
// field name. Unparseable values are left out; validate reports them first.
func (t *SizingTier) quantities() map[string]resource.Quantity {
	out := make(map[string]resource.Quantity, 2)
	if t.Requests != nil {
		if q, err := resource.ParseQuantity(t.Requests.Memory); err == nil {
			out[SizingFieldRequests] = q
		}
	}
	if t.Limits != nil {
		if q, err := resource.ParseQuantity(t.Limits.Memory); err == nil {
			out[SizingFieldLimits] = q
		}
	}
	return out
}

func sizingErr(file, format string, args ...any) error {
	return errors.New(errors.ErrCodeInvalidRequest, file+" "+fmt.Sprintf(format, args...))
}
