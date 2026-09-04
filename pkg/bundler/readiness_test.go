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

package bundler

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	stderrors "errors"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const validReadinessTestYAML = `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-readiness
`

func TestGateImage(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{"explicit v-prefixed version", "v1.2.3", "ghcr.io/nvidia/aicr-gate:v1.2.3"},
		{"release version without v prefix", "0.13.0", "ghcr.io/nvidia/aicr-gate:v0.13.0"},
		{"empty falls back to dev", "", "ghcr.io/nvidia/aicr-gate:dev"},
		{"dev stays dev", "dev", "ghcr.io/nvidia/aicr-gate:dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &DefaultBundler{Config: config.NewConfig(config.WithVersion(tt.version))}
			if got := b.gateImage(); got != tt.want {
				t.Errorf("gateImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateReadinessTestYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"valid", validReadinessTestYAML, false},
		{"wrong kind", "apiVersion: chainsaw.kyverno.io/v1alpha1\nkind: Policy\n", true},
		{"wrong apiVersion", "apiVersion: v1\nkind: Test\n", true},
		{"invalid yaml", ":\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateReadinessTestYAML("gpu-operator", []byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateReadinessTestYAML() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var se *aicrerrors.StructuredError
				if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
					t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
				}
			}
		})
	}
}

func TestCollectComponentReadiness(t *testing.T) {
	tmpData := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpData, "registry.yaml"), []byte("apiVersion: aicr.run/v1alpha2\nkind: ComponentRegistry\ncomponents: []\n"), 0o600); err != nil {
		t.Fatalf("WriteFile registry.yaml: %v", err)
	}
	compDir := filepath.Join(tmpData, "components", "gpu-operator")
	if err := os.MkdirAll(compDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(compDir, readinessFileName), []byte(validReadinessTestYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{ExternalDir: tmpData})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator", Namespace: "gpu-operator"}},
	}
	rr.BindDataProvider(layered)

	t.Run("disabled returns empty", func(t *testing.T) {
		b, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := b.collectComponentReadiness(context.Background(), rr)
		if err != nil {
			t.Fatalf("collectComponentReadiness: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d entries, want 0", len(got))
		}
	})

	t.Run("enabled collects manifest", func(t *testing.T) {
		b, err := New(WithConfig(config.NewConfig(
			config.WithReadinessHooks(true),
			config.WithDeployer(config.DeployerArgoCD),
			config.WithVersion("0.13.0"),
		)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := b.collectComponentReadiness(context.Background(), rr)
		if err != nil {
			t.Fatalf("collectComponentReadiness: %v", err)
		}
		manifests, ok := got["gpu-operator"]
		if !ok {
			t.Fatal("gpu-operator readiness missing")
		}
		body, ok := manifests[readinessManifestKey]
		if !ok {
			t.Fatal("readiness manifest key missing")
		}
		s := string(body)
		if !strings.Contains(s, "argocd.argoproj.io/sync-options: Replace=true,Force=true") {
			t.Errorf("missing Replace=true,Force=true:\n%s", s)
		}
		if !strings.Contains(s, "ghcr.io/nvidia/aicr-gate:v0.13.0") {
			t.Errorf("missing normalized gate image tag:\n%s", s)
		}
	})

	t.Run("network-operator readiness gate emitted when NCP manifest is attached", func(t *testing.T) {
		// Guards issue #2251: readiness.yaml for the Helm network-operator
		// must ship in the embedded FS so RDMA recipes get a NIC-stack
		// readiness gate. Uses the pure embedded provider so a rename/move
		// of the on-disk readiness.yaml breaks this test. Attaches the
		// aks.yaml-style NCP manifest under the network-operator ref so the
		// recipeAttachesNicClusterPolicy probe (#2337) returns true and the
		// gate is emitted.
		embeddedOnly := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
		nrr := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{
				Name:      "network-operator",
				Namespace: "nvidia-network-operator",
				ManifestFiles: []string{
					"components/network-operator/manifests/nic-cluster-policy-aks.yaml",
				},
			}},
		}
		nrr.BindDataProvider(embeddedOnly)

		b, err := New(WithConfig(config.NewConfig(
			config.WithReadinessHooks(true),
			config.WithDeployer(config.DeployerArgoCD),
		)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := b.collectComponentReadiness(context.Background(), nrr)
		if err != nil {
			t.Fatalf("collectComponentReadiness: %v", err)
		}
		manifests, ok := got["network-operator"]
		if !ok {
			t.Fatal("network-operator readiness manifest missing — recipes/components/network-operator/readiness.yaml not shipped?")
		}
		body := string(manifests[readinessManifestKey])
		for _, want := range []string{
			"kind: NicClusterPolicy",
			"apiVersion: mellanox.com/v1alpha1",
			"state: ready",
			// RBAC for the NCP read must be granted by the gate ClusterRole,
			// narrowed to the nicclusterpolicies resource (least privilege).
			`  - apiGroups: ["mellanox.com"]
    resources: ["nicclusterpolicies"]
    verbs: ["get", "list", "watch"]`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("network-operator readiness manifest missing %q:\n%s", want, body)
			}
		}
		// Match-any-by-state design: the assert must NOT pin
		// metadata.name to a specific NCP. Regex targets the pin
		// structure (metadata: followed by an indented name line)
		// so mentions of nic-cluster-policy in the file comments or
		// as manifest paths (recipes/components/.../manifests/
		// nic-cluster-policy-aks.yaml) are not confused with the
		// pin itself.
		pinPattern := regexp.MustCompile(`metadata:\s*\n\s+name:\s+nic-cluster-policy\b`)
		if pinPattern.MatchString(body) {
			t.Errorf("network-operator readiness manifest must not pin nic-cluster-policy name:\n%s", body)
		}
	})

	t.Run("network-operator gate skipped when no NCP manifest is attached (kind shape)", func(t *testing.T) {
		// Guards issue #2337: recipes like kind and Talos-mixin bases
		// include the network-operator component but attach no NCP CR;
		// the pinned Helm chart does not template one either, so the gate
		// would poll to --max-wait timeout. Emission must be suppressed.
		embeddedOnly := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
		nrr := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{
				Name:      "network-operator",
				Namespace: "nvidia-network-operator",
				// Attach a non-NCP manifest (mirrors os-talos.yaml which
				// attaches only a Namespace) — the probe must not
				// misidentify this as an NCP declaration.
				ManifestFiles: []string{
					"components/network-operator/manifests/talos-namespace.yaml",
				},
			}},
		}
		nrr.BindDataProvider(embeddedOnly)

		b, err := New(WithConfig(config.NewConfig(
			config.WithReadinessHooks(true),
			config.WithDeployer(config.DeployerArgoCD),
		)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := b.collectComponentReadiness(context.Background(), nrr)
		if err != nil {
			t.Fatalf("collectComponentReadiness: %v", err)
		}
		if _, present := got["network-operator"]; present {
			t.Fatal("network-operator readiness gate must be skipped when no NCP is attached; got a gate manifest")
		}
	})

	t.Run("network-operator gate emitted when NCP is attached by a sibling ref", func(t *testing.T) {
		// The NCP is often attached by a different ref than the one
		// whose readiness gate needs it (e.g., a hypothetical overlay
		// splits network-operator across a base ref and an
		// NCP-attaching ref). The probe must see the whole recipe.
		embeddedOnly := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
		nrr := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{Name: "network-operator", Namespace: "nvidia-network-operator"},
				{Name: "sidecar", ManifestFiles: []string{
					"components/network-operator/manifests/nic-cluster-policy-aks.yaml",
				}},
			},
		}
		nrr.BindDataProvider(embeddedOnly)

		b, err := New(WithConfig(config.NewConfig(
			config.WithReadinessHooks(true),
			config.WithDeployer(config.DeployerArgoCD),
		)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := b.collectComponentReadiness(context.Background(), nrr)
		if err != nil {
			t.Fatalf("collectComponentReadiness: %v", err)
		}
		if _, present := got["network-operator"]; !present {
			t.Fatal("network-operator readiness gate missing; probe must scan every ref's manifests, not only the ref being emitted")
		}
	})

	t.Run("malformed test rejected", func(t *testing.T) {
		badDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(badDir, "registry.yaml"), []byte("apiVersion: aicr.run/v1alpha2\nkind: ComponentRegistry\ncomponents: []\n"), 0o600); err != nil {
			t.Fatalf("WriteFile registry.yaml: %v", err)
		}
		badComp := filepath.Join(badDir, "components", "gpu-operator")
		if err := os.MkdirAll(badComp, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(badComp, readinessFileName), []byte("kind: ConfigMap\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		badLayered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{ExternalDir: badDir})
		if err != nil {
			t.Fatalf("NewLayeredDataProvider: %v", err)
		}
		badRR := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}},
		}
		badRR.BindDataProvider(badLayered)

		b, err := New(WithConfig(config.NewConfig(config.WithReadinessHooks(true))))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := b.collectComponentReadiness(context.Background(), badRR); err == nil {
			t.Fatal("expected error for malformed readiness.yaml")
		}
	})
}

func TestRecipeAttachesNicClusterPolicy(t *testing.T) {
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	tests := []struct {
		name string
		refs []recipe.ComponentRef
		want bool
	}{
		{
			name: "empty refs",
			refs: nil,
			want: false,
		},
		{
			name: "ref with no manifests",
			refs: []recipe.ComponentRef{{Name: "network-operator"}},
			want: false,
		},
		{
			name: "ref attaches non-NCP manifest",
			refs: []recipe.ComponentRef{{
				Name:          "network-operator",
				ManifestFiles: []string{"components/network-operator/manifests/talos-namespace.yaml"},
			}},
			want: false,
		},
		{
			name: "ref attaches NCP via ManifestFiles",
			refs: []recipe.ComponentRef{{
				Name:          "network-operator",
				ManifestFiles: []string{"components/network-operator/manifests/nic-cluster-policy-aks.yaml"},
			}},
			want: true,
		},
		{
			name: "ref attaches NCP via PreManifestFiles",
			refs: []recipe.ComponentRef{{
				Name:             "network-operator",
				PreManifestFiles: []string{"components/network-operator/manifests/nic-cluster-policy-aks.yaml"},
			}},
			want: true,
		},
		{
			name: "NCP attached by a sibling ref",
			refs: []recipe.ComponentRef{
				{Name: "network-operator"},
				{Name: "sidecar", ManifestFiles: []string{
					"components/network-operator/manifests/nic-cluster-policy-aks.yaml",
				}},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := recipeAttachesNicClusterPolicy(context.Background(), provider, tt.refs)
			if err != nil {
				t.Fatalf("recipeAttachesNicClusterPolicy: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWarnPeermemReadinessDisabled(t *testing.T) {
	tests := []struct {
		name            string
		criteria        recipe.Criteria
		profile         string
		readinessHooks  bool
		removeComponent string
		wantWarning     bool
		wantGPUOperator string
		wantNetworkOp   string
	}{
		{
			name: "AKS operator-managed peermem warns",
			criteria: recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				Intent: recipe.CriteriaIntentTraining,
			},
			profile:         "gpuStack=operator-managed",
			wantWarning:     true,
			wantGPUOperator: gpuOperatorComponentName,
			wantNetworkOp:   networkOperatorComponentName,
		},
		{
			name: "AKS azure-managed driver is unaffected",
			criteria: recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				Intent: recipe.CriteriaIntentTraining,
			},
			profile: "gpuStack=azure-managed",
		},
		{
			name: "AKS operator-managed with readiness hooks is gated",
			criteria: recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				Intent: recipe.CriteriaIntentTraining,
			},
			profile:        "gpuStack=operator-managed",
			readinessHooks: true,
		},
		{
			name: "OCP peermem warns",
			criteria: recipe.Criteria{
				Service: recipe.CriteriaServiceOCP,
				Intent:  recipe.CriteriaIntentTraining,
			},
			wantWarning:     true,
			wantGPUOperator: "gpu-operator-ocp",
			wantNetworkOp:   networkOperatorOCPComponentName,
		},
		{
			name: "OCP with readiness hooks is gated",
			criteria: recipe.Criteria{
				Service: recipe.CriteriaServiceOCP,
				Intent:  recipe.CriteriaIntentTraining,
			},
			readinessHooks: true,
		},
		{
			name: "DMA-BUF EKS recipe is unaffected",
			criteria: recipe.Criteria{
				Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
		},
		{
			name: "subset without Network Operator is externally satisfied",
			criteria: recipe.Criteria{
				Service: recipe.CriteriaServiceOCP,
				Intent:  recipe.CriteriaIntentTraining,
			},
			removeComponent: networkOperatorOCPComponentName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			criteria := tt.criteria
			result, err := recipe.NewBuilder().BuildFromCriteriaWithProfile(
				t.Context(), &criteria, tt.profile)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
			}

			cfg := config.NewConfig(config.WithReadinessHooks(tt.readinessHooks))
			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			values, err := b.extractComponentValues(t.Context(), result)
			if err != nil {
				t.Fatalf("extractComponentValues() error = %v", err)
			}
			delete(values, tt.removeComponent)

			if err := b.warnPeermemReadinessDisabled(t.Context(), result, values); err != nil {
				t.Fatalf("warnPeermemReadinessDisabled() error = %v", err)
			}

			if got := len(b.warnings); got != boolCount(tt.wantWarning) {
				t.Fatalf("warnings = %v, want warning=%v", b.warnings, tt.wantWarning)
			}
			if !tt.wantWarning {
				return
			}
			warning := b.warnings[0]
			for _, want := range []string{
				"Warning: ", tt.wantGPUOperator, tt.wantNetworkOp,
				gpuDriverEnabledValuePath + "=true",
				gpuDriverRDMAEnabledValuePath + "=true",
				"dependencyRefs only orders manifest submission",
				"NicClusterPolicy.status.state=ready",
				"--readiness-hooks", "Flux", "Helmfile", "REST API", "#2499",
			} {
				if !strings.Contains(warning, want) {
					t.Errorf("warning = %q, want substring %q", warning, want)
				}
			}
		})
	}
}

func TestMakePeermemRecipeWithoutReadinessHooksSurfacesWarning(t *testing.T) {
	criteria := &recipe.Criteria{
		Service: recipe.CriteriaServiceOCP,
		Intent:  recipe.CriteriaIntentInference,
	}
	resolved, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}
	b, err := New(WithConfig(config.NewConfig(config.WithVersion("v-test"))))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	output, err := b.Make(t.Context(), resolved, t.TempDir())
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}
	if output.Deployment == nil {
		t.Fatal("Make() returned nil deployment info")
	}

	joined := strings.Join(output.Deployment.Notes, "\n")
	for _, want := range []string{
		"gpu-operator-ocp", "network-operator-ocp",
		"NicClusterPolicy.status.state=ready", "--readiness-hooks",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("deployment notes = %q, want substring %q", joined, want)
		}
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

// TestRecipeAttachesNicClusterPolicy_CanceledContext confirms an already-
// canceled ctx is surfaced as ErrCodeCanceled through wrapCtxErr rather
// than swallowed into a false-negative "no NCP" answer that would silently
// suppress the readiness gate for a real RDMA recipe.
func TestRecipeAttachesNicClusterPolicy_CanceledContext(t *testing.T) {
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	refs := []recipe.ComponentRef{{
		Name:          "network-operator",
		ManifestFiles: []string{"components/network-operator/manifests/nic-cluster-policy-aks.yaml"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := recipeAttachesNicClusterPolicy(ctx, provider, refs)
	if err == nil {
		t.Fatalf("want cancellation error; got present=%v err=nil", got)
	}
	if got {
		t.Fatalf("cancellation must not report present=true; got present=%v", got)
	}
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("want errors.Is(err, context.Canceled); got %v", err)
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeCanceled {
		t.Fatalf("want ErrCodeCanceled; got %v", err)
	}
}

// TestRecipeAttachesNicClusterPolicy_ReadError confirms a missing manifest
// path surfaces a wrapped read error carrying ErrCodeNotFound (propagated
// via PropagateOrWrap) rather than degrading to (false, nil) — the latter
// would silently skip the readiness gate on a real RDMA recipe when the
// probe hits transient FS trouble.
func TestRecipeAttachesNicClusterPolicy_ReadError(t *testing.T) {
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	refs := []recipe.ComponentRef{{
		Name:          "network-operator",
		ManifestFiles: []string{"components/network-operator/manifests/does-not-exist.yaml"},
	}}

	got, err := recipeAttachesNicClusterPolicy(context.Background(), provider, refs)
	if err == nil {
		t.Fatalf("want read error; got present=%v err=nil", got)
	}
	if got {
		t.Fatalf("read error must not report present=true; got present=%v", got)
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
		t.Fatalf("want ErrCodeNotFound (propagated from GetManifestContentWithContext); got %v", err)
	}
}

// TestNCPRegexNearMiss pins ncpKindRE and ncpAPIVersionRE against the
// near-miss shapes their comments claim to reject: commented lines, indented
// lines, and single-signal (kind-only or apiVersion-only) documents. Also
// pins the forward-compatibility claim that a v1beta1 apiVersion still
// matches when paired with the kind. A future edit that drops (?m), the ^
// anchor, or the AND between the two patterns would silently re-introduce
// the #2337 false-emit regression; this test fails it instead.
func TestNCPRegexNearMiss(t *testing.T) {
	// AND semantics used by recipeAttachesNicClusterPolicy — encode it here
	// so a same-doc match is what the assertion actually pins.
	matches := func(doc string) bool {
		return ncpKindRE.MatchString(doc) && ncpAPIVersionRE.MatchString(doc)
	}
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{
			name: "commented kind + apiVersion is not a match",
			doc:  "# kind: NicClusterPolicy\n# apiVersion: mellanox.com/v1alpha1\n",
			want: false,
		},
		{
			name: "indented kind + apiVersion is not a match (list item shape)",
			doc:  "items:\n  - kind: NicClusterPolicy\n    apiVersion: mellanox.com/v1alpha1\n",
			want: false,
		},
		{
			name: "apiVersion alone is not a match",
			doc:  "apiVersion: mellanox.com/v1alpha1\nkind: Something\n",
			want: false,
		},
		{
			name: "kind alone is not a match",
			doc:  "apiVersion: v1\nkind: NicClusterPolicy\n",
			want: false,
		},
		{
			name: "kind + v1alpha1 apiVersion is a match",
			doc:  "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\n",
			want: true,
		},
		{
			name: "kind + v1beta1 apiVersion is a match (forward-compat)",
			doc:  "apiVersion: mellanox.com/v1beta1\nkind: NicClusterPolicy\n",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matches(tt.doc); got != tt.want {
				t.Errorf("matches(%q) = %v, want %v", tt.doc, got, tt.want)
			}
		})
	}
}

func TestWrapCtxErr(t *testing.T) {
	// Pins the invariants required by the review: the returned error must
	// (a) carry the distinguished ErrCode — ErrCodeCanceled for explicit
	// cancellation, ErrCodeTimeout for deadline expiration — (b) preserve
	// the underlying context sentinel through Unwrap so callers can branch
	// on stderrors.Is, and (c) surface a message that distinguishes the
	// two exit paths.
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := wrapCtxErr(ctx, "unit test")
		if !stderrors.Is(err, context.Canceled) {
			t.Fatalf("want errors.Is(err, context.Canceled); got %v", err)
		}
		if stderrors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("must not conflate cancellation with deadline; got %v", err)
		}
		var se *aicrerrors.StructuredError
		if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeCanceled {
			t.Fatalf("want ErrCodeCanceled; got %v", err)
		}
		if !strings.Contains(err.Error(), "cancelled") {
			t.Errorf("message should distinguish cancellation; got %q", err.Error())
		}
	})
	t.Run("deadline exceeded", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
		defer cancel()
		<-ctx.Done() // ensure ctx.Err() has flipped to DeadlineExceeded before the call
		err := wrapCtxErr(ctx, "unit test")
		if !stderrors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want errors.Is(err, context.DeadlineExceeded); got %v", err)
		}
		if stderrors.Is(err, context.Canceled) {
			t.Fatalf("must not conflate deadline with cancellation; got %v", err)
		}
		var se *aicrerrors.StructuredError
		if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeTimeout {
			t.Fatalf("want ErrCodeTimeout; got %v", err)
		}
		if !strings.Contains(err.Error(), "deadline exceeded") {
			t.Errorf("message should distinguish deadline; got %q", err.Error())
		}
	})
}

func TestBuildDeployer_ReadinessHooksUnsupportedDeployer(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig(
		config.WithReadinessHooks(true),
		config.WithDeployer(config.DeployerFlux),
	)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}
	_, err = b.buildDeployer(context.Background(), rr, map[string]map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected error for flux + readiness-hooks")
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
	}
}
