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
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// tcpxoTestMapping returns a valid ordered mapping with distinct networks.
// Tests mutate a copy for the negative cases.
func tcpxoTestMapping() []NetworkInterfaceMapping {
	return []NetworkInterfaceMapping{
		{InterfaceName: "eth1", Network: "gpu-nic-0"},
		{InterfaceName: "eth2", Network: "gpu-nic-1"},
		{InterfaceName: "eth3", Network: "gpu-nic-2"},
		{InterfaceName: "eth4", Network: "gpu-nic-3"},
		{InterfaceName: "eth5", Network: "gpu-nic-4"},
		{InterfaceName: "eth6", Network: "gpu-nic-5"},
		{InterfaceName: "eth7", Network: "gpu-nic-6"},
		{InterfaceName: "eth8", Network: "gpu-nic-7"},
	}
}

// tcpxoTestResult is a minimal h100 GKE kubeflow recipe that ships the
// torch-distributed-tcpxo runtime: both components present and enabled.
func tcpxoTestResult() *RecipeResult {
	return &RecipeResult{
		Kind:       RecipeResultKind,
		APIVersion: ConfiguredRecipeResultAPIVersion,
		Criteria: &Criteria{
			Service:     CriteriaServiceGKE,
			Accelerator: CriteriaAcceleratorH100,
			OS:          CriteriaOSCOS,
			Intent:      CriteriaIntentTraining,
			Platform:    CriteriaPlatformKubeflow,
		},
		ComponentRefs: []ComponentRef{
			{Name: "gpu-operator", Type: ComponentTypeHelm, Namespace: "gpu-operator"},
			{Name: gkeNCCLTCPXOComponentName, Type: ComponentTypeHelm, Namespace: "kube-system"},
			{Name: kubeflowTrainerComponentName, Type: ComponentTypeHelm, Namespace: "kubeflow", ManifestFiles: []string{gkeTCPXORuntimeManifest}},
		},
	}
}

func TestParseGKETCPXOInterfaces(t *testing.T) {
	t.Parallel()

	t.Run("round-trips the canonical form", func(t *testing.T) {
		t.Parallel()
		want := tcpxoTestMapping()
		got, err := ParseGKETCPXOInterfaces(FormatGKETCPXOInterfaces(want))
		if err != nil {
			t.Fatalf("ParseGKETCPXOInterfaces() error = %v", err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("ParseGKETCPXOInterfaces() = %v, want %v", got, want)
		}
	})

	t.Run("preserves supplied order", func(t *testing.T) {
		t.Parallel()
		// Order is part of the recorded value even though the interface
		// names fully key the mapping: the emitted recipe must read the way
		// the operator wrote it.
		got, err := ParseGKETCPXOInterfaces(
			"eth8=gpu-nic-7,eth7=gpu-nic-6,eth6=gpu-nic-5,eth5=gpu-nic-4," +
				"eth4=gpu-nic-3,eth3=gpu-nic-2,eth2=gpu-nic-1,eth1=gpu-nic-0")
		if err != nil {
			t.Fatalf("ParseGKETCPXOInterfaces() error = %v", err)
		}
		if got[0].InterfaceName != "eth8" {
			t.Errorf("ParseGKETCPXOInterfaces() first entry = %v, want eth8", got[0])
		}
	})

	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "missing network", input: "eth1=,eth2=a,eth3=b,eth4=c,eth5=d,eth6=e,eth7=f,eth8=g"},
		{name: "missing separator", input: "eth1,eth2=a,eth3=b,eth4=c,eth5=d,eth6=e,eth7=f,eth8=g"},
		{name: "seven entries", input: "eth1=a,eth2=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g"},
		{name: "nine entries", input: "eth1=a,eth2=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g,eth8=h,eth1=i"},
		{name: "eth0 not accepted", input: "eth0=a,eth2=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g,eth8=h"},
		{name: "non-eth interface", input: "ens1=a,eth2=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g,eth8=h"},
		{name: "duplicate interface", input: "eth1=a,eth1=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g,eth8=h"},
		{name: "duplicate network", input: "eth1=a,eth2=a,eth3=b,eth4=c,eth5=d,eth6=e,eth7=f,eth8=g"},
		{name: "uppercase network", input: "eth1=GPU-NIC-0,eth2=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g,eth8=h"},
		{name: "network with underscore", input: "eth1=gpu_nic_0,eth2=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g,eth8=h"},
		{name: "network starting with digit", input: "eth1=0gpu-nic,eth2=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g,eth8=h"},
		{name: "network with trailing dash", input: "eth1=gpu-nic-,eth2=b,eth3=c,eth4=d,eth5=e,eth6=f,eth7=g,eth8=h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseGKETCPXOInterfaces(tt.input); err == nil {
				t.Errorf("ParseGKETCPXOInterfaces(%q) = nil error, want rejection", tt.input)
			}
		})
	}

	t.Run("prefixed cluster network names accepted", func(t *testing.T) {
		t.Parallel()
		// The integrator doc's supported form: names carry the cluster
		// prefix, e.g. aicr-demo2-gpu-nic-0.
		if _, err := ParseGKETCPXOInterfaces(
			"eth1=aicr-demo2-gpu-nic-0,eth2=aicr-demo2-gpu-nic-1,eth3=aicr-demo2-gpu-nic-2," +
				"eth4=aicr-demo2-gpu-nic-3,eth5=aicr-demo2-gpu-nic-4,eth6=aicr-demo2-gpu-nic-5," +
				"eth7=aicr-demo2-gpu-nic-6,eth8=aicr-demo2-gpu-nic-7"); err != nil {
			t.Errorf("ParseGKETCPXOInterfaces() prefixed names rejected: %v", err)
		}
	})
}

func TestResolveBuildConfigGKETCPXOInterfaces(t *testing.T) {
	t.Parallel()

	mapping := tcpxoTestMapping()

	tests := []struct {
		name     string
		criteria *Criteria
		wantErr  string
	}{
		{
			name:     "gke h100 accepted",
			criteria: &Criteria{Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorH100},
		},
		{
			name:     "wildcard criteria deferred to apply time",
			criteria: &Criteria{Service: CriteriaServiceAny, Accelerator: CriteriaAcceleratorAny},
		},
		{
			name:     "empty criteria deferred to apply time",
			criteria: &Criteria{},
		},
		{
			name:     "concrete non-gke service rejected",
			criteria: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100},
			wantErr:  "service is gke",
		},
		{
			name:     "concrete non-h100 accelerator rejected",
			criteria: &Criteria{Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorB200},
			wantErr:  "h100 (a3-megagpu-8g) recipes only",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveBuildConfig(tt.criteria, WithGKETCPXOInterfaces(mapping))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("resolveBuildConfig() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resolveBuildConfig() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}

	t.Run("invalid mapping rejected at the boundary", func(t *testing.T) {
		t.Parallel()
		bad := tcpxoTestMapping()[:4]
		if _, err := resolveBuildConfig(&Criteria{Service: CriteriaServiceGKE},
			WithGKETCPXOInterfaces(bad)); err == nil {
			t.Fatal("resolveBuildConfig() = nil error, want rejection of a 4-entry mapping")
		}
	})
}

func TestApplyGKETCPXOInterfaces(t *testing.T) {
	t.Parallel()

	t.Run("records configuration and projects the override", func(t *testing.T) {
		t.Parallel()
		result := tcpxoTestResult()
		mapping := tcpxoTestMapping()

		if err := applyGKETCPXOInterfaces(result, &mapping); err != nil {
			t.Fatalf("applyGKETCPXOInterfaces() error = %v", err)
		}

		got, present := result.GKETCPXOInterfaces()
		if !present {
			t.Fatal("GKETCPXOInterfaces() present = false, want recorded value")
		}
		if !slices.Equal(got, mapping) {
			t.Errorf("GKETCPXOInterfaces() = %v, want %v", got, mapping)
		}
		if result.APIVersion != ConfiguredRecipeResultAPIVersion {
			t.Errorf("APIVersion = %q, want %q", result.APIVersion, ConfiguredRecipeResultAPIVersion)
		}

		// The override is the render input: it must carry the same ordered
		// mapping as the recorded configuration.
		ref := result.GetComponentRef(kubeflowTrainerComponentName)
		projected, err := NormalizeGKETCPXOInterfaces(ref.Overrides[gkeTCPXOInterfacesValueKey])
		if err != nil {
			t.Fatalf("NormalizeGKETCPXOInterfaces() error = %v", err)
		}
		if !slices.Equal(projected, mapping) {
			t.Errorf("projected override = %v, want %v", projected, mapping)
		}
	})

	t.Run("fails closed when a TCPXO recipe supplies no mapping", func(t *testing.T) {
		t.Parallel()
		result := tcpxoTestResult()
		err := applyGKETCPXOInterfaces(result, nil)
		if err == nil {
			t.Fatal("applyGKETCPXOInterfaces(nil) = nil error on a TCPXO recipe, want fail-closed")
		}
		if !strings.Contains(err.Error(), "--gke-tcpxo-interfaces") {
			t.Errorf("error should name the remedy flag, got: %v", err)
		}
	})

	t.Run("no-op on a non-TCPXO recipe without a mapping", func(t *testing.T) {
		t.Parallel()
		result := tcpxoTestResult()
		result.ComponentRefs = result.ComponentRefs[:1] // gpu-operator only
		if err := applyGKETCPXOInterfaces(result, nil); err != nil {
			t.Fatalf("applyGKETCPXOInterfaces() error = %v, want nil", err)
		}
		if _, present := result.GKETCPXOInterfaces(); present {
			t.Error("GKETCPXOInterfaces() present = true on a non-TCPXO recipe")
		}
	})

	t.Run("rejected when kubeflow-trainer is absent", func(t *testing.T) {
		t.Parallel()
		result := tcpxoTestResult()
		result.ComponentRefs = result.ComponentRefs[:2] // drop kubeflow-trainer
		mapping := tcpxoTestMapping()
		if err := applyGKETCPXOInterfaces(result, &mapping); err == nil {
			t.Fatal("applyGKETCPXOInterfaces() = nil error without kubeflow-trainer, want rejection")
		}
	})

	t.Run("rejected on a non-h100 recipe", func(t *testing.T) {
		t.Parallel()
		result := tcpxoTestResult()
		result.Criteria.Accelerator = CriteriaAcceleratorB200
		mapping := tcpxoTestMapping()
		err := applyGKETCPXOInterfaces(result, &mapping)
		if err == nil || !strings.Contains(err.Error(), "h100") {
			t.Fatalf("applyGKETCPXOInterfaces() error = %v, want h100 scoping rejection", err)
		}
	})

	t.Run("rejected when the tcpxo component is disabled by the recipe", func(t *testing.T) {
		t.Parallel()
		result := tcpxoTestResult()
		disabled := false
		for i := range result.ComponentRefs {
			if result.ComponentRefs[i].Name == gkeNCCLTCPXOComponentName {
				result.ComponentRefs[i].Overrides = map[string]any{"enabled": disabled}
			}
		}
		mapping := tcpxoTestMapping()
		if err := applyGKETCPXOInterfaces(result, &mapping); err == nil {
			t.Fatal("applyGKETCPXOInterfaces() = nil error with gke-nccl-tcpxo disabled, want rejection")
		}
	})
}

func TestShipsGKETCPXORuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*RecipeResult)
		want   bool
	}{
		{name: "fingerprint family", mutate: func(*RecipeResult) {}, want: true},
		{
			name: "kubeflow-trainer missing",
			mutate: func(r *RecipeResult) {
				r.ComponentRefs = r.ComponentRefs[:2]
			},
			want: false,
		},
		{
			name: "runtime declaration missing",
			mutate: func(r *RecipeResult) {
				r.ComponentRefs[2].ManifestFiles = nil
			},
			want: false,
		},
		{
			name:   "declaration is authoritative even with incorrect accelerator",
			mutate: func(r *RecipeResult) { r.Criteria.Accelerator = CriteriaAcceleratorA100 },
			want:   true,
		},
		{
			name: "component disabled",
			mutate: func(r *RecipeResult) {
				r.ComponentRefs[2].Overrides = map[string]any{"enabled": false}
			},
			want: false,
		},
		{
			name:   "nil criteria",
			mutate: func(r *RecipeResult) { r.Criteria = nil },
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := tcpxoTestResult()
			tt.mutate(result)
			if got := result.ShipsGKETCPXORuntime(); got != tt.want {
				t.Errorf("ShipsGKETCPXORuntime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateGKEConfiguration(t *testing.T) {
	t.Parallel()

	// appliedResult returns a fingerprint recipe with the mapping applied.
	appliedResult := func(t *testing.T) *RecipeResult {
		t.Helper()
		result := tcpxoTestResult()
		mapping := tcpxoTestMapping()
		if err := applyGKETCPXOInterfaces(result, &mapping); err != nil {
			t.Fatalf("applyGKETCPXOInterfaces() error = %v", err)
		}
		return result
	}

	t.Run("applied recipe is coherent", func(t *testing.T) {
		t.Parallel()
		if err := appliedResult(t).validateGKEConfiguration(); err != nil {
			t.Fatalf("validateGKEConfiguration() error = %v", err)
		}
	})

	t.Run("hand-edited override disagrees with the recorded value", func(t *testing.T) {
		t.Parallel()
		result := appliedResult(t)
		ref := result.GetComponentRef(kubeflowTrainerComponentName)
		// Change one network in the projected override only — the recipe now
		// states one mapping and renders another.
		override := ref.Overrides[gkeTCPXOInterfacesValueKey].([]any)
		override[3].(map[string]any)["network"] = "someone-elses-network"
		err := result.validateGKEConfiguration()
		if err == nil || !strings.Contains(err.Error(), "disagrees") {
			t.Fatalf("validateGKEConfiguration() error = %v, want drift rejection", err)
		}
	})

	t.Run("override removed by hand", func(t *testing.T) {
		t.Parallel()
		result := appliedResult(t)
		ref := result.GetComponentRef(kubeflowTrainerComponentName)
		delete(ref.Overrides, gkeTCPXOInterfacesValueKey)
		if err := result.validateGKEConfiguration(); err == nil {
			t.Fatal("validateGKEConfiguration() = nil error with the override removed, want rejection")
		}
	})

	t.Run("recorded value re-validated on load", func(t *testing.T) {
		t.Parallel()
		result := appliedResult(t)
		result.Configuration.GKE.TCPXOInterfaces = result.Configuration.GKE.TCPXOInterfaces[:4]
		if err := result.validateGKEConfiguration(); err == nil {
			t.Fatal("validateGKEConfiguration() = nil error with a 4-entry recorded mapping, want rejection")
		}
	})

	t.Run("rejected on a non-GKE recipe", func(t *testing.T) {
		t.Parallel()
		result := appliedResult(t)
		result.Criteria.Service = CriteriaServiceEKS
		if err := result.validateGKEConfiguration(); err == nil {
			t.Fatal("validateGKEConfiguration() = nil error on an EKS recipe, want rejection")
		}
	})

	t.Run("unsupported apiVersion rejected", func(t *testing.T) {
		t.Parallel()
		result := appliedResult(t)
		result.APIVersion = "aicr.run/v1alpha2"
		err := result.validateGKEConfiguration()
		if err == nil || !strings.Contains(err.Error(), "apiVersion") {
			t.Fatalf("validateGKEConfiguration() error = %v, want apiVersion rejection", err)
		}
	})

	t.Run("no configuration is a no-op", func(t *testing.T) {
		t.Parallel()
		if err := tcpxoTestResult().validateGKEConfiguration(); err != nil {
			t.Fatalf("validateGKEConfiguration() error = %v, want nil without configuration", err)
		}
	})
}

func TestGKETCPXOOwnership(t *testing.T) {
	t.Parallel()

	domain := GKETCPXOOwnership()
	paths, ok := domain.Paths[kubeflowTrainerComponentName]
	if !ok {
		t.Fatalf("GKETCPXOOwnership() has no %q entry", kubeflowTrainerComponentName)
	}
	if !slices.Equal(paths, []string{gkeTCPXOInterfacesValueKey}) {
		t.Errorf("GKETCPXOOwnership() paths = %v, want [%s]", paths, gkeTCPXOInterfacesValueKey)
	}
	if domain.Name == "" {
		t.Error("GKETCPXOOwnership() Name is empty")
	}
}

// TestDeepCopyPreservesGKETCPXOInterfaces guards the adoption path:
// Client.AdoptRecipe deep-copies every incoming recipe, and a configuration
// section omitted from DeepCopy is silently dropped — the bundle then fails
// closed against a mapping that was present in the caller's artifact.
func TestDeepCopyPreservesGKETCPXOInterfaces(t *testing.T) {
	t.Parallel()

	result := tcpxoTestResult()
	mapping := tcpxoTestMapping()
	if err := applyGKETCPXOInterfaces(result, &mapping); err != nil {
		t.Fatalf("applyGKETCPXOInterfaces() error = %v", err)
	}

	clone := result.DeepCopy()
	got, present := clone.GKETCPXOInterfaces()
	if !present {
		t.Fatal("DeepCopy dropped configuration.gke")
	}
	if !slices.Equal(got, mapping) {
		t.Errorf("cloned mapping = %v, want %v", got, mapping)
	}

	// A copy, not an alias: mutating the clone must not reach the original.
	clone.Configuration.GKE.TCPXOInterfaces[0].Network = "mutated-network"
	orig, _ := result.GKETCPXOInterfaces()
	if orig[0].Network == "mutated-network" {
		t.Error("original mapping changed after mutating the clone; the slice is shared")
	}
}

// TestQueryHydrationExposesGKETCPXOInterfaces covers the selector surface:
// `aicr query --selector configuration.gke.tcpxoInterfaces` must see the
// recorded mapping, not report the section missing.
func TestQueryHydrationExposesGKETCPXOInterfaces(t *testing.T) {
	t.Parallel()

	result := tcpxoTestResult()
	mapping := tcpxoTestMapping()
	if err := applyGKETCPXOInterfaces(result, &mapping); err != nil {
		t.Fatalf("applyGKETCPXOInterfaces() error = %v", err)
	}

	hydrated, err := HydrateResult(result)
	if err != nil {
		t.Fatalf("Hydrate() error = %v", err)
	}
	got, err := Select(hydrated, "configuration.gke.tcpxoInterfaces")
	if err != nil {
		t.Fatalf("Select(configuration.gke.tcpxoInterfaces) error = %v", err)
	}
	entries, ok := got.([]any)
	if !ok {
		t.Fatalf("selector returned %T, want []any", got)
	}
	if len(entries) != len(mapping) {
		t.Fatalf("selector returned %d entries, want %d", len(entries), len(mapping))
	}
	first, ok := entries[0].(map[string]any)
	if !ok || first["interfaceName"] != "eth1" || first["network"] != "gpu-nic-0" {
		t.Errorf("first entry = %v, want eth1→gpu-nic-0", entries[0])
	}
}

// TestApplyGKETCPXOInterfacesAcceptsReorderedPairs pins the contract: the
// explicit interfaceName key, not the list position, binds network to
// interface. The provisioner may record pairs in any order.
func TestApplyGKETCPXOInterfacesAcceptsReorderedPairs(t *testing.T) {
	t.Parallel()

	result := tcpxoTestResult()
	mapping := tcpxoTestMapping()
	slices.Reverse(mapping)
	if err := applyGKETCPXOInterfaces(result, &mapping); err != nil {
		t.Fatalf("applyGKETCPXOInterfaces() error = %v", err)
	}
	got, _ := result.GKETCPXOInterfaces()
	if !slices.Equal(got, mapping) {
		t.Errorf("recorded mapping = %v, want supplied order %v", got, mapping)
	}
}

// TestWithGKETCPXOInterfacesDefensiveCopy guards the option boundary: the
// caller may reuse or mutate the slice after passing it.
func TestWithGKETCPXOInterfacesDefensiveCopy(t *testing.T) {
	t.Parallel()

	mapping := tcpxoTestMapping()
	opt := WithGKETCPXOInterfaces(mapping)
	mapping[0].Network = "mutated-after-construction"

	cfg := &buildConfig{}
	opt(cfg)
	if (*cfg.tcpxoInterfaces)[0].Network == "mutated-after-construction" {
		t.Error("build option aliased the caller's slice; later mutation changed the recorded value")
	}
}

func TestIsMissingGKETCPXOInterfaces(t *testing.T) {
	t.Parallel()

	result := tcpxoTestResult()
	missingErr := applyGKETCPXOInterfaces(result, nil)
	if !IsMissingGKETCPXOInterfaces(missingErr) {
		t.Fatal("IsMissingGKETCPXOInterfaces() = false for the fail-closed error")
	}
	if IsMissingGKETCPXOInterfaces(errors.New(errors.ErrCodeInvalidRequest, "some other rejection")) {
		t.Fatal("IsMissingGKETCPXOInterfaces() = true for an unrelated invalid-request error")
	}
	if IsMissingGKETCPXOInterfaces(nil) {
		t.Fatal("IsMissingGKETCPXOInterfaces() = true for nil")
	}
}
