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
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/NVIDIA/aicr/validators"
)

func activeNamespace(name string) runtime.Object {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

// TestCheckPlatformHealthDisabledRef verifies that componentRefs disabled via
// overrides.enabled: false are skipped: their namespaces are never deployed, so
// requiring them to exist would false-fail an otherwise healthy cluster (#1678).
func TestCheckPlatformHealthDisabledRef(t *testing.T) {
	tests := []struct {
		name    string
		refs    []recipe.ComponentRef
		objects []runtime.Object
		wantErr bool
	}{
		{
			name: "disabled ref with missing namespace passes",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator", Namespace: "gpu-operator"},
				{Name: "nvidia-dra-driver-gpu", Namespace: "nvidia-dra-driver",
					Overrides: map[string]any{"enabled": false}},
			},
			objects: []runtime.Object{activeNamespace("gpu-operator")},
			wantErr: false,
		},
		{
			name: "enabled ref with missing namespace fails",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator", Namespace: "gpu-operator"},
			},
			objects: nil,
			wantErr: true,
		},
		{
			name: "all enabled namespaces present passes",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator", Namespace: "gpu-operator"},
				{Name: "cert-manager", Namespace: "cert-manager"},
			},
			objects: []runtime.Object{activeNamespace("gpu-operator"), activeNamespace("cert-manager")},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewSimpleClientset(tt.objects...)
			ctx := &validators.Context{
				Ctx:             context.Background(),
				Clientset:       client,
				ValidationInput: validationInputWithRefs(tt.refs...),
			}
			err := CheckPlatformHealth(ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckPlatformHealth() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
