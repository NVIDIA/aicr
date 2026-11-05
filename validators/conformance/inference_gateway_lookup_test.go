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
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestCheckInferenceGateway_ReadErrors pins that a read the check cannot
// complete is reported as a read failure, never as the object being absent:
// RBAC denial on a GatewayClass, Gateway or CRD leaves its existence unknown.
func TestCheckInferenceGateway_ReadErrors(t *testing.T) {
	const ns = "edge-system"
	profile := &validators.InferenceGatewayProfile{Component: "edge", GatewayClass: testGatewayClass, GatewayName: "inference-gateway", Namespace: ns}
	ref := recipe.ComponentRef{Name: "edge", Namespace: ns, Overrides: map[string]any{
		recipe.InferenceGatewayOverrideKey: map[string]any{"gatewayClass": testGatewayClass, "dataPlane": "gateway-address"},
	}}
	crds := []string{
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"inferencepools.inference.networking.k8s.io",
	}

	tests := []struct {
		name     string
		resource string
		wantErr  string
	}{
		{"GatewayClass read denied", "gatewayclasses", `failed to read GatewayClass "edge-class"`},
		{"Gateway read denied", "gateways", "failed to read Gateway edge-system/inference-gateway"},
		{"CRD read denied", "customresourcedefinitions", "failed to read CRD gateways.gateway.networking.k8s.io"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnforce(t, false)
			gw := programmedGateway(profile, "True", []any{programmedListener("http", "True")}, []any{gatewayAddress("IPAddress", "172.16.100.12")})
			dyn := newGatewayFakeDynamicClient(t, gw)
			if err := dyn.Tracker().Create(gatewayClassGVR, gatewayClassObject("True"), ""); err != nil {
				t.Fatalf("seed GatewayClass: %v", err)
			}
			for _, name := range crds {
				if err := dyn.Tracker().Create(crdGVR, crdObject(name), ""); err != nil {
					t.Fatalf("seed CRD %s: %v", name, err)
				}
			}
			dyn.PrependReactor("get", tt.resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: tt.resource}, "", fmt.Errorf("denied"))
			})
			vctx := &validators.Context{
				Ctx: context.Background(), Clientset: fake.NewClientset(), DynamicClient: dyn,
				ValidationInput: &validatorv1.ValidationInput{ComponentRefs: []recipe.ComponentRef{ref}},
			}

			err := CheckInferenceGateway(vctx)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("CheckInferenceGateway() error = %v, want containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "not found") {
				t.Errorf("a denied read must not be reported as absent: %v", err)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		})
	}
}
