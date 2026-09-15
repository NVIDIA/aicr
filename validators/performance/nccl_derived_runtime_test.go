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
	"encoding/json"
	stderrors "errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
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

// TestDeriveBenchmarkRuntimeBaselineCoversEveryOverriddenPath pins the
// acceptance criterion that every overridden source path has an explicit
// precondition: a shipped change under resources or terminationMessagePolicy
// must fail rather than vanish under the override, while the shipped shape as
// it is today passes.
func TestDeriveBenchmarkRuntimeBaselineCoversEveryOverriddenPath(t *testing.T) {
	skel := loadGKESkeleton(t)
	mutate := func(fn func(worker map[string]any)) *unstructured.Unstructured {
		shipped := shippedTCPXORuntime(shippedMapping())
		tmpl, _ := gkenet.NodeTemplateOf(shipped)
		fn(workerContainer(tmpl))
		if err := setNodeTemplate(shipped, tmpl); err != nil {
			t.Fatal(err)
		}
		return shipped
	}
	tests := []struct {
		name    string
		shipped *unstructured.Unstructured
		wantErr string
	}{
		{"shipped shape today passes", mutate(func(map[string]any) {}), ""},
		{"extra resource under limits", mutate(func(w map[string]any) {
			w["resources"].(map[string]any)["limits"].(map[string]any)["vpc.amazonaws.com/efa"] = "8"
		}), `carry "vpc.amazonaws.com/efa"`},
		{"unknown resources section", mutate(func(w map[string]any) {
			w["resources"].(map[string]any)["claims"] = []any{map[string]any{"name": "gpu"}}
		}), `carry "claims"`},
		{"terminationMessagePolicy set", mutate(func(w map[string]any) {
			w["terminationMessagePolicy"] = "FallbackToLogsOnError"
		}), "sets terminationMessagePolicy"},
		{"GPU request removed from requests", mutate(func(w map[string]any) {
			delete(w["resources"].(map[string]any)["requests"].(map[string]any), "nvidia.com/gpu")
		}), "resources.requests do not request nvidia.com/gpu"},
		{"resources block removed entirely", mutate(func(w map[string]any) {
			delete(w, "resources")
		}), "do not request nvidia.com/gpu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := deriveBenchmarkRuntime(skel, tt.shipped)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("want InvalidRequest containing %q, got %v", tt.wantErr, err)
			}
		})
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
		ManifestFiles: []string{recipe.GKETCPXORuntimeManifest},
		Overrides:     map[string]any{recipe.GKETCPXOInterfacesOverrideKey: raw}}}
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
		return resolveBenchmarkRuntimeSource(ctx, carrier, false, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, variantDefault, fabricEFA)
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
	t.Run("benchmark profile + delivered runtime is rejected", func(t *testing.T) {
		_, err := resolveBenchmarkRuntimeSource(newCtx(tcpxoRefs(m)), "", true,
			recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, variantDefault, fabricEFA)
		if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) || !strings.Contains(err.Error(), perfConstraintNCCLBenchmarkProfile) {
			t.Fatalf("want ErrCodeInvalidRequest profile exclusivity, got %v", err)
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
	t.Run("delivered derivation ignores the validator's own fabric env", func(t *testing.T) {
		// AICR_NCCL_FABRIC describes the embedded fixture's fabric; a delivered
		// runtime carries its own wiring, so a RoCE override must not redirect
		// the skeleton lookup to a template tree that does not exist for GKE.
		objs := append([]runtime.Object{shippedTCPXORuntime(m)}, nets...)
		plan, err := resolveBenchmarkRuntimeSource(newCtx(tcpxoRefs(m), objs...), "", false,
			recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, variantDefault, fabricRoCE)
		if err != nil || plan.source != runtimeSourceDelivered {
			t.Fatalf("fabric env must not affect a delivered derivation: plan=%+v err=%v", plan, err)
		}
	})
	t.Run("delivered, deployed, consistent -> derived carrier", func(t *testing.T) {
		objs := append([]runtime.Object{shippedTCPXORuntime(m)}, nets...)
		plan, err := resolve(newCtx(tcpxoRefs(m), objs...), "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.source != runtimeSourceDelivered || plan.carrier == "" || !plan.derived() {
			t.Fatalf("plan=%+v", plan)
		}
		if plan.provenance != nil {
			t.Error("resolution must not record provenance: nothing has been applied yet")
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
	// Only the closed-set class is published; digests and paths are stdout-only
	// (--full) evidence and must never ride the Extra carrier.
	for _, src := range []ncclRuntimeSource{runtimeSourceCapability, runtimeSourceRecipeSupplied, runtimeSourceDelivered} {
		got := runtimeProvenanceExtra(&benchmarkRuntimePlan{source: src,
			provenance: &derivedRuntimeProvenance{shippedDigest: strings.Repeat("a", 64), derivedDigest: strings.Repeat("b", 64)}})
		if len(got) != 1 || got[extraKeyRuntimeSource] != string(src) {
			t.Fatalf("extra for %s = %v, want only runtimeSource", src, got)
		}
	}
}

// deliveredPlan resolves a consistent delivered plan against a fake cluster.
func deliveredPlan(t *testing.T) *benchmarkRuntimePlan {
	t.Helper()
	m := shippedMapping()
	objs := make([]runtime.Object, 0, 1+len(m))
	objs = append(objs, shippedTCPXORuntime(m))
	for _, e := range m {
		objs = append(objs, gkeNetwork(e.Network))
	}
	ctx := &validators.Context{Ctx: t.Context(),
		DynamicClient: fakeDyn(objs...),
		ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{
			Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100},
			ComponentRefs: tcpxoRefs(m)})}
	plan, err := resolveBenchmarkRuntimeSource(ctx, "", false, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, variantDefault, fabricEFA)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

var derivedTemplateData = map[string]string{"NAMESPACE": "ns", "WORKER_COUNT": "2", "GPU_COUNT": "16", "GPU_COUNT_PER_NODE": "8",
	"TEST_TYPE": "all_reduce", "MIN_MESSAGE_SIZE": "1K", "MAX_MESSAGE_SIZE": "16G"}

// containerArgs returns the args of a job's "node" container as strings,
// failing on any non-string element (Kubeflow Trainer's structural CRD rejects
// those). Both skeleton jobs name their container "node".
func containerArgs(t *testing.T, obj *unstructured.Unstructured, job string) []string {
	const container = benchmarkWorkerContainer
	t.Helper()
	jobs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	for _, raw := range jobs {
		j := raw.(map[string]any)
		if j["name"] != job {
			continue
		}
		cs, _, _ := unstructured.NestedSlice(j, "template", "spec", "template", "spec", "containers")
		for _, c := range cs {
			cm := c.(map[string]any)
			if cm["name"] != container {
				continue
			}
			raw, _ := cm["args"].([]any)
			out := make([]string, 0, len(raw))
			for i, a := range raw {
				str, ok := a.(string)
				if !ok {
					t.Fatalf("%s/%s args[%d] = %T (%v), want string", job, container, i, a, a)
				}
				out = append(out, str)
			}
			return out
		}
	}
	t.Fatalf("no %s/%s container", job, container)
	return nil
}

// TestDerivedRuntimeBuiltAtApplyTimeKeepsTypes is the regression the review
// found: deriving from a serialized carrier with placeholders inside turned a
// quoted "${GPU_COUNT}" into an integer after substitution. The applied object
// is now derived from the skeleton rendered with the real templateData, so
// every args element is a string while numProcPerNode stays a number.
func TestDerivedRuntimeBuiltAtApplyTimeKeepsTypes(t *testing.T) {
	plan := deliveredPlan(t)
	obj, err := buildNCCLRuntimeObject(plan.carrier, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE,
		variantDefault, fabricRoCE, "aicr-validation", derivedTemplateData, plan)
	if err != nil {
		t.Fatal(err)
	}
	if obj.GetName() != ncclTrainingRuntimeName || obj.GetNamespace() != "aicr-validation" {
		t.Errorf("identity = %s/%s", obj.GetNamespace(), obj.GetName())
	}
	launcher := containerArgs(t, obj, benchmarkLauncherJob)
	if !slices.Contains(launcher, "16") || !slices.Contains(launcher, "/usr/local/bin/all_reduce_mpi") {
		t.Errorf("launcher args not rendered: %v", launcher)
	}
	if v, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "mlPolicy", "mpi", "numProcPerNode"); v != int64(8) {
		t.Errorf("numProcPerNode = %T %v, want int64 8", v, v)
	}
	tmpl, err := gkenet.NodeTemplateOf(obj)
	if err != nil {
		t.Fatal(err)
	}
	ann, _, _ := unstructured.NestedString(tmpl, "metadata", "annotations", gkenet.InterfacesAnnotation)
	if ann != interfacesAnnotation(shippedMapping()) {
		t.Errorf("shipped interfaces annotation not carried: %q", ann)
	}
}

// TestDerivedRuntimeMeasuresShippedEnv pins the second review finding: the
// fixture's worker bootstrap re-sourced the host nccl-env-profile.sh over the
// shipped env and the launcher pushed fixture NCCL/CUDA tuning via mpirun -x,
// so the number would have described the fixture's env under the shipped
// container. A derived runtime exports only what the shipped worker declares
// and keeps just the benchmark-owned exports on the launcher.
func TestDerivedRuntimeMeasuresShippedEnv(t *testing.T) {
	plan := deliveredPlan(t)
	obj, err := buildNCCLRuntimeObject(plan.carrier, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE,
		variantDefault, fabricEFA, "ns", derivedTemplateData, plan)
	if err != nil {
		t.Fatal(err)
	}
	worker := containerArgs(t, obj, gkenet.TCPXONodeJob)
	if len(worker) != 1 || strings.Contains(worker[0], "nccl-env-profile.sh") || !strings.Contains(worker[0], "^LD_LIBRARY_PATH=") {
		t.Errorf("worker bootstrap must export the shipped env, not source the host profile: %q", worker)
	}
	launcher := containerArgs(t, obj, benchmarkLauncherJob)
	for i, a := range launcher {
		if a != "-x" {
			continue
		}
		v := launcher[i+1]
		switch {
		case strings.HasPrefix(v, "NCCL_DEBUG="), strings.HasPrefix(v, "UCX_"):
		default:
			t.Errorf("launcher still exports fixture tuning: -x %s", v)
		}
	}
	if !slices.Contains(launcher, "NCCL_DEBUG=WARN") {
		t.Errorf("NCCL_DEBUG export must be kept: %v", launcher)
	}
	// The capability fixture is untouched: its launcher still carries its tuning.
	fixture, err := buildNCCLRuntimeObject("", recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE,
		variantDefault, fabricEFA, "ns", derivedTemplateData, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(containerArgs(t, fixture, benchmarkLauncherJob), "CUDA_DEVICE_MAX_CONNECTIONS=1") {
		t.Error("control: the embedded fixture must keep its own tuning exports")
	}
}

// TestFinalizeRuntimeProvenanceDescribesAppliedObject checks the audit record
// is computed against the applied runtime — scheduling included — and that the
// inherited inventory is the shipped leaves the diff did not touch.
func TestFinalizeRuntimeProvenanceDescribesAppliedObject(t *testing.T) {
	plan := deliveredPlan(t)
	obj, err := buildNCCLRuntimeObject(plan.carrier, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE,
		variantDefault, fabricEFA, "ns", derivedTemplateData, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyNCCLWorkerScheduling(obj, map[string]string{"pool": "gpu"}, nil); err != nil {
		t.Fatal(err)
	}
	prov, err := finalizeRuntimeProvenance(plan, obj)
	if err != nil {
		t.Fatal(err)
	}
	plan.provenance = prov
	if len(prov.shippedDigest) != 64 || len(prov.derivedDigest) != 64 || prov.shippedDigest == prov.derivedDigest {
		t.Errorf("digests = %q / %q", prov.shippedDigest, prov.derivedDigest)
	}
	for _, want := range []string{"spec.nodeSelector.pool", "spec.containers[node].args", "spec.containers[node].image"} {
		if !slices.Contains(prov.overridePaths, want) {
			t.Errorf("override paths missing %s: %v", want, prov.overridePaths)
		}
	}
	for _, want := range []string{"spec.containers[node].env[NCCL_FASTRAK_IFNAME].value", "metadata.annotations." + gkenet.InterfacesAnnotation} {
		if !slices.Contains(prov.inheritedPaths, want) {
			t.Errorf("inherited paths missing %s: %v", want, prov.inheritedPaths)
		}
	}
	for _, p := range prov.inheritedPaths {
		if overlapsAny(p, prov.overridePaths) {
			t.Errorf("inherited path %s overlaps an override", p)
		}
	}
	emitRuntimeProvenance(plan)                                                   // prints the record; must not panic
	emitRuntimeProvenance(&benchmarkRuntimePlan{source: runtimeSourceCapability}) // no record: no-op
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	return <-done
}

// TestRuntimeSourceEmittedBeforeDeliveredVerification pins the review finding:
// the class is recipe-determined, so a delivered run that fails its live
// verification (here: runtime not deployed) must still have published
// delivered-artifact — the failure describes a delivered measurement, not a
// fixture that never ran.
func TestRuntimeSourceEmittedBeforeDeliveredVerification(t *testing.T) {
	m := shippedMapping()
	ctx := &validators.Context{Ctx: t.Context(), DynamicClient: fakeDyn(),
		ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{ComponentRefs: tcpxoRefs(m)})}
	var err error
	out := captureStdout(t, func() {
		_, err = resolveBenchmarkRuntimeSource(ctx, "", false, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, variantDefault, fabricEFA)
	})
	if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Fatalf("control: want NotFound from the delivered verification, got %v", err)
	}
	want := ctrf.ExtraLinePrefix + `{"runtimeSource":"delivered-artifact"}`
	if !strings.Contains(out, want) {
		t.Errorf("stdout lacks the early source sentinel %q:\n%s", want, out)
	}
}

// TestRuntimeProvenanceCarrierIsEmitted checks the audit record reaches the
// bounded ctrf carrier (the evidence that survives minimal redaction), not just
// the human listing.
func TestRuntimeProvenanceCarrierIsEmitted(t *testing.T) {
	prov := &derivedRuntimeProvenance{shippedDigest: strings.Repeat("a", 64), derivedDigest: strings.Repeat("b", 64),
		overridePaths: []string{"spec.containers[node].args"}, inheritedPaths: []string{"spec.hostNetwork"}}
	out := captureStdout(t, func() { emitRuntimeProvenance(&benchmarkRuntimePlan{source: runtimeSourceDelivered, provenance: prov}) })
	var line string
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, ctrf.ProvenanceLinePrefix); ok {
			line = p
		}
	}
	if line == "" {
		t.Fatalf("no provenance sentinel in:\n%s", out)
	}
	var got ctrf.RuntimeProvenance
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatal(err)
	}
	want := *runtimeProvenanceCarrier(prov)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("carrier = %+v, want %+v", got, want)
	}
}

// TestProvenanceRecordedOnlyAfterRuntimeApplied pins the review finding: a run
// that fails before or at the TrainingRuntime create must publish no record
// claiming an object was applied; a successful create records it.
func TestProvenanceRecordedOnlyAfterRuntimeApplied(t *testing.T) {
	const ns = "aicr-validation"
	config := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8, TotalGPUCount: 16, Namespace: ns}
	apply := func(t *testing.T, client *dynamicfake.FakeDynamicClient) (*benchmarkRuntimePlan, error) {
		t.Helper()
		plan := deliveredPlan(t)
		ctx := &validators.Context{Ctx: t.Context(), DynamicClient: client, Namespace: ns}
		err := applyNCCLResources(ctx, client, config, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE,
			variantDefault, fabricEFA, plan.carrier, plan)
		return plan, err
	}
	t.Run("create rejected -> no provenance", func(t *testing.T) {
		client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), ncclGVRListKinds)
		client.PrependReactor("create", "trainingruntimes", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("admission webhook denied")
		})
		plan, err := apply(t, client)
		if err == nil {
			t.Fatal("control: the rejected create must fail apply")
		}
		if plan.provenance != nil {
			t.Errorf("provenance recorded for a runtime that was never applied: %+v", plan.provenance)
		}
	})
	t.Run("create succeeds -> provenance recorded", func(t *testing.T) {
		plan, err := apply(t, dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), ncclGVRListKinds))
		if err != nil {
			t.Fatal(err)
		}
		if plan.provenance == nil || len(plan.provenance.inheritedPaths) == 0 {
			t.Fatalf("provenance must describe the applied object: %+v", plan.provenance)
		}
	})
}

// TestDerivedRuntimeRejectsShippedGPUCountMismatch completes the resources
// baseline: a deployed runtime whose GPU request differs from the target
// nodes' per-node count is not silently repaired by the skeleton's request.
func TestDerivedRuntimeRejectsShippedGPUCountMismatch(t *testing.T) {
	plan := deliveredPlan(t)
	data := map[string]string{}
	for k, v := range derivedTemplateData {
		data[k] = v
	}
	data["GPU_COUNT_PER_NODE"] = "4" // fixture ships nvidia.com/gpu: "8"
	_, err := buildNCCLRuntimeObject(plan.carrier, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE,
		variantDefault, fabricEFA, "ns", data, plan)
	if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) || !strings.Contains(err.Error(), "carry 4 GPUs") {
		t.Fatalf("want InvalidRequest GPU-count mismatch, got %v", err)
	}
	if _, err := buildNCCLRuntimeObject(plan.carrier, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE,
		variantDefault, fabricEFA, "ns", derivedTemplateData, plan); err != nil {
		t.Fatalf("control: matching count must derive: %v", err)
	}
}
