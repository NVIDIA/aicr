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

// Package sizing loads and validates a component's node-count memory tiers
// (ADR-030), the file a registry entry's sizing.file names.
package sizing

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

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/header"
	"aicr.run/pkg/recipe"
)

// Kind is the kind of a sizing file.
const Kind = "ComponentSizing"

// BasisGPUNodes keys tiers on the GPU node count, the meaning of --nodes.
const BasisGPUNodes = "gpuNodes"

// Sizing field names, as tiers set them and measurements name them in sets.
const (
	Requests = "requests"
	Limits   = "limits"
)

// Measurement kinds. An instantaneous reading is a single sample, so it
// understates the peak a windowed one would have seen.
const (
	Windowed      = "windowed"
	Instantaneous = "instantaneous"
)

const dateLayout = "2006-01-02"

// File is a component's sizing file. It lowers memory resources by
// node-count tier; values above the highest tier, or with no count, are the
// values files' own, so the file holds only lower tiers.
type File struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Basis      string     `yaml:"basis"`
	Resources  []Resource `yaml:"resources"`
}

// Resource is the resources block at Path, written as --set writes it.
// Its Tiers ascend by MaxNodes.
type Resource struct {
	Path  string `yaml:"path"`
	Tiers []Tier `yaml:"tiers"`
}

// Tier covers counts above the next lower tier's MaxNodes, up to its
// own. A tier with neither Requests nor Limits marks a bound only.
type Tier struct {
	MaxNodes     int           `yaml:"maxNodes"`
	Requests     *Memory       `yaml:"requests,omitempty"`
	Limits       *Memory       `yaml:"limits,omitempty"`
	Measurements []Measurement `yaml:"measurements,omitempty"`
}

// Memory holds a Kubernetes memory quantity; only memory is sized.
type Memory struct {
	Memory string `yaml:"memory"`
}

// Measurement is one reading behind a tier. Sets is empty for a reading
// that only constrained the result. TruncatedAt is the limit in force while
// the container was being OOM-killed, which caps what Max could show. Nodes
// is required on a windowed reading that sets a limit. Version, optional, is
// the component's chart version on the measured cluster. Ref, optional,
// links the raw data.
type Measurement struct {
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
func (r *Resource) TierFor(n int) (*Tier, bool) {
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

// Load reads and validates the sizing file at path for component. A nil dp
// reads the embedded catalog, as recipe.GetComponentRegistryFor does. It fails
// closed: a file that cannot be read or breaks a rule is an error, never
// treated as absent.
func Load(ctx context.Context, dp recipe.DataProvider, component, path string) (*File, error) {
	if dp == nil {
		dp = recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	}
	readCtx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()
	data, err := dp.ReadFile(readCtx, path)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			fmt.Sprintf("failed to read sizing file for component %q from %q", component, path))
	}
	return parse(data, path)
}

func parse(data []byte, path string) (*File, error) {
	var s File
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse sizing file %q", path), err)
	}
	switch err := dec.Decode(new(File)); {
	case stderrors.Is(err, io.EOF):
	case err == nil:
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s contains more than one YAML document; a %s file holds exactly one", path, Kind))
	default:
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse %s past its first document", path), err)
	}
	if err := s.validate(path); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *File) validate(path string) error {
	if s.Kind != Kind {
		return fileErr(path, "has kind %q, expected %q", s.Kind, Kind)
	}
	if s.APIVersion != header.GroupVersionV1Beta1 {
		return fileErr(path, "has apiVersion %q, expected %q", s.APIVersion, header.GroupVersionV1Beta1)
	}
	if s.Basis != BasisGPUNodes {
		return fileErr(path, "has basis %q, expected %q", s.Basis, BasisGPUNodes)
	}
	if len(s.Resources) == 0 {
		return fileErr(path, "declares no resources")
	}
	for i := range s.Resources {
		r := &s.Resources[i]
		if !recipe.IsWellFormedValuePath(r.Path) {
			return fileErr(path, "resource %d path %q is not a well-formed dotted path", i, r.Path)
		}
		for _, prev := range s.Resources[:i] {
			if recipe.PathsIntersect(prev.Path, r.Path) {
				return fileErr(path, "resource paths %q and %q overlap", prev.Path, r.Path)
			}
		}
		if err := r.validate(path); err != nil {
			return err
		}
	}
	return nil
}

func (r *Resource) validate(path string) error {
	if len(r.Tiers) == 0 {
		return fileErr(path, "resource %q declares no tiers", r.Path)
	}
	for i := range r.Tiers {
		t := &r.Tiers[i]
		where := fmt.Sprintf("resource %q tier maxNodes %d", r.Path, t.MaxNodes)
		if t.MaxNodes <= 0 {
			return fileErr(path, "%s: maxNodes must be positive", where)
		}
		if i > 0 && t.MaxNodes <= r.Tiers[i-1].MaxNodes {
			return fileErr(path, "%s: maxNodes must ascend strictly", where)
		}
		if err := t.validate(path, where); err != nil {
			return err
		}
	}
	// A tier may only lower what the tier above it sets.
	above := map[string]resource.Quantity{}
	for i := len(r.Tiers) - 1; i >= 0; i-- {
		t := &r.Tiers[i]
		for field, q := range t.quantities() {
			if prev, ok := above[field]; ok && q.Cmp(prev) > 0 {
				return fileErr(path, "resource %q tier maxNodes %d: %s memory %s is above the next higher tier's %s",
					r.Path, t.MaxNodes, field, q.String(), prev.String())
			}
			above[field] = q
		}
	}
	// A field a tier leaves out runs at the values file's value there, so
	// no tier above it may set less.
	omitted := map[string]int{}
	for i := range r.Tiers {
		t := &r.Tiers[i]
		set := t.quantities()
		for _, field := range []string{Requests, Limits} {
			if _, ok := set[field]; !ok {
				if _, seen := omitted[field]; !seen {
					omitted[field] = t.MaxNodes
				}
				continue
			}
			if below, seen := omitted[field]; seen {
				return fileErr(path, "resource %q tier maxNodes %d: sets %s memory, which tier maxNodes %d below it leaves out",
					r.Path, t.MaxNodes, field, below)
			}
		}
	}
	return nil
}

func (t *Tier) validate(path, where string) error {
	for _, f := range []struct {
		name string
		q    *Memory
	}{{Requests, t.Requests}, {Limits, t.Limits}} {
		if f.q == nil {
			continue
		}
		if _, err := resource.ParseQuantity(f.q.Memory); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s %s: %s memory %q is not a quantity", path, where, f.name, f.q.Memory), err)
		}
	}
	qs := t.quantities()
	req, hasReq := qs[Requests]
	lim, hasLim := qs[Limits]
	if hasReq && hasLim && lim.Cmp(req) < 0 {
		return fileErr(path, "%s: limits memory %s is below requests memory %s", where, lim.String(), req.String())
	}

	named := map[string]bool{}
	for j := range t.Measurements {
		m := &t.Measurements[j]
		if err := m.validate(path, fmt.Sprintf("%s measurement %d", where, j)); err != nil {
			return err
		}
		for _, s := range m.Sets {
			if _, ok := qs[s]; !ok {
				return fileErr(path, "%s measurement %d: sets %q, which the tier does not set", where, j, s)
			}
			named[s] = true
		}
		// A limit covers clusters only up to the total nodes of its readings.
		if m.Kind == Windowed && m.Nodes <= 0 && slices.Contains(m.Sets, Limits) {
			return fileErr(path, "%s measurement %d: a windowed measurement that sets limits needs nodes", where, j)
		}
		if hasLim && m.TruncatedAt != "" {
			if killed := resource.MustParse(m.TruncatedAt); lim.Cmp(killed) <= 0 {
				return fileErr(path, "%s: limits memory %s is not above measurement %d's truncatedAt %s",
					where, lim.String(), j, killed.String())
			}
		}
	}
	for field := range qs {
		if !named[field] {
			return fileErr(path, "%s: sets %s with no measurement naming it in sets", where, field)
		}
	}
	return nil
}

func (m *Measurement) validate(path, where string) error {
	for _, s := range m.Sets {
		if s != Requests && s != Limits {
			return fileErr(path, "%s: sets entry %q is not %q or %q", where, s, Requests, Limits)
		}
	}
	switch m.Kind {
	case Windowed:
		if m.Window == "" {
			return fileErr(path, "%s: a windowed measurement needs window", where)
		}
	case Instantaneous:
	default:
		return fileErr(path, "%s: kind %q is not %q or %q", where, m.Kind, Windowed, Instantaneous)
	}
	if m.GPUNodes <= 0 {
		return fileErr(path, "%s: gpuNodes must be positive", where)
	}
	if m.Max == "" {
		return fileErr(path, "%s: max is required", where)
	}
	for _, f := range [][2]string{{"p90", m.P90}, {"max", m.Max}, {"truncatedAt", m.TruncatedAt}} {
		if f[1] == "" {
			continue
		}
		if _, err := resource.ParseQuantity(f[1]); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s %s: %s %q is not a quantity", path, where, f[0], f[1]), err)
		}
	}
	if _, err := time.Parse(dateLayout, m.Date); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s %s: date %q is not YYYY-MM-DD", path, where, m.Date), err)
	}
	return nil
}

// quantities returns the parsed memory quantities the tier sets, keyed by
// field name. Unparseable values are left out; validate reports them first.
func (t *Tier) quantities() map[string]resource.Quantity {
	out := make(map[string]resource.Quantity, 2)
	if t.Requests != nil {
		if q, err := resource.ParseQuantity(t.Requests.Memory); err == nil {
			out[Requests] = q
		}
	}
	if t.Limits != nil {
		if q, err := resource.ParseQuantity(t.Limits.Memory); err == nil {
			out[Limits] = q
		}
	}
	return out
}

func fileErr(path, format string, args ...any) error {
	return errors.New(errors.ErrCodeInvalidRequest, path+" "+fmt.Sprintf(format, args...))
}
