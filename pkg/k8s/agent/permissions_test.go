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

package agent

import (
	"context"
	"fmt"
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

// TestCheckPermissions_ConfigMapDeleteGatedOnOwnership pins the gate on the
// `configmaps: delete` verb. CheckPermissions fails closed, so an
// unconditional entry would make Deploy return ErrCodeUnauthorized at Step 0
// for a caller who supplied their own `cm://` output URI — a run that never
// deletes a ConfigMap at all, because both Cleanup's staging sweep and
// getSnapshotFromConfigMap's created-set record are gated on
// Config.OwnsOutputConfigMap.
func TestCheckPermissions_ConfigMapDeleteGatedOnOwnership(t *testing.T) {
	tests := []struct {
		name              string
		ownsOutput        bool
		wantCMDeleteCheck bool
		wantErr           bool
	}{
		{
			name:              "owns output ConfigMap requires configmaps delete",
			ownsOutput:        true,
			wantCMDeleteCheck: true,
			// The identity below is denied exactly `configmaps: delete`,
			// so a required check makes the whole pre-flight fail.
			wantErr: true,
		},
		{
			name:              "caller-supplied output ConfigMap does not require configmaps delete",
			ownsOutput:        false,
			wantCMDeleteCheck: false,
			wantErr:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()

			// Deny only `configmaps: delete`; allow everything else. This
			// models the least-privilege identity the gate exists for.
			//
			// CheckPermissions fans the checks out over an errgroup, so this
			// reactor runs on worker goroutines, not the test goroutine.
			// t.Fatalf there would Goexit only the worker and leave the
			// errgroup waiting on a goroutine that never returns a value;
			// report with t.Errorf and hand the failure back as the
			// reactor's error so the call under test terminates.
			clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create, ok := action.(k8stesting.CreateAction)
				if !ok {
					reactorErr := fmt.Errorf("action %T is not a CreateAction", action)
					t.Error(reactorErr)
					return true, nil, reactorErr
				}
				review, ok := create.GetObject().(*authv1.SelfSubjectAccessReview)
				if !ok {
					reactorErr := fmt.Errorf("object %T is not a SelfSubjectAccessReview", create.GetObject())
					t.Error(reactorErr)
					return true, nil, reactorErr
				}
				attrs := review.Spec.ResourceAttributes
				allowed := attrs.Resource != resourceCM || attrs.Verb != verbDelete
				return true, &authv1.SelfSubjectAccessReview{
					Status: authv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: "test reason"},
				}, nil
			})

			deployer := NewDeployer(clientset, Config{
				Namespace:           "gpu-operator",
				RunID:               testRunID,
				OwnsOutputConfigMap: tt.ownsOutput,
			})

			checks, err := deployer.CheckPermissions(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckPermissions() error = %v, wantErr %v", err, tt.wantErr)
			}

			gotCMDelete := false
			for _, c := range checks {
				if c.Resource == resourceCM && c.Verb == verbDelete {
					gotCMDelete = true
				}
			}
			if gotCMDelete != tt.wantCMDeleteCheck {
				t.Errorf("configmaps delete check present = %v, want %v", gotCMDelete, tt.wantCMDeleteCheck)
			}
		})
	}
}
