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
	"context"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

// validSizing is a fixture; its values are not real tiers.
const validSizing = `apiVersion: aicr.run/v1beta1
kind: ComponentSizing
basis: gpuNodes
resources:
  - path: operator.resources
    tiers:
      - maxNodes: 100
        requests: {memory: 100Mi}
        limits: {memory: 200Mi}
        measurements:
          - sets: [requests, limits]
            kind: windowed
            window: 24h
            gpuNodes: 90
            nodes: 95
            pods: 1000
            p90: 80Mi
            max: 150Mi
            date: 2026-01-02
            ref: https://example.com/1
          - sets: []
            kind: instantaneous
            gpuNodes: 50
            max: 140Mi
            truncatedAt: 150Mi
            date: 2026-01-03
            ref: https://example.com/2
      - maxNodes: 200
        requests: {memory: 100Mi}
        limits: {memory: 400Mi}
        measurements:
          - sets: [requests, limits]
            kind: windowed
            window: 7d
            gpuNodes: 210
            nodes: 215
            max: 300Mi
            date: 2026-01-04
            ref: https://example.com/3
      - maxNodes: 300
`

func TestParseComponentSizing(t *testing.T) {
	tests := []struct {
		name    string
		old     string
		new     string
		wantErr string
	}{
		{name: "valid"},
		{name: "unknown field", old: "basis: gpuNodes", new: "basis: gpuNodes\nbogus: 1", wantErr: "failed to parse"},
		{name: "second document", old: "kind: ComponentSizing", new: "kind: ComponentSizing\n---\nkind: x", wantErr: "more than one YAML document"},
		{name: "wrong kind", old: "kind: ComponentSizing", new: "kind: ComponentUpgrades", wantErr: "has kind"},
		{name: "wrong apiVersion", old: "aicr.run/v1beta1", new: "aicr.run/v1alpha1", wantErr: "has apiVersion"},
		{name: "wrong basis", old: "basis: gpuNodes", new: "basis: nodes", wantErr: "has basis"},
		{name: "malformed path", old: "path: operator.resources", new: "path: operator..resources", wantErr: "not a well-formed"},
		{name: "non-positive maxNodes", old: "maxNodes: 100", new: "maxNodes: 0", wantErr: "must be positive"},
		{name: "maxNodes not ascending", old: "maxNodes: 300", new: "maxNodes: 200", wantErr: "ascend strictly"},
		{name: "unparseable quantity", old: "requests: {memory: 100Mi}\n        limits: {memory: 200Mi}", new: "requests: {memory: lots}\n        limits: {memory: 200Mi}", wantErr: "not a quantity"},
		{name: "limit below request", old: "limits: {memory: 200Mi}", new: "limits: {memory: 50Mi}", wantErr: "below requests"},
		{name: "tier above the next higher tier", old: "limits: {memory: 200Mi}", new: "limits: {memory: 500Mi}", wantErr: "above the next higher tier"},
		{name: "field set with no measurement", old: "          - sets: [requests, limits]\n            kind: windowed\n            window: 7d", new: "          - sets: [limits]\n            kind: windowed\n            window: 7d", wantErr: "no measurement naming it"},
		{name: "measurement sets an unset field", old: "      - maxNodes: 300\n", new: "      - maxNodes: 300\n        measurements:\n          - sets: [limits]\n            kind: instantaneous\n            gpuNodes: 1\n            max: 1Mi\n            date: 2026-01-01\n            ref: x\n", wantErr: "which the tier does not set"},
		{name: "unknown sets entry", old: "sets: []", new: "sets: [cpu]", wantErr: "sets entry"},
		{name: "unknown kind", old: "kind: instantaneous", new: "kind: sampled", wantErr: "kind \"sampled\""},
		{name: "windowed without window", old: "            window: 7d\n", new: "", wantErr: "needs window"},
		{name: "missing gpuNodes", old: "            gpuNodes: 50\n", new: "", wantErr: "gpuNodes must be positive"},
		{name: "missing max", old: "            max: 300Mi\n", new: "", wantErr: "max is required"},
		{name: "unparseable max", old: "max: 300Mi", new: "max: big", wantErr: "max \"big\""},
		{name: "unparseable truncatedAt", old: "truncatedAt: 150Mi", new: "truncatedAt: x", wantErr: "truncatedAt"},
		{name: "limit not above truncatedAt", old: "truncatedAt: 150Mi", new: "truncatedAt: 200Mi", wantErr: "is not above measurement"},
		{name: "windowed limit reading without nodes", old: "            nodes: 95\n", new: "", wantErr: "needs nodes"},
		{name: "version is optional", old: "            date: 2026-01-02\n", new: "            date: 2026-01-02\n            version: v1.2.3\n"},
		{name: "bad date", old: "date: 2026-01-04", new: "date: Jan 4", wantErr: "YYYY-MM-DD"},
		{name: "ref is optional", old: "            ref: https://example.com/3\n", new: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := validSizing
			if tt.old != "" {
				if !strings.Contains(doc, tt.old) {
					t.Fatalf("fixture does not contain %q", tt.old)
				}
				doc = strings.Replace(doc, tt.old, tt.new, 1)
			}
			s, err := parseComponentSizing([]byte(doc), "sizing.yaml")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(s.Resources) != 1 || len(s.Resources[0].Tiers) != 3 {
					t.Fatalf("unexpected parse: %+v", s)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseComponentSizingRejectsStructure(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name:    "no resources",
			doc:     "apiVersion: aicr.run/v1beta1\nkind: ComponentSizing\nbasis: gpuNodes\nresources: []\n",
			wantErr: "declares no resources",
		},
		{
			name:    "no tiers",
			doc:     "apiVersion: aicr.run/v1beta1\nkind: ComponentSizing\nbasis: gpuNodes\nresources:\n  - path: a.resources\n",
			wantErr: "declares no tiers",
		},
		{
			name: "overlapping paths",
			doc: "apiVersion: aicr.run/v1beta1\nkind: ComponentSizing\nbasis: gpuNodes\nresources:\n" +
				"  - path: a\n    tiers: [{maxNodes: 1}]\n  - path: a.resources\n    tiers: [{maxNodes: 1}]\n",
			wantErr: "overlap",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseComponentSizing([]byte(tt.doc), "sizing.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

func TestSizingResourceTierFor(t *testing.T) {
	s, err := parseComponentSizing([]byte(validSizing), "sizing.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := &s.Resources[0]
	tests := []struct {
		n      int
		wantOK bool
		want   int
	}{
		{-1, false, 0}, {0, false, 0}, {1, true, 100}, {100, true, 100}, {101, true, 200},
		{300, true, 300}, {301, false, 0},
	}
	for _, tt := range tests {
		tier, ok := r.TierFor(tt.n)
		if ok != tt.wantOK {
			t.Fatalf("TierFor(%d) ok = %v, want %v", tt.n, ok, tt.wantOK)
		}
		if ok && tier.MaxNodes != tt.want {
			t.Fatalf("TierFor(%d) = %d, want %d", tt.n, tier.MaxNodes, tt.want)
		}
	}
}

func TestLoadComponentSizing(t *testing.T) {
	dp := providertest.NewMap(map[string][]byte{"components/x/sizing.yaml": []byte(validSizing)})
	s, err := LoadComponentSizing(context.Background(), dp, "x", "components/x/sizing.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Resources[0].Path != "operator.resources" {
		t.Fatalf("unexpected document: %+v", s)
	}
	if _, err := LoadComponentSizing(context.Background(), dp, "x", "components/x/missing.yaml"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// TestRegistrySizingFilesLoad loads every sizing file the embedded registry
// references; the tier values themselves are validated empirically, not here.
func TestRegistrySizingFilesLoad(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range registry.Components {
		if c.Sizing.File == "" {
			continue
		}
		if _, err := LoadComponentSizing(context.Background(), nil, c.Name, c.Sizing.File); err != nil {
			t.Errorf("component %q: %v", c.Name, err)
		}
	}
}

// TestRegistrySizingFilesShareBounds fails when two sizing files declare
// different maxNodes sets, so one count selects the same row in each.
func TestRegistrySizingFilesShareBounds(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var first string
	var want []int
	for _, c := range registry.Components {
		if c.Sizing.File == "" {
			continue
		}
		sz, err := LoadComponentSizing(context.Background(), nil, c.Name, c.Sizing.File)
		if err != nil {
			t.Fatalf("component %q: %v", c.Name, err)
		}
		for _, r := range sz.Resources {
			got := make([]int, 0, len(r.Tiers))
			for _, tier := range r.Tiers {
				got = append(got, tier.MaxNodes)
			}
			if want == nil {
				first, want = c.Name+" "+r.Path, got
				continue
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s %s declares bounds %v, but %s declares %v", c.Name, r.Path, got, first, want)
			}
		}
	}
}

// TestRegistrySizingPathsAreSet fails when a sizing path is set by none of
// its component's values files, so a typo or a key renamed by a chart bump
// cannot turn sizing into a silent no-op.
func TestRegistrySizingPathsAreSet(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	fsys := GetEmbeddedFS()
	for _, c := range registry.Components {
		if c.Sizing.File == "" {
			continue
		}
		sz, err := LoadComponentSizing(context.Background(), nil, c.Name, c.Sizing.File)
		if err != nil {
			t.Fatalf("component %q: %v", c.Name, err)
		}
		files, err := fs.Glob(fsys, path.Join("components", c.Name, "values*.yaml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("component %q: no values files (%v)", c.Name, err)
		}
		for _, r := range sz.Resources {
			found := false
			for _, f := range files {
				data, readErr := fs.ReadFile(fsys, f)
				if readErr != nil {
					t.Fatal(readErr)
				}
				var values map[string]any
				if yamlErr := yaml.Unmarshal(data, &values); yamlErr != nil {
					t.Fatalf("%s: %v", f, yamlErr)
				}
				if hasPath(values, r.Path+".requests.memory") || hasPath(values, r.Path+".limits.memory") {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("component %q: sizing path %q sets no memory in any of %v", c.Name, r.Path, files)
			}
		}
	}
}

func hasPath(m map[string]any, dotted string) bool {
	var cur any = m
	for _, k := range strings.Split(dotted, ".") {
		next, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = next[k]; !ok {
			return false
		}
	}
	return true
}
