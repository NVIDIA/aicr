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

package main

import (
	stderrors "errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
)

// shippedMapping is the eight-NIC mapping a GKE cluster named "c1" records.
func shippedMapping() []recipe.NetworkInterfaceMapping {
	out := make([]recipe.NetworkInterfaceMapping, 0, 8)
	for i := range 8 {
		out = append(out, recipe.NetworkInterfaceMapping{
			InterfaceName: "eth" + string(rune('1'+i)), Network: "c1-gpu-nic-" + string(rune('0'+i))})
	}
	return out
}

func interfacesAnnotation(m []recipe.NetworkInterfaceMapping) string {
	s := `[{"interfaceName":"eth0","network":"default"}`
	for _, e := range m {
		s += `,{"interfaceName":"` + e.InterfaceName + `","network":"` + e.Network + `"}`
	}
	return s + "]"
}

// shippedTCPXORuntime mirrors the shape of torch-distributed-tcpxo-cluster-
// training-runtime.yaml after Helm rendering: one node job, mlPolicy.torch,
// fabric annotations in template.metadata, the tcpxo-daemon sidecar, NCCL env
// on the worker, no worker command (Trainer injects torchrun).
func shippedTCPXORuntime(m []recipe.NetworkInterfaceMapping) *unstructured.Unstructured {
	worker := map[string]any{
		"name":  "node",
		"image": "pytorch/pytorch:2.11.0",
		"resources": map[string]any{
			"limits":   map[string]any{"nvidia.com/gpu": "8"},
			"requests": map[string]any{"nvidia.com/gpu": "8"},
		},
		"env": []any{
			map[string]any{"name": "NCCL_FASTRAK_IFNAME", "value": "eth1,eth2,eth3,eth4,eth5,eth6,eth7,eth8"},
			map[string]any{"name": "NCCL_SOCKET_IFNAME", "value": "eth0"},
			map[string]any{"name": "NCCL_FASTRAK_USE_LLCM", "value": "1"},
		},
		"volumeMounts": []any{
			map[string]any{"name": "nvtcpxo-libraries", "mountPath": "/usr/local/nvidia", "readOnly": true},
		},
	}
	tmpl := map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			gkenet.InterfacesAnnotation:             interfacesAnnotation(m),
			gkenet.DefaultInterfaceAnnotation:       "eth0",
			"devices.gke.io/container.tcpxo-daemon": "- path: /dev/nvidia0",
		}},
		"spec": map[string]any{
			"nodeSelector":   map[string]any{"cloud.google.com/gke-accelerator": "nvidia-h100-mega-80gb"},
			"initContainers": []any{map[string]any{"name": "tcpxo-daemon", "image": "tcpgpudmarxd-dev:v1.0.21"}},
			"containers":     []any{worker},
			"volumes": []any{
				map[string]any{"name": "nvtcpxo-libraries", "hostPath": map[string]any{"path": "/home/kubernetes/bin/nvidia"}},
			},
		},
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "trainer.kubeflow.org/v1alpha1",
		"kind":       "ClusterTrainingRuntime",
		"metadata":   map[string]any{"name": gkenet.TCPXORuntimeName, "labels": map[string]any{"trainer.kubeflow.org/framework": "torch"}},
		"spec": map[string]any{
			"mlPolicy": map[string]any{"numNodes": int64(2), "torch": map[string]any{}},
			"template": map[string]any{"spec": map[string]any{"replicatedJobs": []any{map[string]any{
				"name":     "node",
				"template": map[string]any{"spec": map[string]any{"template": tmpl}},
			}}}},
		},
	}}
}

func loadGKESkeleton(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	skel, err := parseYAMLTemplate(templatePath(recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, variantDefault, fabricEFA, "runtime.yaml"), nil)
	if err != nil {
		t.Fatalf("load skeleton: %v", err)
	}
	return skel
}

func TestDeriveBenchmarkRuntimeCarriesShippedWiringAndReappliesOverrides(t *testing.T) {
	skel := loadGKESkeleton(t)
	shipped := shippedTCPXORuntime(shippedMapping())

	derived, prov, err := deriveBenchmarkRuntime(skel, shipped)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	// Skeleton-owned structure survives: TrainingRuntime kind, mpi policy, launcher job.
	if derived.GetKind() != validatorv1.KindTrainingRuntime {
		t.Errorf("kind = %q, want TrainingRuntime", derived.GetKind())
	}
	if _, ok, _ := unstructured.NestedMap(derived.Object, "spec", "mlPolicy", "mpi"); !ok {
		t.Error("derived runtime lost mlPolicy.mpi")
	}
	jobs, _, _ := unstructured.NestedSlice(derived.Object, "spec", "template", "spec", "replicatedJobs")
	if len(jobs) != 2 {
		t.Fatalf("replicatedJobs = %d, want launcher + node", len(jobs))
	}
	// Shipped wiring is carried: annotations (metadata!), sidecar, env, volumes, nodeSelector.
	tmpl, err := gkenet.NodeTemplateOf(derived)
	if err != nil {
		t.Fatal(err)
	}
	ann, _, _ := unstructured.NestedStringMap(tmpl, "metadata", "annotations")
	if got := ann[gkenet.InterfacesAnnotation]; got != interfacesAnnotation(shippedMapping()) {
		t.Errorf("interfaces annotation not carried: %q", got)
	}
	if _, ok := ann["devices.gke.io/container.tcpxo-daemon"]; !ok {
		t.Error("device annotation not carried")
	}
	inits, _, _ := unstructured.NestedSlice(tmpl, "spec", "initContainers")
	if len(inits) != 1 || inits[0].(map[string]any)["name"] != "tcpxo-daemon" {
		t.Errorf("tcpxo-daemon sidecar not carried: %v", inits)
	}
	if ns, _, _ := unstructured.NestedStringMap(tmpl, "spec", "nodeSelector"); ns["cloud.google.com/gke-accelerator"] == "" {
		t.Error("nodeSelector not carried")
	}
	worker := workerContainer(tmpl)
	envs, _ := worker["env"].([]any)
	if len(envs) != 3 {
		t.Errorf("shipped NCCL env not carried: %d entries", len(envs))
	}
	// Benchmark-owned overrides applied from the skeleton.
	if img, _ := worker["image"].(string); !strings.Contains(img, "nvcr.io/nvidia/pytorch") {
		t.Errorf("worker image not overridden to the benchmark image: %q", img)
	}
	if _, ok := worker["command"]; !ok {
		t.Error("worker command (sshd bootstrap) not applied")
	}
	// Volumes: shipped kept, skeleton's additional ones merged additively.
	vols, _, _ := unstructured.NestedSlice(tmpl, "spec", "volumes")
	names := map[string]bool{}
	for _, v := range vols {
		names[v.(map[string]any)["name"].(string)] = true
	}
	if !names["nvtcpxo-libraries"] || !names["dshm"] {
		t.Errorf("volumes not additively merged: %v", names)
	}
	// Provenance: two distinct sha256 identities, and every diff path is benchmark-owned or additive.
	if len(prov.shippedDigest) != 64 || len(prov.derivedDigest) != 64 || prov.shippedDigest == prov.derivedDigest {
		t.Errorf("bad digests: %q %q", prov.shippedDigest, prov.derivedDigest)
	}
	for _, p := range prov.overridePaths {
		if !overlapsAny(p, benchmarkOwnedNodePaths) && !isAdditiveMergePath(p) {
			t.Errorf("diff path %q is outside the owned list", p)
		}
	}
}

// TestDeriveBenchmarkRuntimeTracksShippedWiring is the acceptance test #2297
// names outright: removing fabric wiring from the shipped runtime must change
// what the benchmark runs. Dropping the interfaces annotation changes the
// derived identity; dropping the fabric env is caught by the baseline
// precondition before anything is derived at all.
func TestDeriveBenchmarkRuntimeTracksShippedWiring(t *testing.T) {
	skel := loadGKESkeleton(t)
	base := shippedTCPXORuntime(shippedMapping())
	_, provBase, err := deriveBenchmarkRuntime(skel, base)
	if err != nil {
		t.Fatal(err)
	}

	unwired := shippedTCPXORuntime(shippedMapping())
	tmpl, _ := gkenet.NodeTemplateOf(unwired)
	ann, _, _ := unstructured.NestedMap(tmpl, "metadata", "annotations")
	delete(ann, gkenet.InterfacesAnnotation)
	_ = unstructured.SetNestedMap(tmpl, ann, "metadata", "annotations")
	if setErr := setNodeTemplate(unwired, tmpl); setErr != nil {
		t.Fatal(setErr)
	}
	_, provUnwired, err := deriveBenchmarkRuntime(skel, unwired)
	if err != nil {
		t.Fatalf("annotation removal must still derive (the deployed-vs-recipe check catches it): %v", err)
	}
	if provUnwired.derivedDigest == provBase.derivedDigest {
		t.Fatal("removing the interfaces annotation did not change the derived runtime — the benchmark would measure something other than what ships")
	}

	noEnv := shippedTCPXORuntime(shippedMapping())
	tmpl, _ = gkenet.NodeTemplateOf(noEnv)
	workerContainer(tmpl)["env"] = []any{}
	if err := setNodeTemplate(noEnv, tmpl); err != nil {
		t.Fatal(err)
	}
	if _, _, err := deriveBenchmarkRuntime(skel, noEnv); err == nil ||
		!strings.Contains(err.Error(), "NCCL_FASTRAK_IFNAME") {

		t.Fatalf("missing fabric env must fail the baseline precondition, got %v", err)
	}
}

func TestDeriveBenchmarkRuntimeRefusesShippedWorkerEntrypoint(t *testing.T) {
	skel := loadGKESkeleton(t)
	shipped := shippedTCPXORuntime(shippedMapping())
	tmpl, _ := gkenet.NodeTemplateOf(shipped)
	workerContainer(tmpl)["command"] = []any{"/bin/sh", "-c", ". /usr/local/nvidia/lib64/nccl-env-profile.sh"}
	if err := setNodeTemplate(shipped, tmpl); err != nil {
		t.Fatal(err)
	}
	_, _, err := deriveBenchmarkRuntime(skel, shipped)
	if err == nil || !strings.Contains(err.Error(), "worker sets command") {
		t.Fatalf("a shipped entrypoint hides fabric activation under an overridden path and must fail loudly, got %v", err)
	}
}

func TestDiffTemplatePathsAndOverlap(t *testing.T) {
	a := map[string]any{"spec": map[string]any{
		"containers": []any{
			map[string]any{"name": "node", "image": "x", "env": []any{map[string]any{"name": "A", "value": "1"}}},
			map[string]any{"name": "side", "image": "y"},
		},
		"hostNetwork": false,
	}}
	b := serializer.DeepCopyAnyMap(a)
	// Reorder containers (no diff), change node image (owned), add hostNetwork change (not owned).
	bc := b["spec"].(map[string]any)["containers"].([]any)
	bc[0], bc[1] = bc[1], bc[0]
	workerContainer(b)["image"] = "z"
	b["spec"].(map[string]any)["hostNetwork"] = true

	got := diffTemplatePaths(a, b)
	want := []string{"spec.containers[node].image", "spec.hostNetwork"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("diff = %v, want %v", got, want)
	}
	if !overlapsAny("spec.containers[node].image", benchmarkOwnedNodePaths) {
		t.Error("owned path must overlap itself")
	}
	if !overlapsAny("spec.containers[node].resources.limits", benchmarkOwnedNodePaths) {
		t.Error("a path contained by an owned path must overlap")
	}
	if !overlapsAny("spec.containers[node]", benchmarkOwnedNodePaths) {
		t.Error("a path containing an owned path must overlap")
	}
	if overlapsAny("spec.hostNetwork", benchmarkOwnedNodePaths) {
		t.Error("unrelated path must not overlap")
	}
}

func tcpxoRefs(m []recipe.NetworkInterfaceMapping) []recipe.ComponentRef {
	raw := make([]any, 0, len(m))
	for _, e := range m {
		raw = append(raw, map[string]any{"interfaceName": e.InterfaceName, "network": e.Network})
	}
	return []recipe.ComponentRef{{Name: recipe.KubeflowTrainerComponentName,
		Overrides: map[string]any{recipe.GKETCPXOInterfacesOverrideKey: raw}}}
}

func gkeNetwork(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.gke.io/v1", "kind": "Network", "metadata": map[string]any{"name": name}}}
}

func fakeDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gkenet.ClusterTrainingRuntimeGVR: "ClusterTrainingRuntimeList",
		gkenet.NetworkGVR:                "NetworkList",
	}, objs...)
}

func TestResolveBenchmarkRuntimeSource(t *testing.T) {
	m := shippedMapping()
	nets := make([]runtime.Object, 0, 8)
	for _, e := range m {
		nets = append(nets, gkeNetwork(e.Network))
	}
	newCtx := func(refs []recipe.ComponentRef, objs ...runtime.Object) *validators.Context {
		return &validators.Context{
			Ctx:           t.Context(),
			DynamicClient: fakeDyn(objs...),
			ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{
				Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100},
				ComponentRefs: refs,
			}),
		}
	}
	resolve := func(ctx *validators.Context, carrier string) (*benchmarkRuntimePlan, error) {
		return resolveBenchmarkRuntimeSource(ctx, carrier, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, variantDefault, fabricEFA)
	}

	t.Run("no delivered runtime -> cluster-capability, empty carrier", func(t *testing.T) {
		plan, err := resolve(newCtx(nil), "")
		if err != nil || plan.source != runtimeSourceCapability || plan.carrier != "" {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
	})
	t.Run("recipe-supplied runtime wins when nothing is delivered", func(t *testing.T) {
		plan, err := resolve(newCtx(nil), validBenchmarkRuntime)
		if err != nil || plan.source != runtimeSourceRecipeSupplied || plan.carrier != validBenchmarkRuntime {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
	})
	t.Run("supplied runtime + delivered runtime is rejected", func(t *testing.T) {
		_, err := resolve(newCtx(tcpxoRefs(m)), validBenchmarkRuntime)
		if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) || !strings.Contains(err.Error(), "two owners") {
			t.Fatalf("want ErrCodeInvalidRequest exclusivity, got %v", err)
		}
	})
	t.Run("delivered but not deployed -> NotFound, no fixture fallback", func(t *testing.T) {
		_, err := resolve(newCtx(tcpxoRefs(m), nets...), "")
		if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
			t.Fatalf("want ErrCodeNotFound, got %v", err)
		}
	})
	t.Run("deployed mapping diverging from recipe fails", func(t *testing.T) {
		drift := append([]recipe.NetworkInterfaceMapping(nil), m...)
		drift[3].Network = "c1-gpu-nic-9"
		objs := append([]runtime.Object{shippedTCPXORuntime(drift)}, nets...)
		_, err := resolve(newCtx(tcpxoRefs(m), objs...), "")
		if err == nil || !strings.Contains(err.Error(), "diverges from the recipe") {
			t.Fatalf("want recipe-vs-deployed failure, got %v", err)
		}
	})
	t.Run("deployed network missing on cluster fails", func(t *testing.T) {
		objs := append([]runtime.Object{shippedTCPXORuntime(m)}, nets[:7]...)
		_, err := resolve(newCtx(tcpxoRefs(m), objs...), "")
		if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) || !strings.Contains(err.Error(), "do not exist on this cluster") {
			t.Fatalf("want deployed-vs-cluster NotFound, got %v", err)
		}
	})
	t.Run("delivered, deployed, consistent -> derived carrier", func(t *testing.T) {
		objs := append([]runtime.Object{shippedTCPXORuntime(m)}, nets...)
		plan, err := resolve(newCtx(tcpxoRefs(m), objs...), "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.source != runtimeSourceDelivered || plan.carrier == "" || plan.provenance == nil {
			t.Fatalf("plan=%+v", plan)
		}
		if err := validatorv1.ValidateBenchmarkRuntime(plan.carrier); err != nil {
			t.Errorf("derived carrier must pass the shape gate: %v", err)
		}
		if !strings.Contains(plan.carrier, interfacesAnnotation(m)) {
			t.Error("derived carrier does not carry the shipped interfaces annotation")
		}
		if !plan.source.runsGKETCPXOChecks() || runtimeSourceRecipeSupplied.runsGKETCPXOChecks() {
			t.Error("preflight/watcher gating: derived must run them, recipe-supplied must not")
		}
	})
}

func TestRuntimeProvenanceExtra(t *testing.T) {
	got := runtimeProvenanceExtra(&benchmarkRuntimePlan{source: runtimeSourceCapability})
	if got[extraKeyRuntimeSource] != "cluster-capability" || len(got) != 1 {
		t.Fatalf("capability extra = %v", got)
	}
	got = runtimeProvenanceExtra(&benchmarkRuntimePlan{
		source: runtimeSourceDelivered,
		provenance: &derivedRuntimeProvenance{
			shippedDigest: strings.Repeat("a", 64), derivedDigest: strings.Repeat("b", 64)},
	})
	if got[extraKeyRuntimeSource] != "delivered-artifact" ||
		got[extraKeyShippedRuntimeDigest] != strings.Repeat("a", 64) ||
		got[extraKeyDerivedRuntimeDigest] != strings.Repeat("b", 64) {

		t.Fatalf("delivered extra = %v", got)
	}
}
