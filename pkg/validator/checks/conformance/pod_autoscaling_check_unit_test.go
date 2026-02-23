// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/validator/checks"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCheckPodAutoscaling(t *testing.T) {
	tests := []struct {
		name        string
		clientset   bool
		wantErr     bool
		errContains string
	}{
		{
			name:        "no clientset",
			clientset:   false,
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name:        "fake client lacks REST client",
			clientset:   true,
			wantErr:     true,
			errContains: "discovery REST client is not available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset()
				ctx = &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
				}
			} else {
				ctx = &checks.ValidationContext{
					Context: context.Background(),
				}
			}

			err := CheckPodAutoscaling(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckPodAutoscaling() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckPodAutoscaling() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestValidateHPABehavior(t *testing.T) {
	tests := []struct {
		name            string
		desiredReplicas int32
		currentReplicas int32
		deployReplicas  int32
		wantErr         bool
		errContains     string
		useShortTimeout bool // use very short timeout to trigger timeout error
	}{
		{
			name:            "scaling intent detected and deployment scales",
			desiredReplicas: 2,
			currentReplicas: 1,
			deployReplicas:  2,
			wantErr:         false,
		},
		{
			name:            "no scaling intent",
			desiredReplicas: 1,
			currentReplicas: 1,
			wantErr:         true,
			errContains:     "HPA did not report scaling intent",
			useShortTimeout: true,
		},
		{
			name:            "scale from zero",
			desiredReplicas: 3,
			currentReplicas: 0,
			deployReplicas:  3,
			wantErr:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			clientset := fake.NewSimpleClientset()

			// HPA Get reactor: return HPA with specified status.
			clientset.PrependReactor("get", "horizontalpodautoscalers",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					hpa := &autoscalingv2.HorizontalPodAutoscaler{
						ObjectMeta: metav1.ObjectMeta{
							Name:      action.(k8stesting.GetAction).GetName(),
							Namespace: action.GetNamespace(),
						},
						Status: autoscalingv2.HorizontalPodAutoscalerStatus{
							DesiredReplicas: tt.desiredReplicas,
							CurrentReplicas: tt.currentReplicas,
						},
					}
					return true, hpa, nil
				})

			// Deployment Get reactor: return Deployment with scaled replicas.
			clientset.PrependReactor("get", "deployments",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					deploy := &appsv1.Deployment{
						ObjectMeta: metav1.ObjectMeta{
							Name:      action.(k8stesting.GetAction).GetName(),
							Namespace: action.GetNamespace(),
						},
						Status: appsv1.DeploymentStatus{
							Replicas: tt.deployReplicas,
						},
					}
					return true, deploy, nil
				})

			ctx := context.Background()
			if tt.useShortTimeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 1*time.Second)
				defer cancel()
			}

			err := validateHPABehavior(ctx, clientset)

			if (err != nil) != tt.wantErr {
				t.Errorf("validateHPABehavior() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("validateHPABehavior() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckPodAutoscalingRegistration(t *testing.T) {
	check, ok := checks.GetCheck("pod-autoscaling")
	if !ok {
		t.Fatal("pod-autoscaling check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}
