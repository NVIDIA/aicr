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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestCheckInferenceGateway(t *testing.T) {
	tests := []struct {
		name         string
		setup        func(*dynamicfake.FakeDynamicClient) // populate objects via Create
		hasDynClient bool
		wantErr      bool
		errContains  string
	}{
		{
			name: "all healthy",
			setup: func(dc *dynamicfake.FakeDynamicClient) {
				createDynObject(t, dc, gcGVR, "", createGatewayClass("True"))
				createDynObject(t, dc, gwGVR, "kgateway-system", createGateway("kgateway-system", "inference-gateway", "Programmed", "True"))
				createDynObject(t, dc, crdGVR, "", createCRD("gateways.gateway.networking.k8s.io"))
				createDynObject(t, dc, crdGVR, "", createCRD("httproutes.gateway.networking.k8s.io"))
				createDynObject(t, dc, crdGVR, "", createCRD("inferencepools.inference.networking.x-k8s.io"))
			},
			hasDynClient: true,
			wantErr:      false,
		},
		{
			name:         "no dynamic client",
			hasDynClient: false,
			wantErr:      true,
			errContains:  "RESTConfig is not available",
		},
		{
			name:         "GatewayClass missing",
			setup:        func(dc *dynamicfake.FakeDynamicClient) {},
			hasDynClient: true,
			wantErr:      true,
			errContains:  "GatewayClass 'kgateway' not found",
		},
		{
			name: "GatewayClass not accepted",
			setup: func(dc *dynamicfake.FakeDynamicClient) {
				createDynObject(t, dc, gcGVR, "", createGatewayClass("False"))
			},
			hasDynClient: true,
			wantErr:      true,
			errContains:  "GatewayClass not accepted",
		},
		{
			name: "Gateway missing",
			setup: func(dc *dynamicfake.FakeDynamicClient) {
				createDynObject(t, dc, gcGVR, "", createGatewayClass("True"))
			},
			hasDynClient: true,
			wantErr:      true,
			errContains:  "Gateway 'inference-gateway' not found",
		},
		{
			name: "Gateway not programmed",
			setup: func(dc *dynamicfake.FakeDynamicClient) {
				createDynObject(t, dc, gcGVR, "", createGatewayClass("True"))
				createDynObject(t, dc, gwGVR, "kgateway-system", createGateway("kgateway-system", "inference-gateway", "Programmed", "False"))
			},
			hasDynClient: true,
			wantErr:      true,
			errContains:  "Gateway not programmed",
		},
		{
			name: "missing CRD",
			setup: func(dc *dynamicfake.FakeDynamicClient) {
				createDynObject(t, dc, gcGVR, "", createGatewayClass("True"))
				createDynObject(t, dc, gwGVR, "kgateway-system", createGateway("kgateway-system", "inference-gateway", "Programmed", "True"))
				createDynObject(t, dc, crdGVR, "", createCRD("gateways.gateway.networking.k8s.io"))
			},
			hasDynClient: true,
			wantErr:      true,
			errContains:  "CRD httproutes.gateway.networking.k8s.io not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.hasDynClient {
				scheme := runtime.NewScheme()
				dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						gcGVR:  "GatewayClassList",
						gwGVR:  "GatewayList",
						crdGVR: "CustomResourceDefinitionList",
					})
				if tt.setup != nil {
					tt.setup(dynClient)
				}

				ctx = &checks.ValidationContext{
					Context:       context.Background(),
					DynamicClient: dynClient,
				}
			} else {
				ctx = &checks.ValidationContext{
					Context: context.Background(),
				}
			}

			err := CheckInferenceGateway(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckInferenceGateway() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckInferenceGateway() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

// GVRs used in inference gateway tests.
var (
	gcGVR = schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses",
	}
	gwGVR = schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways",
	}
	crdGVR = schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
)

// createDynObject adds an unstructured object to the fake dynamic client via Create.
// This avoids the scheme.ObjectKinds collision that happens with tracker.Add
// when multiple unstructured types are registered in the same scheme.
func createDynObject(t *testing.T, dc *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) {
	t.Helper()
	var err error
	if namespace != "" {
		_, err = dc.Resource(gvr).Namespace(namespace).Create(context.Background(), obj, metav1.CreateOptions{})
	} else {
		_, err = dc.Resource(gvr).Create(context.Background(), obj, metav1.CreateOptions{})
	}
	if err != nil {
		t.Fatalf("failed to create dynamic object %s: %v", obj.GetName(), err)
	}
}

func TestCheckInferenceGatewayRegistration(t *testing.T) {
	check, ok := checks.GetCheck("inference-gateway")
	if !ok {
		t.Fatal("inference-gateway check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

// createGatewayClass creates an unstructured GatewayClass "kgateway" with Accepted condition.
func createGatewayClass(condStatus string) *unstructured.Unstructured {
	condType := "Accepted"
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "GatewayClass",
			"metadata": map[string]interface{}{
				"name": "kgateway",
			},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   condType,
						"status": condStatus,
					},
				},
			},
		},
	}
}

// createGateway creates an unstructured Gateway with the given condition.
func createGateway(namespace, name, condType, condStatus string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   condType,
						"status": condStatus,
					},
				},
			},
		},
	}
}
