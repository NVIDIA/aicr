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
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func gkeNetworkObjects(count int) []runtime.Object {
	objs := make([]runtime.Object, 0, count)
	for i := range count {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "networking.gke.io/v1",
			"kind":       "Network",
			"metadata":   map[string]any{"name": fmt.Sprintf("aicr-test-gpu-nic-%d", i)},
		}})
	}
	return objs
}

func gkeNetworkClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gkenet.NetworkGVR: "NetworkList"},
		objects...)
}

// notFoundClient models a cluster that does not serve networks.networking.gke.io
// — i.e. one created without --enable-multi-networking.
func notFoundClient() *dynamicfake.FakeDynamicClient {
	client := gkeNetworkClient()
	client.PrependReactor("list", "networks", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "networking.gke.io", Resource: "networks"}, "")
	})
	return client
}

func tcpxoContext(client *dynamicfake.FakeDynamicClient, declared bool) *validators.Context {
	ctx := &validators.Context{Ctx: context.Background(), DynamicClient: client}
	if declared {
		ctx.ValidationInput = &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: tcpxoComponent}},
		}
	}
	return ctx
}

// TestCheckGKEGPUNICNetworks covers the count boundary on a recipe that
// declares gke-nccl-tcpxo. The zero case is the #2216 regression: the
// component's DaemonSets roll out cleanly on such a cluster, so before this
// check the deployment phase passed and the failure only appeared much later.
func TestCheckGKEGPUNICNetworks(t *testing.T) {
	tests := []struct {
		name      string
		count     int
		wantErr   bool
		wantInMsg string
	}{
		{name: "no networks — the #2216 gap", count: 0, wantErr: true, wantInMsg: "0 of 8"},
		{name: "partial provisioning", count: 7, wantErr: true, wantInMsg: "7 of 8"},
		{name: "exactly the required count", count: gkenet.RequiredGPUNICNetworks, wantErr: false},
		{name: "more than required", count: 9, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tcpxoContext(gkeNetworkClient(gkeNetworkObjects(tt.count)...), true)
			err := checkGKEGPUNICNetworks(ctx)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("expected pass, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected failure, got nil — a cluster without GPU NIC networks must not pass")
			}
			if validators.IsSkip(err) {
				t.Fatalf("expected a failure, got a Skip: %v", err)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
				t.Errorf("expected ErrCodeNotFound, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("message %q does not report the shortfall %q", err.Error(), tt.wantInMsg)
			}
			// The message must name the prerequisite so it is actionable.
			for _, want := range []string{"GKENetworkParamSet", "kubectl get network.networking.gke.io", "gpu-nic"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

// TestCheckGKEGPUNICNetworksApplicability asserts the #2122 contract: skip only
// when the recipe does not declare the component, and never turn an infra
// error into a skip.
func TestCheckGKEGPUNICNetworksApplicability(t *testing.T) {
	t.Run("undeclared recipe skips", func(t *testing.T) {
		err := checkGKEGPUNICNetworks(tcpxoContext(gkeNetworkClient(), false))
		if !validators.IsSkip(err) {
			t.Fatalf("expected Skip on a recipe that does not declare %s, got: %v", tcpxoComponent, err)
		}
	})

	t.Run("undeclared recipe with partial networks still skips", func(t *testing.T) {
		// A non-TCPXO recipe must not be failed by whatever networking its
		// cluster happens to have.
		err := checkGKEGPUNICNetworks(tcpxoContext(gkeNetworkClient(gkeNetworkObjects(3)...), false))
		if !validators.IsSkip(err) {
			t.Fatalf("expected Skip, got: %v", err)
		}
	})

	t.Run("declared recipe with zero networks fails rather than skips", func(t *testing.T) {
		err := checkGKEGPUNICNetworks(tcpxoContext(gkeNetworkClient(), true))
		if err == nil || validators.IsSkip(err) {
			t.Fatalf("expected a failure, got: %v", err)
		}
	})

	// An RBAC denial must block on BOTH declaration states — it is not evidence
	// that TCPXO is inapplicable.
	for _, declared := range []bool{true, false} {
		t.Run(fmt.Sprintf("forbidden blocks (declared=%v)", declared), func(t *testing.T) {
			client := gkeNetworkClient()
			client.PrependReactor("list", "networks", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(
					schema.GroupResource{Group: "networking.gke.io", Resource: "networks"}, "", nil)
			})

			err := checkGKEGPUNICNetworks(tcpxoContext(client, declared))
			if err == nil {
				t.Fatal("expected an error on a forbidden list")
			}
			if validators.IsSkip(err) {
				t.Fatalf("an RBAC denial must not skip: %v", err)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeUnauthorized, "")) {
				t.Errorf("expected ErrCodeUnauthorized, got: %v", err)
			}
		})
	}

	// A timeout or a transport failure must block for the same reason: neither
	// is evidence that TCPXO is inapplicable.
	infraErrs := []struct {
		name     string
		err      error
		wantCode errors.ErrorCode
	}{
		{
			name:     "timeout",
			err:      apierrors.NewTimeoutError("list timed out", 1),
			wantCode: errors.ErrCodeTimeout,
		},
		{
			name:     "transport failure",
			err:      apierrors.NewServiceUnavailable("apiserver unavailable"),
			wantCode: errors.ErrCodeUnavailable,
		},
	}
	for _, ie := range infraErrs {
		for _, declared := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s blocks (declared=%v)", ie.name, declared), func(t *testing.T) {
				client := gkeNetworkClient()
				client.PrependReactor("list", "networks", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, ie.err
				})

				err := checkGKEGPUNICNetworks(tcpxoContext(client, declared))
				if err == nil {
					t.Fatalf("expected an error on a %s", ie.name)
				}
				if validators.IsSkip(err) {
					t.Fatalf("a %s must not skip: %v", ie.name, err)
				}
				if !stderrors.Is(err, errors.New(ie.wantCode, "")) {
					t.Errorf("expected %s, got: %v", ie.wantCode, err)
				}
			})
		}
	}

	// An ABSENT Network API is clean absence, not an infra failure: the CRD
	// arrives with --enable-multi-networking, so a cluster created without it
	// does not serve this GVR at all. Undeclared must skip; declared must still
	// get the actionable prerequisite message rather than a generic read error.
	t.Run("absent Network API skips when undeclared", func(t *testing.T) {
		err := checkGKEGPUNICNetworks(tcpxoContext(notFoundClient(), false))
		if !validators.IsSkip(err) {
			t.Fatalf("expected Skip when the API is absent and %s is undeclared, got: %v",
				tcpxoComponent, err)
		}
	})

	t.Run("absent Network API fails with the prerequisite when declared", func(t *testing.T) {
		err := checkGKEGPUNICNetworks(tcpxoContext(notFoundClient(), true))
		if err == nil || validators.IsSkip(err) {
			t.Fatalf("expected a failure, got: %v", err)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
			t.Errorf("expected ErrCodeNotFound, got: %v", err)
		}
		// Must name the prerequisite, not just "failed to read".
		for _, want := range []string{"--enable-multi-networking", "GKENetworkParamSet", "gpu-nic"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message %q does not name %q", err.Error(), want)
			}
		}
	})

	t.Run("missing dynamic client is rejected", func(t *testing.T) {
		err := checkGKEGPUNICNetworks(&validators.Context{Ctx: context.Background()})
		if err == nil || validators.IsSkip(err) {
			t.Fatalf("expected a failure without a dynamic client, got: %v", err)
		}
	})
}

// --- #2297: delivered-runtime arms -------------------------------------------

func tcpxoMapping(prefix string) []recipe.NetworkInterfaceMapping {
	out := make([]recipe.NetworkInterfaceMapping, 0, 8)
	for i := range 8 {
		out = append(out, recipe.NetworkInterfaceMapping{
			InterfaceName: fmt.Sprintf("eth%d", i+1), Network: fmt.Sprintf("%s-gpu-nic-%d", prefix, i)})
	}
	return out
}

func tcpxoNetworkObjects(m []recipe.NetworkInterfaceMapping) []runtime.Object {
	objs := make([]runtime.Object, 0, len(m))
	for _, e := range m {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "networking.gke.io/v1", "kind": "Network",
			"metadata": map[string]any{"name": e.Network}}})
	}
	return objs
}

func tcpxoRuntimeObject(m []recipe.NetworkInterfaceMapping) *unstructured.Unstructured {
	ann := `[{"interfaceName":"eth0","network":"default"}`
	for _, e := range m {
		ann += fmt.Sprintf(`,{"interfaceName":"%s","network":"%s"}`, e.InterfaceName, e.Network)
	}
	ann += "]"
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "trainer.kubeflow.org/v1alpha1", "kind": "ClusterTrainingRuntime",
		"metadata": map[string]any{"name": gkenet.TCPXORuntimeName},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"replicatedJobs": []any{map[string]any{
			"name": gkenet.TCPXONodeJob,
			"template": map[string]any{"spec": map[string]any{"template": map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{
					gkenet.InterfacesAnnotation: ann, gkenet.DefaultInterfaceAnnotation: "eth0"}},
				"spec": map[string]any{"containers": []any{map[string]any{"name": "node"}}},
			}}},
		}}}}},
	}}
}

func deliveredClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			gkenet.NetworkGVR:                "NetworkList",
			gkenet.ClusterTrainingRuntimeGVR: "ClusterTrainingRuntimeList",
		}, objects...)
}

// deliveredContext declares gke-nccl-tcpxo AND a kubeflow-trainer ref carrying
// the recorded mapping — the shape h100-gke-cos-training-kubeflow produces.
func deliveredContext(client *dynamicfake.FakeDynamicClient, recorded []recipe.NetworkInterfaceMapping) *validators.Context {
	raw := make([]any, 0, len(recorded))
	for _, e := range recorded {
		raw = append(raw, map[string]any{"interfaceName": e.InterfaceName, "network": e.Network})
	}
	return &validators.Context{Ctx: context.Background(), DynamicClient: client,
		ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{
			{Name: tcpxoComponent},
			{Name: recipe.KubeflowTrainerComponentName, ManifestFiles: []string{recipe.GKETCPXORuntimeManifest},
				Overrides: map[string]any{recipe.GKETCPXOInterfacesOverrideKey: raw}},
		}}}
}

func TestCheckGKEGPUNICNetworksDeliveredRuntimeArms(t *testing.T) {
	recorded := tcpxoMapping("c1")
	nets := tcpxoNetworkObjects(recorded)
	drift := append([]recipe.NetworkInterfaceMapping(nil), recorded...)
	drift[2].Network = "c1-gpu-nic-9"

	tests := []struct {
		name     string
		ctx      *validators.Context
		wantErr  error  // sentinel code, nil for pass
		wantText string // substring of the error
	}{
		{
			// Base h100-gke-cos-training: TCPXO declared, no runtime/mapping. The
			// census must still be the whole check — this is the false-fail the
			// predicate gate exists to prevent.
			name: "tcpxo without a delivered runtime keeps census-only behaviour",
			ctx:  tcpxoContext(gkeNetworkClient(nets...), true),
		},
		{
			name: "delivered runtime consistent with recipe and cluster passes",
			ctx:  deliveredContext(deliveredClient(append([]runtime.Object{tcpxoRuntimeObject(recorded)}, nets...)...), recorded),
		},
		{
			name:     "deployed mapping diverging from the recipe fails",
			ctx:      deliveredContext(deliveredClient(append([]runtime.Object{tcpxoRuntimeObject(drift)}, nets...)...), recorded),
			wantErr:  errors.New(errors.ErrCodeInvalidRequest, ""),
			wantText: "diverges from the recipe",
		},
		{
			name:     "recipe ships the runtime but it is not deployed",
			ctx:      deliveredContext(deliveredClient(nets...), recorded),
			wantErr:  errors.New(errors.ErrCodeNotFound, ""),
			wantText: "not deployed",
		},
		{
			// Recipe and deployed agree with each other but name a network this
			// cluster does not have: the census counts 8 (a stray extra network
			// stands in for the missing one), so only the set comparison catches it.
			name: "deployed mapping selects a network the cluster lacks",
			ctx: deliveredContext(deliveredClient(append(
				[]runtime.Object{tcpxoRuntimeObject(recorded), tcpxoNetworkObjects(tcpxoMapping("other"))[0]},
				nets[1:]...)...), recorded),
			wantErr:  errors.New(errors.ErrCodeNotFound, ""),
			wantText: "do not exist on this cluster",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkGKEGPUNICNetworks(tt.ctx)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !stderrors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want code of %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tt.wantText)
			}
		})
	}
}
