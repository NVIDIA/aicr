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

package deployment

import (
	"context"
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCheckOperatorHealth(t *testing.T) {
	tests := []struct {
		name        string
		pods        []corev1.Pod
		clientset   bool
		listError   bool
		wantErr     bool
		errContains string
	}{
		{
			name: "healthy operator with running pod",
			pods: []corev1.Pod{
				createGPUOperatorPod("gpu-operator-abc", "gpu-operator", corev1.PodRunning),
			},
			clientset: true,
			wantErr:   false,
		},
		{
			name: "multiple running pods",
			pods: []corev1.Pod{
				createGPUOperatorPod("gpu-operator-1", "gpu-operator", corev1.PodRunning),
				createGPUOperatorPod("gpu-operator-2", "gpu-operator", corev1.PodRunning),
			},
			clientset: true,
			wantErr:   false,
		},
		{
			name: "mixed pod states with at least one running",
			pods: []corev1.Pod{
				createGPUOperatorPod("gpu-operator-1", "gpu-operator", corev1.PodRunning),
				createGPUOperatorPod("gpu-operator-2", "gpu-operator", corev1.PodPending),
				createGPUOperatorPod("gpu-operator-3", "gpu-operator", corev1.PodFailed),
			},
			clientset: true,
			wantErr:   false,
		},
		{
			name:        "no clientset available",
			clientset:   false,
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name:        "no pods found",
			pods:        []corev1.Pod{},
			clientset:   true,
			wantErr:     true,
			errContains: "no gpu-operator pods found",
		},
		{
			name: "pods exist but none running",
			pods: []corev1.Pod{
				createGPUOperatorPod("gpu-operator-1", "gpu-operator", corev1.PodPending),
				createGPUOperatorPod("gpu-operator-2", "gpu-operator", corev1.PodFailed),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "no gpu-operator pods are in Running state",
		},
		{
			name: "pods in unknown state",
			pods: []corev1.Pod{
				createGPUOperatorPod("gpu-operator-1", "gpu-operator", corev1.PodUnknown),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "no gpu-operator pods are in Running state",
		},
		{
			name: "pods succeeded but not running",
			pods: []corev1.Pod{
				createGPUOperatorPod("gpu-operator-1", "gpu-operator", corev1.PodSucceeded),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "no gpu-operator pods are in Running state",
		},
		{
			name:        "failed to list pods",
			clientset:   true,
			listError:   true,
			wantErr:     true,
			errContains: "failed to list gpu-operator pods",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				// Create fake clientset with pods
				objects := make([]runtime.Object, 0, len(tt.pods))
				for i := range tt.pods {
					objects = append(objects, &tt.pods[i])
				}

				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)

				// Simulate API error if requested
				if tt.listError {
					clientset.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, &fakeAPIError{message: "simulated API error"}
					})
				}

				ctx = &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
				}
			} else {
				// No clientset
				ctx = &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: nil,
				}
			}

			err := CheckOperatorHealth(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckOperatorHealth() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !containsString(err.Error(), tt.errContains) {
					t.Errorf("CheckOperatorHealth() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckOperatorHealthRegistration(t *testing.T) {
	// Verify the check is registered
	check, ok := checks.GetCheck("operator-health")
	if !ok {
		t.Fatal("operator-health check not registered")
	}

	if check.Name != "operator-health" {
		t.Errorf("Name = %v, want operator-health", check.Name)
	}

	if check.Phase != "deployment" {
		t.Errorf("Phase = %v, want deployment", check.Phase)
	}

	if check.Description == "" {
		t.Error("Description is empty")
	}

	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

func TestCheckOperatorHealthWithDifferentNamespaces(t *testing.T) {
	// The check specifically looks in "gpu-operator" namespace
	// Pods in other namespaces should not be found

	// Create pods in different namespace
	pods := []corev1.Pod{
		createGPUOperatorPod("gpu-operator-1", "wrong-namespace", corev1.PodRunning),
	}

	objects := make([]runtime.Object, 0, len(pods))
	for i := range pods {
		objects = append(objects, &pods[i])
	}

	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	clientset := fake.NewSimpleClientset(objects...)

	ctx := &checks.ValidationContext{
		Context:   context.Background(),
		Clientset: clientset,
	}

	err := CheckOperatorHealth(ctx)

	if err == nil {
		t.Error("CheckOperatorHealth() should fail when pods are in wrong namespace")
	}

	if !containsString(err.Error(), "no gpu-operator pods found") {
		t.Errorf("CheckOperatorHealth() error = %v, should contain 'no gpu-operator pods found'", err)
	}
}

func TestCheckOperatorHealthWithWrongLabels(t *testing.T) {
	// The check specifically looks for label "app=gpu-operator"
	// Pods with different labels should not be found

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-operator-1",
			Namespace: "gpu-operator",
			Labels: map[string]string{
				"app": "wrong-app", // Wrong label value
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	clientset := fake.NewSimpleClientset(&pod)

	ctx := &checks.ValidationContext{
		Context:   context.Background(),
		Clientset: clientset,
	}

	err := CheckOperatorHealth(ctx)

	if err == nil {
		t.Error("CheckOperatorHealth() should fail when pods have wrong labels")
	}

	if !containsString(err.Error(), "no gpu-operator pods found") {
		t.Errorf("CheckOperatorHealth() error = %v, should contain 'no gpu-operator pods found'", err)
	}
}

// Helper function to create a GPU operator pod for testing
func createGPUOperatorPod(name, namespace string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app": "gpu-operator",
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

// Helper function to check if a string contains a substring
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			len(s) > len(substr)+1 && findSubstr(s, substr)))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// fakeAPIError implements error interface for simulating API errors
type fakeAPIError struct {
	message string
}

func (e *fakeAPIError) Error() string {
	return e.message
}
