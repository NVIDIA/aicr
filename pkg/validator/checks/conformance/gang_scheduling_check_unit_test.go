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

	"github.com/NVIDIA/aicr/pkg/validator/checks"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckGangScheduling(t *testing.T) {
	// Build the full set of KAI scheduler deployments for the happy path.
	allDeployments := []runtime.Object{
		createDeployment("kai-scheduler", "kai-scheduler-default", 1),
		createDeployment("kai-scheduler", "admission", 1),
		createDeployment("kai-scheduler", "binder", 1),
		createDeployment("kai-scheduler", "kai-operator", 1),
		createDeployment("kai-scheduler", "pod-grouper", 1),
		createDeployment("kai-scheduler", "podgroup-controller", 1),
		createDeployment("kai-scheduler", "queue-controller", 1),
	}

	tests := []struct {
		name           string
		k8sObjects     []runtime.Object
		dynamicObjects []runtime.Object
		clientset      bool
		wantErr        bool
		errContains    string
	}{
		{
			name:       "all healthy",
			k8sObjects: allDeployments,
			dynamicObjects: []runtime.Object{
				createCRD("queues.scheduling.run.ai"),
				createCRD("podgroups.scheduling.run.ai"),
			},
			clientset: true,
			wantErr:   false,
		},
		{
			name:        "no clientset",
			clientset:   false,
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name: "missing one deployment",
			k8sObjects: []runtime.Object{
				// Only first 6 — missing queue-controller
				createDeployment("kai-scheduler", "kai-scheduler-default", 1),
				createDeployment("kai-scheduler", "admission", 1),
				createDeployment("kai-scheduler", "binder", 1),
				createDeployment("kai-scheduler", "kai-operator", 1),
				createDeployment("kai-scheduler", "pod-grouper", 1),
				createDeployment("kai-scheduler", "podgroup-controller", 1),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "queue-controller check failed",
		},
		{
			name: "deployment not available",
			k8sObjects: []runtime.Object{
				createDeployment("kai-scheduler", "kai-scheduler-default", 0), // 0 available
				createDeployment("kai-scheduler", "admission", 1),
				createDeployment("kai-scheduler", "binder", 1),
				createDeployment("kai-scheduler", "kai-operator", 1),
				createDeployment("kai-scheduler", "pod-grouper", 1),
				createDeployment("kai-scheduler", "podgroup-controller", 1),
				createDeployment("kai-scheduler", "queue-controller", 1),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "kai-scheduler-default check failed",
		},
		{
			name:       "missing CRD",
			k8sObjects: allDeployments,
			dynamicObjects: []runtime.Object{
				createCRD("queues.scheduling.run.ai"),
				// Missing podgroups CRD
			},
			clientset:   true,
			wantErr:     true,
			errContains: "podgroups.scheduling.run.ai not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(tt.k8sObjects...)

				scheme := runtime.NewScheme()
				dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
					},
					tt.dynamicObjects...)

				ctx = &checks.ValidationContext{
					Context:       context.Background(),
					Clientset:     clientset,
					DynamicClient: dynClient,
				}
			} else {
				ctx = &checks.ValidationContext{
					Context: context.Background(),
				}
			}

			err := CheckGangScheduling(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckGangScheduling() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckGangScheduling() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckGangSchedulingRegistration(t *testing.T) {
	check, ok := checks.GetCheck("gang-scheduling")
	if !ok {
		t.Fatal("gang-scheduling check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}
