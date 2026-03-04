// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package agent

import (
	"context"
	"strings"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCheckPermissions(t *testing.T) {
	tests := []struct {
		name        string
		allowed     bool
		wantErr     bool
		errContains string
	}{
		{
			name:    "all permissions allowed",
			allowed: true,
			wantErr: false,
		},
		{
			name:        "permissions denied",
			allowed:     false,
			wantErr:     true,
			errContains: "missing required permissions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()

			// Mock SelfSubjectAccessReview responses
			clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authv1.SelfSubjectAccessReview{
					Status: authv1.SubjectAccessReviewStatus{
						Allowed: tt.allowed,
						Reason:  "test reason",
					},
				}, nil
			})

			deployer := NewDeployer(clientset, Config{
				Namespace:          "gpu-operator",
				ServiceAccountName: "aicr",
				JobName:            "aicr",
			})

			ctx := context.Background()
			checks, err := deployer.CheckPermissions(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckPermissions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckPermissions() error = %v, should contain %q", err, tt.errContains)
				}
			}

			if !tt.wantErr && len(checks) == 0 {
				t.Error("CheckPermissions() returned no checks")
			}

			// Verify all checks match expected result
			for _, check := range checks {
				if check.Allowed != tt.allowed {
					t.Errorf("Check %s %s: got allowed=%v, want %v", check.Verb, check.Resource, check.Allowed, tt.allowed)
				}
			}
		})
	}
}

func TestCheckPermission(t *testing.T) {
	tests := []struct {
		name      string
		resource  string
		verb      string
		namespace string
		allowed   bool
		reason    string
	}{
		{
			name:      "allowed permission",
			resource:  "jobs",
			verb:      "create",
			namespace: "gpu-operator",
			allowed:   true,
			reason:    "user has permission",
		},
		{
			name:      "denied permission",
			resource:  "jobs",
			verb:      "create",
			namespace: "gpu-operator",
			allowed:   false,
			reason:    "user lacks permission",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()

			clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authv1.SelfSubjectAccessReview{
					Status: authv1.SubjectAccessReviewStatus{
						Allowed: tt.allowed,
						Reason:  tt.reason,
					},
				}, nil
			})

			deployer := NewDeployer(clientset, Config{
				Namespace: tt.namespace,
			})

			ctx := context.Background()
			allowed, reason, err := deployer.checkPermission(ctx, tt.resource, tt.verb, tt.namespace)

			if err != nil {
				t.Fatalf("checkPermission() error = %v", err)
			}

			if allowed != tt.allowed {
				t.Errorf("checkPermission() allowed = %v, want %v", allowed, tt.allowed)
			}

			if reason != tt.reason {
				t.Errorf("checkPermission() reason = %q, want %q", reason, tt.reason)
			}
		})
	}
}
