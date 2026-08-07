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

package measurement

import (
	stderrors "errors"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestValidatePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		// wantErr empty means the path must be accepted.
		wantErr aicrerrors.ErrorCode
		// wantMsg, when set, must appear in the error message.
		wantMsg string
	}{
		// --- accepted ---
		{name: "closed scalar key", path: "K8s.server.version"},
		{name: "closed scalar key on node", path: "K8s.node.kubelet-version"},
		{name: "closed scalar key on aks pools", path: "K8s.aks-gpu-pools.gpu-driver"},
		{name: "closed scalar key on gpu hardware", path: "GPU.hardware.driver-loaded"},
		{name: "open scalar key on os release", path: "OS.release.VERSION_ID"},
		{name: "open scalar key with dots", path: "OS.sysctl./proc/sys/kernel/osrelease"},
		{name: "open subtype and key on systemd", path: "SystemD.containerd.service.ActiveState"},
		{name: "open key on node labels", path: "NodeTopology.label.nvidia.com/gpu.present"},
		{name: "closed scalar key on topology summary", path: "NodeTopology.summary.node-count"},
		{name: "virtual node-set path", path: PathGPUNodesLabel},
		{name: "item key via index selector", path: "NetworkTopology.pfs[0].rail"},
		{name: "item context key via index selector", path: "NetworkTopology.pfs[0].pciAddress"},
		{name: "item key via predicate selector", path: "NetworkTopology.pfs[rail=3].pciAddress"},
		{name: "scalar and item spaces coexist", path: "K8s.slinky-slurm.detected"},
		{name: "item key on subtype with both spaces", path: "K8s.slinky-slurm[id=controller/ns/n].cluster-name"},

		// --- rejected: the four addressing-form classes a flat key set misses ---
		{
			name:    "selector omitted on items-only subtype",
			path:    "NetworkTopology.pfs.rail",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "requires an item selector",
		},
		{
			name:    "selector used on subtype with no items",
			path:    "NetworkTopology.capabilities[0].sriov",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "has no items",
		},
		{
			name:    "typo in predicate key",
			path:    "NetworkTopology.pfs[raill=3].pciAddress",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "unknown item predicate key",
		},
		{
			name:    "scalar key used against an item",
			path:    "K8s.slinky-slurm[id=controller/ns/n].detected",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "unknown item key",
		},

		// --- rejected: subtype Context is emitted but never addressable ---
		{
			name:    "identity subtype context key is not addressable",
			path:    "NetworkTopology.identity.linkType",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "unknown key",
		},

		// --- rejected: plain typos ---
		{
			name:    "typo in closed scalar key",
			path:    "K8s.server.verison",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: `did you mean "version"`,
		},
		{
			name:    "typo in subtype",
			path:    "K8s.serer.version",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: `did you mean "server"`,
		},
		{
			name:    "unknown subtype with no near match",
			path:    "K8s.completely-made-up.version",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "unknown subtype",
		},
		{
			name:    "gpu-nodes subtype other than the virtual path",
			path:    "NodeTopology.gpu-nodes.taint",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "unknown subtype",
		},

		// --- rejected: grammar, propagated from ParsePath unchanged ---
		{
			name:    "unknown measurement type",
			path:    "Deployment.gpu-operator.version",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "invalid measurement type",
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "cannot be empty",
		},
		{
			name:    "missing key",
			path:    "K8s.server",
			wantErr: aicrerrors.ErrCodeInvalidRequest,
			wantMsg: "expected format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidatePath(tt.path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidatePath(%q) = %v, want nil", tt.path, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidatePath(%q) = nil, want %s", tt.path, tt.wantErr)
			}
			if !stderrors.Is(err, aicrerrors.New(tt.wantErr, "")) {
				t.Errorf("ValidatePath(%q) error = %v, want code %s", tt.path, err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("ValidatePath(%q) error = %q, want it to contain %q", tt.path, err.Error(), tt.wantMsg)
			}
		})
	}
}

// TestValidatePath_MissingCatalogEntry pins the fail-closed contract for a
// catalog gap: a measurement Type that exists but has no catalog row is an
// internal defect and must never read as "path accepted".
func TestValidatePath_MissingCatalogEntry(t *testing.T) {
	t.Parallel()

	// Deliberately empty: every known Type is missing.
	err := validatePathIn(map[Type]typeSpec{}, "K8s.server.version")
	if err == nil {
		t.Fatal("validatePathIn with an empty catalog = nil, want an error")
	}
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
		t.Errorf("error = %v, want code %s", err, aicrerrors.ErrCodeInternal)
	}
	if !strings.Contains(err.Error(), "no catalog entry") {
		t.Errorf("error = %q, want it to mention the missing catalog entry", err.Error())
	}
}

// TestCatalogCoversEveryType is the build-time half of the same contract.
func TestCatalogCoversEveryType(t *testing.T) {
	t.Parallel()

	for _, mt := range Types {
		if _, ok := catalog[mt]; !ok {
			t.Errorf("measurement type %q has no catalog entry in pkg/measurement/catalog.go", mt)
		}
	}
}

// TestCatalogSubtypesAreAddressable guards against a catalog row that accepts
// a subtype name but exposes no address space at all, which would reject every
// path through it with a confusing message.
func TestCatalogSubtypesAreAddressable(t *testing.T) {
	t.Parallel()

	for mt, ts := range catalog {
		if ts.openSubtype && !ts.fallback.scalar.addressable() && !ts.fallback.item.addressable() {
			t.Errorf("type %q is openSubtype but its fallback addresses nothing", mt)
		}
		for name, spec := range ts.subtypes {
			if !spec.scalar.addressable() && !spec.item.addressable() {
				t.Errorf("type %q subtype %q addresses nothing", mt, name)
			}
		}
	}
}

func TestSuggest(t *testing.T) {
	t.Parallel()

	candidates := []string{"version", "platform", "goVersion"}

	tests := []struct {
		name       string
		got        string
		candidates []string
		want       string
		wantOK     bool
	}{
		{name: "distance 1", got: "versio", candidates: candidates, want: "version", wantOK: true},
		{name: "distance 2", got: "verison", candidates: candidates, want: "version", wantOK: true},
		{name: "distance 3 is too far", got: "vrsn", candidates: candidates, wantOK: false},
		{name: "exact match still suggests itself", got: "version", candidates: candidates, want: "version", wantOK: true},
		{name: "no candidates", got: "version", candidates: nil, wantOK: false},
		{name: "empty input", got: "", candidates: candidates, wantOK: false},
		{
			name:       "equidistant candidates resolve lexically",
			got:        "aa",
			candidates: []string{"zb", "ab", "bb"},
			want:       "ab",
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := suggest(tt.got, tt.candidates)
			if ok != tt.wantOK {
				t.Fatalf("suggest(%q) ok = %v, want %v (got %q)", tt.got, ok, tt.wantOK, got)
			}
			if ok && got != tt.want {
				t.Errorf("suggest(%q) = %q, want %q", tt.got, got, tt.want)
			}
		})
	}
}

// TestSuggestIsDeterministic pins the sort. Subtype candidates come from a Go
// map, so an unsorted scan emits a different suggestion run to run — and the
// suggestion is part of an error string tests and --format json consumers read.
func TestSuggestIsDeterministic(t *testing.T) {
	t.Parallel()

	const path = "K8s.serer.version"

	first := ValidatePath(path)
	if first == nil {
		t.Fatalf("ValidatePath(%q) = nil, want an error", path)
	}
	for i := range 50 {
		again := ValidatePath(path)
		if again == nil {
			t.Fatalf("ValidatePath(%q) = nil on iteration %d", path, i)
		}
		if again.Error() != first.Error() {
			t.Fatalf("ValidatePath(%q) is nondeterministic:\n first = %q\n iter %d = %q",
				path, first.Error(), i, again.Error())
		}
	}
}

func TestEditDistance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"version", "verison", 2},
		{"version", "versio", 1},
		{"kitten", "sitting", 3},
		// Multi-byte: rune-based, not byte-based.
		{"héllo", "hello", 1},
	}

	for _, tt := range tests {
		t.Run(tt.a+"/"+tt.b, func(t *testing.T) {
			t.Parallel()

			if got := editDistance(tt.a, tt.b); got != tt.want {
				t.Errorf("editDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
