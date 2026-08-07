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
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestParsePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		wantType    Type
		wantSubtype string
		wantKey     string
		expectError bool
	}{
		// Valid paths
		{
			name:        "k8s server version",
			path:        "K8s.server.version",
			wantType:    TypeK8s,
			wantSubtype: "server",
			wantKey:     "version",
		},
		{
			name:        "os release id",
			path:        "OS.release.ID",
			wantType:    TypeOS,
			wantSubtype: "release",
			wantKey:     "ID",
		},
		{
			name:        "os release version",
			path:        "OS.release.VERSION_ID",
			wantType:    TypeOS,
			wantSubtype: "release",
			wantKey:     "VERSION_ID",
		},
		{
			name:        "os sysctl kernel osrelease",
			path:        "OS.sysctl./proc/sys/kernel/osrelease",
			wantType:    TypeOS,
			wantSubtype: "sysctl",
			wantKey:     "/proc/sys/kernel/osrelease",
		},
		{
			name:        "gpu info type",
			path:        "GPU.info.type",
			wantType:    TypeGPU,
			wantSubtype: "info",
			wantKey:     "type",
		},
		{
			// Corrected in #1783. This case previously asserted subtype
			// "containerd" with key "service.ActiveState" — a split no
			// producer can satisfy, since pkg/collector/systemd emits
			// Subtype{Name: "containerd.service"} with D-Bus property keys.
			// Every SystemD constraint path therefore resolved to NotFound.
			// See TestParsePath_DottedSubtype and TestPath_Extract_SystemDUnit.
			name:        "systemd containerd service splits at the last dot",
			path:        "SystemD.containerd.service.ActiveState",
			wantType:    TypeSystemD,
			wantSubtype: "containerd.service",
			wantKey:     "ActiveState",
		},

		// Error cases
		{name: "empty path", path: "", expectError: true},
		{name: "single part", path: "K8s", expectError: true},
		{name: "two parts", path: "K8s.server", expectError: true},
		{name: "invalid type", path: "InvalidType.subtype.key", expectError: true},
		// Note: Type matching is case-sensitive
		{name: "lowercase k8s", path: "k8s.server.version", expectError: true},
		{name: "lowercase os", path: "os.release.ID", expectError: true},
		{name: "lowercase gpu", path: "gpu.info.type", expectError: true},
		{name: "lowercase systemd", path: "systemd.containerd.service.ActiveState", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ParsePath(tt.path)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Type != tt.wantType {
				t.Errorf("Type = %v, want %v", result.Type, tt.wantType)
			}
			if result.Subtype != tt.wantSubtype {
				t.Errorf("Subtype = %q, want %q", result.Subtype, tt.wantSubtype)
			}
			if result.Key != tt.wantKey {
				t.Errorf("Key = %q, want %q", result.Key, tt.wantKey)
			}
		})
	}
}

// TestParsePath_DottedSubtype pins the split rule, which differs by Type.
//
// The {Type}.{Subtype}.{Key} grammar is ambiguous when both parts may contain
// dots. SystemD subtypes ARE unit names ("containerd.service") and its keys are
// dot-free D-Bus properties, so it splits at the last dot. Every other Type has
// dot-free subtypes and possibly dotted keys, so it splits at the first — which
// is what makes OS.sysctl./proc/... and NodeTopology.label.<label-key> resolve.
//
// Before this rule, "SystemD.containerd.service.ActiveState" parsed as subtype
// "containerd" with key "service.ActiveState", which no producer emits, so
// every SystemD constraint path silently resolved to NotFound.
func TestParsePath_DottedSubtype(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		wantSubtype string
		wantKey     string
	}{
		{
			name:        "systemd unit splits at the last dot",
			path:        "SystemD.containerd.service.ActiveState",
			wantSubtype: "containerd.service",
			wantKey:     "ActiveState",
		},
		{
			name:        "systemd talos unit splits at the last dot",
			path:        "SystemD.kubelet.service.SubState",
			wantSubtype: "kubelet.service",
			wantKey:     "SubState",
		},
		{
			name:        "dotted key on a dot-free subtype still splits at the first dot",
			path:        "OS.sysctl./proc/sys/kernel/osrelease",
			wantSubtype: "sysctl",
			wantKey:     "/proc/sys/kernel/osrelease",
		},
		{
			name:        "dotted label key still splits at the first dot",
			path:        "NodeTopology.label.nvidia.com/gpu.present",
			wantSubtype: "label",
			wantKey:     "nvidia.com/gpu.present",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParsePath(tt.path)
			if err != nil {
				t.Fatalf("ParsePath(%q) error = %v", tt.path, err)
			}
			if got.Subtype != tt.wantSubtype {
				t.Errorf("Subtype = %q, want %q", got.Subtype, tt.wantSubtype)
			}
			if got.Key != tt.wantKey {
				t.Errorf("Key = %q, want %q", got.Key, tt.wantKey)
			}
			if round := got.String(); round != tt.path {
				t.Errorf("String() = %q, want the original %q", round, tt.path)
			}
		})
	}
}

// systemDCollectorMeasurements mirrors what pkg/collector/systemd and
// pkg/collector/talos/service emit: the subtype IS the unit name and the keys
// are dot-free D-Bus properties.
func systemDCollectorMeasurements() []*Measurement {
	return []*Measurement{
		{
			Type: TypeSystemD,
			Subtypes: []Subtype{
				{
					Name: "containerd.service",
					Data: map[string]Reading{
						"ActiveState": Str("active"),
						"SubState":    Str("running"),
					},
				},
				{
					Name: "kubelet.service",
					Data: map[string]Reading{"ActiveState": Str("inactive")},
				},
			},
		},
	}
}

// TestSystemDPathParseValidateExtract is the regression test for the defect
// this catalog exists to eliminate, exercised as one chain rather than three
// independent assertions.
//
// The original bug was not that any single stage was broken — each looked
// self-consistent. It was that the stages DISAGREED: the parser produced
// subtype "containerd", the catalog accepted it (SystemD is open-subtype, so
// it accepts any name), and extraction then found nothing because the producer
// emits "containerd.service". The path sailed through load validation and
// degraded to NotFound at evaluation, which the resolver reads as "reading
// absent from this snapshot" and silently excludes the overlay — exactly the
// false-PASS #1783 removes.
//
// Asserting parse, validate, and extract separately cannot catch that class of
// bug, because each stage passes its own test while contradicting the next.
// This chains them on one input.
func TestSystemDPathParseValidateExtract(t *testing.T) {
	t.Parallel()

	ms := systemDCollectorMeasurements()

	tests := []struct {
		name        string
		path        string
		wantSubtype string
		wantKey     string
		want        string
	}{
		{
			name:        "containerd active state",
			path:        "SystemD.containerd.service.ActiveState",
			wantSubtype: "containerd.service",
			wantKey:     "ActiveState",
			want:        "active",
		},
		{
			name:        "containerd sub state",
			path:        "SystemD.containerd.service.SubState",
			wantSubtype: "containerd.service",
			wantKey:     "SubState",
			want:        "running",
		},
		{
			name:        "talos kubelet unit",
			path:        "SystemD.kubelet.service.ActiveState",
			wantSubtype: "kubelet.service",
			wantKey:     "ActiveState",
			want:        "inactive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// 1. Parse: the split must name the unit the producer emits.
			p, err := ParsePath(tt.path)
			if err != nil {
				t.Fatalf("ParsePath(%q) error = %v", tt.path, err)
			}
			if p.Subtype != tt.wantSubtype {
				t.Errorf("Subtype = %q, want %q", p.Subtype, tt.wantSubtype)
			}
			if p.Key != tt.wantKey {
				t.Errorf("Key = %q, want %q", p.Key, tt.wantKey)
			}

			// 2. Validate: the catalog must accept what the parser produced.
			if validateErr := ValidatePath(tt.path); validateErr != nil {
				t.Fatalf("ValidatePath(%q) = %v, want nil", tt.path, validateErr)
			}

			// 3. Extract: and it must actually resolve. A path that validates
			// but cannot extract is the silent-exclusion bug.
			got, err := p.Extract(ms)
			if err != nil {
				t.Fatalf("Extract(%q) error = %v — validates but does not resolve", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("Extract(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// TestSystemDUnitNameTypoIsNotCaught documents the residual limit, so the
// catalog is not mistaken for stronger than it is.
//
// SystemD subtype names are unit names supplied by collector configuration,
// not a fixed vocabulary, so the catalog declares the space open and CANNOT
// reject a mistyped unit. Such a path still validates and still degrades to
// NotFound at evaluation. This is the same residual as any other open key
// space (a mistyped /etc/os-release field, a mistyped node label), and it is
// the documented cost of accepting producer-defined names.
//
// What the fix does close is the systematic case: correctly spelled paths no
// longer fail.
func TestSystemDUnitNameTypoIsNotCaught(t *testing.T) {
	t.Parallel()

	const typo = "SystemD.contaierd.service.ActiveState"

	if err := ValidatePath(typo); err != nil {
		t.Fatalf("ValidatePath(%q) = %v; the open SystemD subtype space accepts any unit name", typo, err)
	}

	p, err := ParsePath(typo)
	if err != nil {
		t.Fatalf("ParsePath(%q) error = %v", typo, err)
	}
	// Assert the code, not just non-nil: NotFound is what the resolver reads
	// as "reading absent from this snapshot", and it is the code that makes
	// this residual a graceful exclusion rather than a hard failure.
	_, err = p.Extract(systemDCollectorMeasurements())
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeNotFound, "")) {
		t.Fatalf("Extract() error = %v, want code %s for a mistyped unit name",
			err, aicrerrors.ErrCodeNotFound)
	}
}

func TestPath_ExtractValue(t *testing.T) {
	t.Parallel()

	// Create a test snapshot with sample measurements
	snapshot := []*Measurement{
		{
			Type: TypeK8s,
			Subtypes: []Subtype{
				{
					Name: "server",
					Data: map[string]Reading{
						"version": Str("v1.33.5-eks-3025e55"),
					},
				},
				{
					Name: "images",
					Data: map[string]Reading{
						"count": Str("42"),
					},
				},
			},
		},
		{
			Type: TypeOS,
			Subtypes: []Subtype{
				{
					Name: "release",
					Data: map[string]Reading{
						"ID":         Str("ubuntu"),
						"VERSION_ID": Str("24.04"),
					},
				},
				{
					Name: "sysctl",
					Data: map[string]Reading{
						"/proc/sys/kernel/osrelease": Str("6.8.0-1028-aws"),
					},
				},
			},
		},
		{
			Type: TypeGPU,
			Subtypes: []Subtype{
				{
					Name: "info",
					Data: map[string]Reading{
						"type":   Str("H100"),
						"driver": Str("550.107.02"),
					},
				},
			},
		},
	}

	tests := []struct {
		name string
		path Path
		want string
		// wantCode is the expected structured code when extraction fails;
		// empty means it must succeed. See TestPath_ExtractValue_ItemSelector
		// for why the code and not merely non-nil is asserted.
		wantCode aicrerrors.ErrorCode
	}{
		// Valid extractions
		{
			name: "k8s server version",
			path: Path{
				Type:    TypeK8s,
				Subtype: "server",
				Key:     "version",
			},
			want: "v1.33.5-eks-3025e55",
		},
		{
			name: "os release id",
			path: Path{
				Type:    TypeOS,
				Subtype: "release",
				Key:     "ID",
			},
			want: "ubuntu",
		},
		{
			name: "os release version",
			path: Path{
				Type:    TypeOS,
				Subtype: "release",
				Key:     "VERSION_ID",
			},
			want: "24.04",
		},
		{
			name: "kernel version",
			path: Path{
				Type:    TypeOS,
				Subtype: "sysctl",
				Key:     "/proc/sys/kernel/osrelease",
			},
			want: "6.8.0-1028-aws",
		},
		{
			name: "gpu type",
			path: Path{
				Type:    TypeGPU,
				Subtype: "info",
				Key:     "type",
			},
			want: "H100",
		},

		// Error cases - not found
		{
			name: "measurement type not found",
			path: Path{
				Type:    TypeSystemD,
				Subtype: "containerd.service",
				Key:     "ActiveState",
			},
			wantCode: aicrerrors.ErrCodeNotFound,
		},
		{
			name: "subtype not found",
			path: Path{
				Type:    TypeK8s,
				Subtype: "nonexistent",
				Key:     "version",
			},
			wantCode: aicrerrors.ErrCodeNotFound,
		},
		{
			name: "key not found",
			path: Path{
				Type:    TypeK8s,
				Subtype: "server",
				Key:     "nonexistent",
			},
			wantCode: aicrerrors.ErrCodeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := tt.path.Extract(snapshot)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("expected %s, got nil; result=%q", tt.wantCode, result)
				}
				if !stderrors.Is(err, aicrerrors.New(tt.wantCode, "")) {
					t.Errorf("error = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.want {
				t.Errorf("Extract() = %q, want %q", result, tt.want)
			}
		})
	}
}

func TestParsePath_ItemSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		wantType     Type
		wantSubtype  string
		wantKey      string
		wantSelector string // empty means no selector expected
		wantIndex    *int   // non-nil when index selector expected
		wantPredKey  string // non-empty when predicate expected
		wantPredVal  string
		expectError  bool
	}{
		{
			// Exercises the empty-wantSelector contract: a selector-free path
			// must parse with no selector at all, not merely a different one.
			name:        "no selector",
			path:        "NetworkTopology.capabilities.sriov",
			wantType:    TypeNetworkTopology,
			wantSubtype: "capabilities",
			wantKey:     "sriov",
		},
		{
			name:         "index selector",
			path:         "NetworkTopology.pfs[0].rail",
			wantType:     TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "rail",
			wantSelector: "0",
			wantIndex:    intPtr(0),
		},
		{
			name:         "index selector larger",
			path:         "NetworkTopology.pfs[7].pciAddress",
			wantType:     TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "pciAddress",
			wantSelector: "7",
			wantIndex:    intPtr(7),
		},
		{
			name:         "predicate selector numeric value",
			path:         "NetworkTopology.pfs[rail=3].pciAddress",
			wantType:     TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "pciAddress",
			wantSelector: "rail=3",
			wantPredKey:  "rail",
			wantPredVal:  "3",
		},
		{
			name:         "predicate selector string value",
			path:         "NetworkTopology.pfs[traffic=east-west].rdmaDevice",
			wantType:     TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "rdmaDevice",
			wantSelector: "traffic=east-west",
			wantPredKey:  "traffic",
			wantPredVal:  "east-west",
		},
		{
			name:         "selector with dotted key after",
			path:         "NetworkTopology.pfs[0]./some/dotted/key",
			wantType:     TypeNetworkTopology,
			wantSubtype:  "pfs",
			wantKey:      "/some/dotted/key",
			wantSelector: "0",
			wantIndex:    intPtr(0),
		},

		// Error cases
		{name: "unclosed bracket", path: "NetworkTopology.pfs[0.rail", expectError: true},
		{name: "missing dot after selector", path: "NetworkTopology.pfs[0]rail", expectError: true},
		{name: "missing key after selector", path: "NetworkTopology.pfs[0]", expectError: true},
		{name: "missing key after selector with dot", path: "NetworkTopology.pfs[0].", expectError: true},
		{name: "empty selector", path: "NetworkTopology.pfs[].rail", expectError: true},
		{name: "empty subtype before bracket", path: "NetworkTopology.[0].rail", expectError: true},
		{name: "negative index", path: "NetworkTopology.pfs[-1].rail", expectError: true},
		{name: "non-integer non-predicate", path: "NetworkTopology.pfs[notnumber].rail", expectError: true},
		{name: "predicate empty key", path: "NetworkTopology.pfs[=3].rail", expectError: true},
		{name: "predicate empty value", path: "NetworkTopology.pfs[rail=].pciAddress", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ParsePath(tt.path)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil; result=%+v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Type != tt.wantType {
				t.Errorf("Type = %v, want %v", result.Type, tt.wantType)
			}
			if result.Subtype != tt.wantSubtype {
				t.Errorf("Subtype = %q, want %q", result.Subtype, tt.wantSubtype)
			}
			if result.Key != tt.wantKey {
				t.Errorf("Key = %q, want %q", result.Key, tt.wantKey)
			}
			// Honor the table's documented contract: an empty wantSelector
			// means the path carries no selector. Unconditionally requiring
			// one made that case unexpressible, so a selector-free row would
			// have failed rather than asserted anything.
			if tt.wantSelector == "" {
				if result.selector != nil {
					t.Errorf("Selector = %+v, want none", result.selector)
				}
				return
			}
			if result.selector == nil {
				t.Fatalf("Selector is nil; want %q", tt.wantSelector)
			}
			if result.selector.Raw != tt.wantSelector {
				t.Errorf("Selector.Raw = %q, want %q", result.selector.Raw, tt.wantSelector)
			}
			if tt.wantIndex != nil {
				if result.selector.Index == nil {
					t.Errorf("Selector.Index = nil, want %d", *tt.wantIndex)
				} else if *result.selector.Index != *tt.wantIndex {
					t.Errorf("Selector.Index = %d, want %d", *result.selector.Index, *tt.wantIndex)
				}
				if result.selector.Predicate != nil {
					t.Errorf("Selector.Predicate = %+v, want nil", result.selector.Predicate)
				}
			}
			if tt.wantPredKey != "" {
				if result.selector.Predicate == nil {
					t.Errorf("Selector.Predicate = nil, want key=%s value=%s", tt.wantPredKey, tt.wantPredVal)
				} else {
					if result.selector.Predicate.Key != tt.wantPredKey {
						t.Errorf("Selector.Predicate.Key = %q, want %q", result.selector.Predicate.Key, tt.wantPredKey)
					}
					if result.selector.Predicate.Value != tt.wantPredVal {
						t.Errorf("Selector.Predicate.Value = %q, want %q", result.selector.Predicate.Value, tt.wantPredVal)
					}
				}
				if result.selector.Index != nil {
					t.Errorf("Selector.Index = %d, want nil", *result.selector.Index)
				}
			}
		})
	}
}

func intPtr(i int) *int { return &i }

func TestPath_ExtractValue_ItemSelector(t *testing.T) {
	t.Parallel()

	snap := []*Measurement{
		{
			Type: TypeNetworkTopology,
			Subtypes: []Subtype{
				{
					Name: "pfs",
					Items: []ItemEntry{
						{
							Context: map[string]string{
								"pciAddress": "0000:03:00.0",
								"rdmaDevice": "mlx5_0",
							},
							Data: map[string]Reading{
								"rail":    Int(0),
								"traffic": Str("east-west"),
							},
						},
						{
							Context: map[string]string{
								"pciAddress": "0000:03:00.1",
								"rdmaDevice": "mlx5_1",
							},
							Data: map[string]Reading{
								"rail":    Int(1),
								"traffic": Str("east-west"),
							},
						},
						{
							Context: map[string]string{
								"pciAddress": "0000:03:00.2",
								"rdmaDevice": "mlx5_2",
							},
							Data: map[string]Reading{
								"rail":    Int(2),
								"traffic": Str("east-west"),
							},
						},
					},
				},
				{
					Name: "capabilities",
					Data: map[string]Reading{
						"sriov": Bool(true),
					},
				},
			},
		},
	}

	mustParse := func(t *testing.T, path string) *Path {
		t.Helper()
		p, err := ParsePath(path)
		if err != nil {
			t.Fatalf("ParsePath(%q) error = %v", path, err)
		}
		return p
	}

	tests := []struct {
		name string
		path string
		want string
		// wantCode is the expected structured code when extraction fails;
		// empty means extraction must succeed. Asserting the code matters
		// because the resolver branches on it: only ErrCodeNotFound is the
		// graceful-exclusion signal, so a regression that swapped NotFound
		// for Conflict (or vice versa) would change resolution behavior while
		// still returning "some error".
		wantCode aicrerrors.ErrorCode
	}{
		// Index selectors
		{name: "index 0 data field", path: "NetworkTopology.pfs[0].rail", want: "0"},
		{name: "index 1 data field", path: "NetworkTopology.pfs[1].rail", want: "1"},
		{name: "index 0 context field", path: "NetworkTopology.pfs[0].pciAddress", want: "0000:03:00.0"},
		{name: "index 2 rdmaDevice", path: "NetworkTopology.pfs[2].rdmaDevice", want: "mlx5_2"},
		// Predicate selectors
		{name: "predicate by data field", path: "NetworkTopology.pfs[rail=1].pciAddress", want: "0000:03:00.1"},
		{name: "predicate by context field", path: "NetworkTopology.pfs[pciAddress=0000:03:00.2].rail", want: "2"},
		// Backward compat: non-item path still works
		{name: "non-item path data", path: "NetworkTopology.capabilities.sriov", want: "true"},

		// Errors
		{name: "index out of bounds", path: "NetworkTopology.pfs[99].rail", wantCode: aicrerrors.ErrCodeNotFound},
		{name: "predicate no match", path: "NetworkTopology.pfs[rail=99].pciAddress", wantCode: aicrerrors.ErrCodeNotFound},
		{name: "key not in item", path: "NetworkTopology.pfs[0].nonexistent", wantCode: aicrerrors.ErrCodeNotFound},
		{name: "selector on subtype with no items", path: "NetworkTopology.capabilities[0].sriov", wantCode: aicrerrors.ErrCodeNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := mustParse(t, tt.path)
			got, err := path.Extract(snap)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("expected %s, got nil; result=%q", tt.wantCode, got)
				}
				if !stderrors.Is(err, aicrerrors.New(tt.wantCode, "")) {
					t.Errorf("error = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Extract() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPath_ExtractValue_SlinkySlurmSummary(t *testing.T) {
	t.Parallel()

	snap := []*Measurement{
		{
			Type: TypeK8s,
			Subtypes: []Subtype{
				{
					Name: "slinky-slurm",
					Data: map[string]Reading{
						"detected":         Bool(true),
						"collection-state": Str("detected"),
					},
					Items: []ItemEntry{
						{
							Context: map[string]string{
								"id":            "nodeset/slurm/workers",
								"kind":          "NodeSet",
								"controller-id": "controller/slurm/cluster",
							},
							Data: map[string]Reading{
								"partition-enabled": Bool(true),
							},
						},
					},
				},
				{
					Name: "mariadb-operator",
					Data: map[string]Reading{
						"collection-state": Str("absent"),
					},
				},
			},
		},
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "detected",
			path: "K8s.slinky-slurm.detected",
			want: "true",
		},
		{
			name: "collection state",
			path: "K8s.slinky-slurm.collection-state",
			want: "detected",
		},
		{
			name: "item data by canonical id",
			path: "K8s.slinky-slurm[id=nodeset/slurm/workers].partition-enabled",
			want: "true",
		},
		{
			name: "item context by kind",
			path: "K8s.slinky-slurm[kind=NodeSet].controller-id",
			want: "controller/slurm/cluster",
		},
		{
			name: "MariaDB collection state",
			path: "K8s.mariadb-operator.collection-state",
			want: "absent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path, err := ParsePath(tt.path)
			if err != nil {
				t.Fatalf("ParsePath(%q) error = %v", tt.path, err)
			}
			got, err := path.Extract(snap)
			if err != nil {
				t.Fatalf("Extract() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Extract() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPath_ExtractValue_PredicateAmbiguous(t *testing.T) {
	t.Parallel()

	snap := []*Measurement{
		{
			Type: TypeNetworkTopology,
			Subtypes: []Subtype{
				{
					Name: "pfs",
					Items: []ItemEntry{
						{Data: map[string]Reading{"traffic": Str("east-west"), "rail": Int(0)}},
						{Data: map[string]Reading{"traffic": Str("east-west"), "rail": Int(1)}},
					},
				},
			},
		},
	}

	path, err := ParsePath("NetworkTopology.pfs[traffic=east-west].rail")
	if err != nil {
		t.Fatalf("ParsePath() error = %v", err)
	}
	_, err = path.Extract(snap)
	if err == nil {
		t.Fatal("expected ambiguous-match error, got nil")
	}
	// Assert the code, not just non-nil: a predicate matching several items is
	// ErrCodeConflict, and it must not be confused with the ErrCodeNotFound
	// that a zero-match predicate returns. Only NotFound is the resolver's
	// graceful-exclusion signal, so an ambiguous predicate silently degrading
	// to NotFound would be a real fault this assertion would otherwise miss.
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeConflict, "")) {
		t.Errorf("error = %v, want code %s", err, aicrerrors.ErrCodeConflict)
	}
}

func TestPath_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path Path
		want string
	}{
		{
			name: "simple path",
			path: Path{
				Type:    TypeK8s,
				Subtype: "server",
				Key:     "version",
			},
			want: "K8s.server.version",
		},
		{
			name: "path with special chars",
			path: Path{
				Type:    TypeOS,
				Subtype: "sysctl",
				Key:     "/proc/sys/kernel/osrelease",
			},
			want: "OS.sysctl./proc/sys/kernel/osrelease",
		},
		{
			name: "index selector",
			path: Path{
				Type:     TypeNetworkTopology,
				Subtype:  "pfs",
				Key:      "rail",
				selector: &itemSelector{Raw: "0", Index: intPtr(0)},
			},
			want: "NetworkTopology.pfs[0].rail",
		},
		{
			name: "predicate selector",
			path: Path{
				Type:     TypeNetworkTopology,
				Subtype:  "pfs",
				Key:      "pciAddress",
				selector: &itemSelector{Raw: "rail=3", Predicate: &itemPredicate{Key: "rail", Value: "3"}},
			},
			want: "NetworkTopology.pfs[rail=3].pciAddress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := tt.path.String()
			if result != tt.want {
				t.Errorf("String() = %q, want %q", result, tt.want)
			}
		})
	}
}
