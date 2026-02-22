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
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckRobustController(t *testing.T) {
	tests := []struct {
		name           string
		k8sObjects     []runtime.Object
		dynamicObjects []runtime.Object
		clientset      bool
		wantErr        bool
		errContains    string
	}{
		{
			name: "all healthy",
			k8sObjects: []runtime.Object{
				createDeployment("dynamo-system", "dynamo-platform-dynamo-operator-controller-manager", 1),
				createDynamoWebhookConfig("dynamo-system", "dynamo-webhook-service"),
				createEndpointSlice("dynamo-system", "dynamo-webhook-service"),
			},
			dynamicObjects: []runtime.Object{
				createCRD("dynamographdeployments.nvidia.com"),
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
			name: "operator deployment not available",
			k8sObjects: []runtime.Object{
				createDeployment("dynamo-system", "dynamo-platform-dynamo-operator-controller-manager", 0),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "Dynamo operator controller-manager check failed",
		},
		{
			name:       "operator deployment missing",
			k8sObjects: []runtime.Object{
				// No operator deployment
			},
			clientset:   true,
			wantErr:     true,
			errContains: "Dynamo operator controller-manager check failed",
		},
		{
			name: "webhook missing",
			k8sObjects: []runtime.Object{
				createDeployment("dynamo-system", "dynamo-platform-dynamo-operator-controller-manager", 1),
				// No webhook configuration
			},
			clientset:   true,
			wantErr:     true,
			errContains: "Dynamo validating webhook configuration not found",
		},
		{
			name: "webhook endpoint missing",
			k8sObjects: []runtime.Object{
				createDeployment("dynamo-system", "dynamo-platform-dynamo-operator-controller-manager", 1),
				createDynamoWebhookConfig("dynamo-system", "dynamo-webhook-service"),
				// No endpoints for the webhook service
			},
			clientset:   true,
			wantErr:     true,
			errContains: "EndpointSlice for webhook service",
		},
		{
			name: "CRD missing",
			k8sObjects: []runtime.Object{
				createDeployment("dynamo-system", "dynamo-platform-dynamo-operator-controller-manager", 1),
				createDynamoWebhookConfig("dynamo-system", "dynamo-webhook-service"),
				createEndpointSlice("dynamo-system", "dynamo-webhook-service"),
			},
			dynamicObjects: []runtime.Object{
				// No CRD
			},
			clientset:   true,
			wantErr:     true,
			errContains: "DynamoGraphDeployment CRD not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(tt.k8sObjects...)

				scheme := runtime.NewScheme()
				var dynClient *dynamicfake.FakeDynamicClient
				if len(tt.dynamicObjects) > 0 {
					dynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
						map[schema.GroupVersionResource]string{
							{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
						},
						tt.dynamicObjects...)
				} else {
					dynClient = dynamicfake.NewSimpleDynamicClient(scheme)
				}

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

			err := CheckRobustController(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckRobustController() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckRobustController() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckRobustControllerRegistration(t *testing.T) {
	check, ok := checks.GetCheck("robust-controller")
	if !ok {
		t.Fatal("robust-controller check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

// createDynamoWebhookConfig creates a ValidatingWebhookConfiguration for testing.
func createDynamoWebhookConfig(namespace, serviceName string) *admissionregistrationv1.ValidatingWebhookConfiguration {
	sideEffectsNone := admissionregistrationv1.SideEffectClassNone
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dynamo-validating-webhook",
		},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{
				Name:                    "validate.dynamo.nvidia.com",
				SideEffects:             &sideEffectsNone,
				AdmissionReviewVersions: []string{"v1"},
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Name:      serviceName,
						Namespace: namespace,
					},
				},
			},
		},
	}
}

// createEndpointSlice creates a minimal EndpointSlice for a service.
func createEndpointSlice(namespace, serviceName string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName + "-abc",
			Namespace: namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name": serviceName,
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.1"}},
		},
	}
}

// createCRD creates an unstructured CustomResourceDefinition for testing.
func createCRD(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata": map[string]interface{}{
				"name": name,
			},
		},
	}
}
