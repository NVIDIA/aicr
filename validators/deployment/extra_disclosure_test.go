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
	"encoding/json"
	"io"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// captureExtras returns the merged Extra that fn writes to stdout, and fn's
// error. Callers must not be parallel, because it swaps the global os.Stdout.
func captureExtras(t *testing.T, fn func() error) (map[string]string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		out, _ := io.ReadAll(r)
		done <- out
	}()
	runErr := fn()
	_ = w.Close()
	os.Stdout = orig
	out := <-done

	extra := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		payload, ok := strings.CutPrefix(line, ctrf.ExtraLinePrefix)
		if !ok {
			continue
		}
		var parsed map[string]string
		if jerr := json.Unmarshal([]byte(payload), &parsed); jerr != nil {
			t.Fatalf("sentinel payload %q: %v", payload, jerr)
		}
		maps.Copy(extra, parsed)
	}
	return extra, runErr
}

func assertExtraSubset(t *testing.T, got, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("extra[%s] = %q, want %q (extra = %v)", k, got[k], v, got)
		}
	}
}

func TestCheckExpectedResourcesDisclosesReadinessCounts(t *testing.T) {
	nodewrightRef := recipe.ComponentRef{
		Name: nodewrightCustomizationsComponent, Namespace: "skyhook", ManifestFiles: []string{testNodewrightManifest},
	}
	draRef := recipe.ComponentRef{Name: draDriverComponent, Namespace: "nvidia-dra-driver"}

	tests := []struct {
		name        string
		kube        []runtime.Object
		dynamic     []runtime.Object
		refs        []recipe.ComponentRef
		wantErr     bool
		want        map[string]string
		wantMissing []string
	}{
		{
			name: "all gates healthy",
			kube: []runtime.Object{
				activeNamespace("skyhook"),
				activeNamespace("nvidia-dra-driver"),
				readyDaemonSet("nvidia-dra-driver", testDefaultDRADSName, 2),
			},
			dynamic: []runtime.Object{nodewrightWithStatus("tuning", nodewrightCompleteState)},
			refs:    []recipe.ComponentRef{nodewrightRef, draRef},
			want: map[string]string{
				validators.ExtraKeyNamespacesActive:   "2",
				validators.ExtraKeyNamespacesExpected: "2",
				validators.ExtraKeyDRAReady:           "2",
				validators.ExtraKeyDRADesired:         "2",
				validators.ExtraKeyNodewrightComplete: "1",
				validators.ExtraKeyNodewrightExpected: "1",
			},
		},
		{
			name: "incomplete nodewright and partial DRA rollout",
			kube: []runtime.Object{
				activeNamespace("skyhook"),
				activeNamespace("nvidia-dra-driver"),
				&appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Name: testDefaultDRADSName, Namespace: "nvidia-dra-driver"},
					Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 1},
				},
			},
			dynamic: []runtime.Object{nodewrightWithStatus("tuning", "waiting")},
			refs:    []recipe.ComponentRef{nodewrightRef, draRef},
			wantErr: true,
			want: map[string]string{
				validators.ExtraKeyDRAReady:           "1",
				validators.ExtraKeyDRADesired:         "2",
				validators.ExtraKeyNodewrightComplete: "0",
				validators.ExtraKeyNodewrightExpected: "1",
			},
		},
		{
			name:    "inactive namespace and missing DRA plugin",
			kube:    []runtime.Object{inactiveNamespace("nvidia-dra-driver")},
			refs:    []recipe.ComponentRef{draRef},
			wantErr: true,
			want: map[string]string{
				validators.ExtraKeyNamespacesActive:   "0",
				validators.ExtraKeyNamespacesExpected: "1",
			},
			wantMissing: []string{validators.ExtraKeyDRAReady, validators.ExtraKeyDRADesired},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := newDeploymentTestContext(t, tt.kube, tt.dynamic, tt.refs)
			extra, err := captureExtras(t, func() error { return checkExpectedResources(ctx) })
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkExpectedResources() error = %v, wantErr %v", err, tt.wantErr)
			}
			assertExtraSubset(t, extra, tt.want)
			for _, k := range tt.wantMissing {
				if v, ok := extra[k]; ok {
					t.Errorf("extra[%s] = %q, want absent", k, v)
				}
			}
		})
	}
}

func TestVerifyNodewrightReadyDisclosesSuppressedAsZero(t *testing.T) {
	ref := recipe.ComponentRef{
		Name:          nodewrightCustomizationsComponent,
		Namespace:     "skyhook",
		ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning-generic.yaml"},
		Overrides:     map[string]any{"enabled": false},
	}
	ctx := newDeploymentTestContext(t, []runtime.Object{activeNamespace("skyhook")}, nil, []recipe.ComponentRef{ref})
	extra, err := captureExtras(t, func() error {
		return verifyNodewrightReady(ctx, ref, []corev1.Taint{legacyRuntimeRequiredTaint})
	})
	if err != nil {
		t.Fatalf("verifyNodewrightReady() error = %v, want nil", err)
	}
	assertExtraSubset(t, extra, map[string]string{
		validators.ExtraKeyNodewrightComplete: "0",
		validators.ExtraKeyNodewrightExpected: "0",
	})
}

func TestCheckGPUOperatorVersionDisclosesOutcome(t *testing.T) {
	tests := []struct {
		name       string
		label      string
		constraint string
		wantErr    bool
		wantSkip   bool
		want       map[string]string
	}{
		{
			name:       "satisfied",
			constraint: ">= v25.10.0",
			want: map[string]string{
				validators.ExtraKeyDetectedVersion:  "v25.10.1",
				validators.ExtraKeyVersionSatisfied: "true",
			},
		},
		{
			name:       "release candidate disclosed as evaluated",
			label:      "v25.10.1-rc.2",
			constraint: ">= v25.10.0",
			want: map[string]string{
				validators.ExtraKeyDetectedVersion:  "v25.10.1-rc.2",
				validators.ExtraKeyVersionSatisfied: "true",
			},
		},
		{
			name:       "unparseable version has no verdict",
			label:      "latest",
			constraint: ">= v25.10.0",
			wantErr:    true,
			want:       map[string]string{validators.ExtraKeyDetectedVersion: "vlatest"},
		},
		{
			name:       "not satisfied",
			constraint: ">= v26.0.0",
			wantErr:    true,
			want: map[string]string{
				validators.ExtraKeyDetectedVersion:  "v25.10.1",
				validators.ExtraKeyVersionSatisfied: "false",
			},
		},
		{
			name:     "no constraint",
			wantSkip: true,
			want: map[string]string{
				validators.ExtraKeyDetectedVersion: "v25.10.1",
				validators.ExtraKeySkipReason:      validators.SkipReasonNoConstraint,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label := tt.label
			if label == "" {
				label = "25.10.1"
			}
			deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Name:      gpuOperatorNamespace,
				Namespace: gpuOperatorNamespace,
				Labels:    map[string]string{"app.kubernetes.io/version": label},
			}}
			ctx := &validators.Context{
				Ctx:             context.Background(),
				Clientset:       k8sfake.NewClientset(deploy),
				ValidationInput: &v1.ValidationInput{},
			}
			if tt.constraint != "" {
				ctx.ValidationInput.Config.Deployment = &v1.ValidationPhase{
					Constraints: []recipe.Constraint{{Name: "Deployment.gpu-operator.version", Value: tt.constraint}},
				}
			}
			extra, err := captureExtras(t, func() error { return checkGPUOperatorVersion(ctx) })
			switch {
			case tt.wantSkip:
				if !validators.IsSkip(err) {
					t.Fatalf("checkGPUOperatorVersion() error = %v, want a skip", err)
				}
			case (err != nil) != tt.wantErr:
				t.Fatalf("checkGPUOperatorVersion() error = %v, wantErr %v", err, tt.wantErr)
			}
			assertExtraSubset(t, extra, tt.want)
			if !tt.wantSkip {
				if v, ok := extra[validators.ExtraKeySkipReason]; ok {
					t.Errorf("skipReason = %q on a non-skip outcome", v)
				}
			}
			if _, want := tt.want[validators.ExtraKeyVersionSatisfied]; !want {
				if v, ok := extra[validators.ExtraKeyVersionSatisfied]; ok {
					t.Errorf("versionSatisfied = %q, want absent", v)
				}
			}
		})
	}
}
