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

	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestValidateGPUOperatorVersion(t *testing.T) {
	tests := []struct {
		name               string
		deployment         *appsv1.Deployment
		constraint         recipe.Constraint
		wantVersion        string
		wantPassed         bool
		wantErr            bool
		expectNoDeployment bool
	}{
		{
			name: "version matches exact constraint",
			deployment: createGPUOperatorDeployment("gpu-operator", "gpu-operator", map[string]string{
				"app.kubernetes.io/version": "v24.6.0",
			}, "nvcr.io/nvidia/gpu-operator:v24.6.0"),
			constraint: recipe.Constraint{
				Name:  "Deployment.gpu-operator.version",
				Value: "== v24.6.0",
			},
			wantVersion: "v24.6.0",
			wantPassed:  true,
			wantErr:     false,
		},
		{
			name: "version satisfies >= constraint",
			deployment: createGPUOperatorDeployment("gpu-operator", "gpu-operator", map[string]string{
				"app.kubernetes.io/version": "v24.6.1",
			}, "nvcr.io/nvidia/gpu-operator:v24.6.1"),
			constraint: recipe.Constraint{
				Name:  "Deployment.gpu-operator.version",
				Value: ">= v24.6.0",
			},
			wantVersion: "v24.6.1",
			wantPassed:  true,
			wantErr:     false,
		},
		{
			name: "version fails < constraint",
			deployment: createGPUOperatorDeployment("gpu-operator", "gpu-operator", map[string]string{
				"app.kubernetes.io/version": "v24.3.0",
			}, "nvcr.io/nvidia/gpu-operator:v24.3.0"),
			constraint: recipe.Constraint{
				Name:  "Deployment.gpu-operator.version",
				Value: ">= v24.6.0",
			},
			wantVersion: "v24.3.0",
			wantPassed:  false,
			wantErr:     false,
		},
		{
			name: "extract version from image tag when label missing",
			deployment: createGPUOperatorDeployment("gpu-operator", "gpu-operator", nil,
				"nvcr.io/nvidia/gpu-operator:v25.10.1"),
			constraint: recipe.Constraint{
				Name:  "Deployment.gpu-operator.version",
				Value: "== v25.10.1",
			},
			wantVersion: "v25.10.1",
			wantPassed:  true,
			wantErr:     false,
		},
		{
			name: "handle image tag with os suffix",
			deployment: createGPUOperatorDeployment("gpu-operator", "gpu-operator", nil,
				"nvcr.io/nvidia/gpu-operator:v24.6.0-ubuntu22.04"),
			constraint: recipe.Constraint{
				Name:  "Deployment.gpu-operator.version",
				Value: "== v24.6.0",
			},
			wantVersion: "v24.6.0",
			wantPassed:  true,
			wantErr:     false,
		},
		{
			name:       "deployment not found",
			deployment: nil,
			constraint: recipe.Constraint{
				Name:  "Deployment.gpu-operator.version",
				Value: ">= v24.6.0",
			},
			expectNoDeployment: true,
			wantErr:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake clientset
			var clientset *fake.Clientset
			if tt.deployment != nil {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset = fake.NewSimpleClientset(tt.deployment)
			} else {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset = fake.NewSimpleClientset()
			}

			ctx := &checks.ValidationContext{
				Context:   context.Background(),
				Clientset: clientset,
			}

			gotVersion, gotPassed, err := ValidateGPUOperatorVersion(ctx, tt.constraint)

			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateGPUOperatorVersion() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if gotVersion != tt.wantVersion {
				t.Errorf("ValidateGPUOperatorVersion() version = %v, want %v", gotVersion, tt.wantVersion)
			}

			if gotPassed != tt.wantPassed {
				t.Errorf("ValidateGPUOperatorVersion() passed = %v, want %v", gotPassed, tt.wantPassed)
			}
		})
	}
}

func TestExtractVersionFromImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "nvcr.io with v prefix",
			image: "nvcr.io/nvidia/gpu-operator:v24.6.0",
			want:  "v24.6.0",
		},
		{
			name:  "nvcr.io with os suffix",
			image: "nvcr.io/nvidia/gpu-operator:v24.6.0-ubuntu22.04",
			want:  "v24.6.0",
		},
		{
			name:  "docker.io without v prefix",
			image: "docker.io/nvidia/gpu-operator:24.6.0",
			want:  "v24.6.0",
		},
		{
			name:  "latest tag",
			image: "nvcr.io/nvidia/gpu-operator:latest",
			want:  "vlatest",
		},
		{
			name:  "no colon separator",
			image: "nvcr.io/nvidia/gpu-operator",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractVersionFromImage(tt.image)
			if got != tt.want {
				t.Errorf("extractVersionFromImage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{
			name:    "already has v prefix",
			version: "v24.6.0",
			want:    "v24.6.0",
		},
		{
			name:    "missing v prefix",
			version: "24.6.0",
			want:    "v24.6.0",
		},
		{
			name:    "with whitespace",
			version: " v24.6.0 ",
			want:    "v24.6.0",
		},
		{
			name:    "empty string",
			version: "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeVersion(tt.version)
			if got != tt.want {
				t.Errorf("normalizeVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluateVersionConstraint(t *testing.T) {
	tests := []struct {
		name           string
		actualVersion  string
		constraintExpr string
		wantPassed     bool
		wantErr        bool
	}{
		{
			name:           "exact match passes",
			actualVersion:  "v24.6.0",
			constraintExpr: "== v24.6.0",
			wantPassed:     true,
		},
		{
			name:           "greater than passes",
			actualVersion:  "v25.0.0",
			constraintExpr: ">= v24.6.0",
			wantPassed:     true,
		},
		{
			name:           "less than fails",
			actualVersion:  "v24.3.0",
			constraintExpr: ">= v24.6.0",
			wantPassed:     false,
		},
		{
			name:           "empty constraint expression",
			actualVersion:  "v24.6.0",
			constraintExpr: "",
			wantErr:        true,
		},
		{
			name:           "invalid actual version",
			actualVersion:  "not-a-version",
			constraintExpr: ">= v24.6.0",
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			passed, err := evaluateVersionConstraint(tt.actualVersion, tt.constraintExpr)
			if (err != nil) != tt.wantErr {
				t.Errorf("evaluateVersionConstraint() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && passed != tt.wantPassed {
				t.Errorf("evaluateVersionConstraint() = %v, want %v", passed, tt.wantPassed)
			}
		})
	}
}

// Helper function to create a GPU operator deployment for testing
//
//nolint:unparam // namespace may vary in other tests
func createGPUOperatorDeployment(namespace, name string, labels map[string]string, image string) *appsv1.Deployment {
	if labels == nil {
		labels = make(map[string]string)
	}
	labels["app"] = "gpu-operator"

	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "gpu-operator",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "gpu-operator",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "gpu-operator",
							Image: image,
						},
					},
				},
			},
		},
	}
}
