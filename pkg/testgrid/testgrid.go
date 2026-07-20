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

package testgrid

import (
	"bytes"
	_ "embed"
	stderrors "errors"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/corroborate"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Origin is the AICR evidence dashboard's public origin. It is the single
// pinned host every consumer builds links against and the link-check bot pins
// its resolution to; a coordinate path is appended to it, never interpolated
// from recipe/overlay content, so the target can never be steered off-origin.
const Origin = "https://validation.aicr.run"

// DataURL is the published boot payload the dashboard renderer fetches on load
// (index.json under the site's data/ directory). It is the same-origin data
// source the warning-only link-check bot reads to learn which coordinates the
// live dashboard actually serves — the hash fragment in a deep-link is client
// side only, so a GET on the fragment URL cannot reveal coordinate presence.
const DataURL = Origin + "/data/index.json"

// linkFragmentPrefix is the hash-route prefix of the hash-routed static SPA:
// a deep-link is Origin + "/#/" + Coordinate.Path().
const linkFragmentPrefix = "/#/"

// LinkFor returns the dashboard deep-link for a coordinate: the pinned Origin
// plus the hash route for the coordinate's canonical path. Construction is
// pure and offline — the same coordinate always yields the same byte-stable
// link — so the recipe-health generator stays hermetic.
func LinkFor(co recipe.Coordinate) string {
	return Origin + linkFragmentPrefix + co.Path()
}

// presenceManifest is the committed dashboard presence list, embedded so the
// recipe-health generator reads it offline (never over the network).
//
//go:embed presence.yaml
var presenceManifest []byte

// presenceDoc is the on-disk shape of presence.yaml.
type presenceDoc struct {
	Coordinates []string `yaml:"coordinates"`
}

// Presence is the committed set of coordinate paths that currently carry a
// dashboard presence. It answers Has(coordinate) for the generator and exposes
// its Paths for the link-check bot.
type Presence struct {
	paths map[string]struct{}
}

// LoadPresence parses the embedded presence manifest. It fails closed on a
// malformed manifest or a malformed coordinate path rather than silently
// treating a broken file as "nothing is present", which would drop every
// Evidence link without warning. The decoder is strict (KnownFields) and an
// absent or empty `coordinates` list is rejected, so a typo such as
// `coordinate:` cannot load an empty presence set and make the whole matrix
// fall back to `pending`.
func LoadPresence() (*Presence, error) {
	return parsePresence(presenceManifest)
}

// parsePresence decodes and validates a presence manifest. It is separated from
// LoadPresence so the malformed-input regressions can drive it with bytes the
// embedded (always-valid) manifest cannot exercise.
func parsePresence(manifest []byte) (*Presence, error) {
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	dec.KnownFields(true)
	var doc presenceDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "parse testgrid presence manifest", err)
	}
	// The manifest is a single document. A trailing `---` with extra (possibly
	// malformed) content would otherwise be silently ignored after the first
	// doc decodes, so require the stream to end here.
	if err := dec.Decode(new(presenceDoc)); !stderrors.Is(err, io.EOF) {
		return nil, errors.New(errors.ErrCodeInternal,
			"testgrid presence manifest must contain exactly one YAML document")
	}
	if len(doc.Coordinates) == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			"testgrid presence manifest has no coordinates (missing or empty \"coordinates\" list)")
	}

	paths := make(map[string]struct{}, len(doc.Coordinates))
	for _, p := range doc.Coordinates {
		if err := validatePath(p); err != nil {
			return nil, err
		}
		if _, dup := paths[p]; dup {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"testgrid presence manifest has duplicate coordinate "+quote(p))
		}
		paths[p] = struct{}{}
	}
	return &Presence{paths: paths}, nil
}

// validatePath rejects a manifest entry that is not a well-formed
// "<group>/<dashboard>/<tab>" coordinate path — three non-empty segments and
// no leading, trailing, or interior empty segment. A malformed entry would let
// a typo silently never match any recipe.
func validatePath(p string) error {
	segments := strings.Split(p, "/")
	if len(segments) != 3 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"testgrid presence path "+quote(p)+" must have exactly three segments (<group>/<dashboard>/<tab>)")
	}
	for _, s := range segments {
		if s == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				"testgrid presence path "+quote(p)+" has an empty segment")
		}
	}
	return nil
}

func quote(s string) string { return "\"" + s + "\"" }

// Has reports whether the coordinate has a committed dashboard presence.
func (p *Presence) Has(co recipe.Coordinate) bool {
	_, ok := p.paths[co.Path()]
	return ok
}

// Paths returns the committed coordinate paths, sorted, for deterministic
// iteration by the link-check bot.
func (p *Presence) Paths() []string {
	out := make([]string, 0, len(p.paths))
	for path := range p.paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// LivePaths extracts the set of coordinate paths present in a parsed dashboard
// index. It reuses pkg/recipe.CoordinateFor as the single source of truth for
// how a coordinate path is spelled, so the live and committed sides can never
// drift on path formatting. A tab whose coordinate does not resolve to a
// concrete path (an "any"/empty required dimension the dashboard should never
// emit) is skipped rather than mis-placed.
func LivePaths(idx *corroborate.Index) map[string]struct{} {
	present := make(map[string]struct{})
	if idx == nil {
		return present
	}
	for _, group := range idx.Groups {
		for _, dashboard := range group.Dashboards {
			for _, tab := range dashboard.Tabs {
				co, err := recipe.CoordinateFor(criteriaFromCoord(tab.Coord))
				if err != nil {
					continue
				}
				present[co.Path()] = struct{}{}
			}
		}
	}
	return present
}

// criteriaFromCoord rebuilds resolved Criteria from a tab's coord map so
// CoordinateFor can re-derive the canonical path. The axis keys come from
// corroborate's exported constants (the same ones its generator writes into
// each Tab.Coord), so a future axis rename fails to compile here instead of
// silently yielding an empty coordinate.
func criteriaFromCoord(coord map[string]string) *recipe.Criteria {
	return &recipe.Criteria{
		Service:     recipe.CriteriaServiceType(coord[corroborate.AxisService]),
		Accelerator: recipe.CriteriaAcceleratorType(coord[corroborate.AxisAccelerator]),
		OS:          recipe.CriteriaOSType(coord[corroborate.AxisOS]),
		Intent:      recipe.CriteriaIntentType(coord[corroborate.AxisIntent]),
		Platform:    recipe.CriteriaPlatformType(coord[corroborate.AxisPlatform]),
	}
}
