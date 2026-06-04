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

	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func lbService(name string, sourceRanges []string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentgateway-system"},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeLoadBalancer,
			LoadBalancerSourceRanges: sourceRanges,
		},
	}
}

func TestAssessGatewayExposure(t *testing.T) {
	tests := []struct {
		name          string
		objects       []*corev1.Service
		requireScoped bool
		wantErr       bool
	}{
		{
			name:    "open LoadBalancer, default policy → warn (pass)",
			objects: []*corev1.Service{lbService("inference-gateway", nil)},
			wantErr: false,
		},
		{
			name:          "open LoadBalancer, enforce policy → fail",
			objects:       []*corev1.Service{lbService("inference-gateway", nil)},
			requireScoped: true,
			wantErr:       true,
		},
		{
			name:          "scoped LoadBalancer, enforce policy → pass",
			objects:       []*corev1.Service{lbService("inference-gateway", []string{"10.0.0.0/8"})},
			requireScoped: true,
			wantErr:       false,
		},
		{
			name:          "explicit 0.0.0.0/0, enforce policy → fail",
			objects:       []*corev1.Service{lbService("inference-gateway", []string{"0.0.0.0/0"})},
			requireScoped: true,
			wantErr:       true,
		},
		{
			name:          "explicit ::/0 (IPv6 any), enforce policy → fail",
			objects:       []*corev1.Service{lbService("inference-gateway", []string{"::/0"})},
			requireScoped: true,
			wantErr:       true,
		},
		{
			name:          "any-source CIDR mixed with a scoped range, enforce policy → fail",
			objects:       []*corev1.Service{lbService("inference-gateway", []string{"10.0.0.0/8", "0.0.0.0/0"})},
			requireScoped: true,
			wantErr:       true,
		},
		{
			name:          "unrelated open LoadBalancer is ignored, enforce policy → pass",
			objects:       []*corev1.Service{lbService("agentgateway-controller-manager", nil)},
			requireScoped: true,
			wantErr:       false,
		},
		{
			// Shares the "inference-gateway" prefix but is a control-plane
			// component, not the data-plane proxy: must not fail enforce mode.
			name:          "inference-gateway control-plane LB is ignored, enforce policy → pass",
			objects:       []*corev1.Service{lbService("inference-gateway-controller-manager", nil)},
			requireScoped: true,
			wantErr:       false,
		},
		{
			name: "non-LoadBalancer service is ignored",
			objects: []*corev1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "inference-gateway", Namespace: "agentgateway-system"},
					Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
				},
			},
			requireScoped: true,
			wantErr:       false,
		},
		{
			name:          "no services → pass",
			objects:       nil,
			requireScoped: true,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tt.objects))
			for _, o := range tt.objects {
				objs = append(objs, o)
			}
			vctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: fake.NewClientset(objs...),
			}
			if tt.requireScoped {
				t.Setenv(requireScopedGatewayEnv, "true")
			} else {
				t.Setenv(requireScopedGatewayEnv, "")
			}

			err := assessGatewayExposure(vctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("assessGatewayExposure() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "0.0.0.0/0") {
				t.Errorf("error %q should describe the open exposure", err.Error())
			}
		})
	}
}

func TestAssessGatewayExposure_NilClientset(t *testing.T) {
	err := assessGatewayExposure(&validators.Context{Ctx: context.Background()})
	if err == nil {
		t.Fatal("expected error when clientset is nil")
	}
}

// TestAssessGatewayExposure_ListError verifies that a transient apiserver error
// listing Services does not fail the (already-passed) inference-gateway check in
// the default advisory mode, but does fail closed under enforce mode. See #1160.
func TestAssessGatewayExposure_ListError(t *testing.T) {
	tests := []struct {
		name          string
		requireScoped bool
		wantErr       bool
	}{
		{name: "default advisory mode → transient list error is non-fatal", requireScoped: false, wantErr: false},
		{name: "enforce mode → transient list error fails closed", requireScoped: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			client.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("apiserver unavailable")
			})
			if tt.requireScoped {
				t.Setenv(requireScopedGatewayEnv, "true")
			} else {
				t.Setenv(requireScopedGatewayEnv, "")
			}
			err := assessGatewayExposure(&validators.Context{Ctx: context.Background(), Clientset: client})
			if (err != nil) != tt.wantErr {
				t.Fatalf("assessGatewayExposure() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIsInferenceGatewayProxyName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"exact proxy name", "inference-gateway", true},
		{"proxy with suffix", "inference-gateway-proxy", true},
		{"controller-manager excluded", "inference-gateway-controller-manager", false},
		{"webhook excluded", "inference-gateway-webhook", false},
		{"metrics excluded", "inference-gateway-metrics", false},
		{"unrelated name without prefix", "agentgateway-controller-manager", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isInferenceGatewayProxyName(tt.in); got != tt.want {
				t.Errorf("isInferenceGatewayProxyName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestEnvTruthy(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"True", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			// Set unconditionally (incl. the empty-string case) so each subtest
			// fully controls the var and can't read an ambient value — hermetic.
			t.Setenv("AICR_TEST_ENV_TRUTHY", tt.val)
			got := envTruthy("AICR_TEST_ENV_TRUTHY")
			if got != tt.want {
				t.Errorf("envTruthy(%q) = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}
