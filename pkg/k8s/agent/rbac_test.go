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
	"bytes"
	"context"
	stderrors "errors"
	"log/slog"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// captureLogs redirects the default slog logger into a buffer at debug level
// for the duration of the test and returns the buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

// TestEnsureServiceAccount_AdoptionDriftCheck covers every branch of the
// bare-name Get that warns when a ServiceAccount already exists under the
// unscoped prefix (ADR-020's adoption-drift warning).
//
// The Forbidden branch is the load-bearing one: `serviceaccounts get` is NOT
// in CheckPermissions' requiredChecks, so an identity holding exactly the
// pre-flight verb set must still be able to deploy. That Get is a diagnostic
// courtesy and must never gate the deployment.
func TestEnsureServiceAccount_AdoptionDriftCheck(t *testing.T) {
	saGR := schema.GroupResource{Group: "", Resource: "serviceaccounts"}
	saGVR := corev1.SchemeGroupVersion.WithResource("serviceaccounts")

	tests := []struct {
		name string
		// getErr, when non-nil, is returned by every ServiceAccount Get.
		getErr error
		// existingBareSA pre-creates a ServiceAccount under the unscoped
		// prefix name.
		existingBareSA bool
		wantErr        bool
		wantCreated    bool
		wantLogSubstr  string
		notWantLog     string
	}{
		{
			name:           "pre-existing unscoped ServiceAccount warns but still creates the run-scoped one",
			existingBareSA: true,
			wantCreated:    true,
			wantLogSubstr:  "already exists under the unscoped name",
		},
		{
			name:        "no pre-existing ServiceAccount is the silent normal path",
			wantCreated: true,
			notWantLog:  "already exists under the unscoped name",
		},
		{
			name:          "forbidden Get does not block the deployment",
			getErr:        apierrors.NewForbidden(saGR, testName, stderrors.New("no get permission")),
			wantCreated:   true,
			wantLogSubstr: "skipping adoption-drift check",
			notWantLog:    "already exists under the unscoped name",
		},
		{
			name:        "unexpected Get error fails closed",
			getErr:      apierrors.NewInternalError(stderrors.New("apiserver exploded")),
			wantErr:     true,
			wantCreated: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			buf := captureLogs(t)

			clientset := fake.NewClientset()
			if tt.existingBareSA {
				if _, err := clientset.CoreV1().ServiceAccounts("test-ns").Create(ctx, &corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{
						Name:        testName,
						Namespace:   "test-ns",
						Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/example"},
					},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatalf("seeding unscoped ServiceAccount: %v", err)
				}
			}
			if tt.getErr != nil {
				clientset.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}

			d := NewDeployer(clientset, Config{Namespace: "test-ns", RunID: testRunID})
			err := d.ensureServiceAccount(ctx)

			if (err != nil) != tt.wantErr {
				t.Fatalf("ensureServiceAccount() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
				t.Errorf("error = %v, want ErrCodeInternal", err)
			}

			// The run-scoped ServiceAccount is what the Job actually binds
			// to; a suppressed diagnostic must not suppress its creation.
			// Read through the tracker, not the clientset: the reactor above
			// stands in for an identity that cannot read ServiceAccounts at
			// all, and that must not also blind the assertions.
			scoped := testName + "-" + testRunID
			_, getErr := clientset.Tracker().Get(saGVR, "test-ns", scoped)
			if gotCreated := getErr == nil; gotCreated != tt.wantCreated {
				t.Errorf("run-scoped ServiceAccount %q created = %v (err %v), want %v", scoped, gotCreated, getErr, tt.wantCreated)
			}
			if tt.wantCreated && !d.hasCreated(kindServiceAccount) {
				t.Error("created-set has no ServiceAccount entry; Cleanup would not delete it")
			}

			if tt.existingBareSA {
				// Adoption is exactly what this release stopped doing: the
				// out-of-band ServiceAccount must be left untouched.
				obj, bareErr := clientset.Tracker().Get(saGVR, "test-ns", testName)
				if bareErr != nil {
					t.Fatalf("unscoped ServiceAccount disappeared: %v", bareErr)
				}
				bare, ok := obj.(*corev1.ServiceAccount)
				if !ok {
					t.Fatalf("tracker returned %T, want *corev1.ServiceAccount", obj)
				}
				if bare.Annotations["eks.amazonaws.com/role-arn"] == "" {
					t.Error("unscoped ServiceAccount annotations were modified")
				}
			}

			if tt.wantLogSubstr != "" && !strings.Contains(buf.String(), tt.wantLogSubstr) {
				t.Errorf("log = %q, want it to contain %q", buf.String(), tt.wantLogSubstr)
			}
			if tt.notWantLog != "" && strings.Contains(buf.String(), tt.notWantLog) {
				t.Errorf("log = %q, want it NOT to contain %q", buf.String(), tt.notWantLog)
			}
		})
	}
}

// TestDeploy_SucceedsWhenServiceAccountGetForbidden is the end-to-end shape of
// the same bug: an identity authorized for exactly CheckPermissions'
// requiredChecks (which do not include `serviceaccounts get`) passes the
// pre-flight, so Deploy must not then fail on the adoption-drift Get.
func TestDeploy_SucceedsWhenServiceAccountGetForbidden(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)

	clientset := fake.NewClientset()
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true, Reason: "pre-flight verb set granted"},
		}, nil
	})
	clientset.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "", Resource: "serviceaccounts"}, testName,
			stderrors.New(`User "snapshot-runner" cannot get resource "serviceaccounts"`))
	})

	d := NewDeployer(clientset, Config{
		Namespace: "test-ns",
		Image:     "aicr:test",
		RunID:     testRunID,
	})

	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v, want nil (the adoption-drift Get must not gate deployment)", err)
	}

	if _, err := clientset.BatchV1().Jobs("test-ns").Get(ctx, "aicr-"+testRunID, metav1.GetOptions{}); err != nil {
		t.Errorf("Job not created: %v", err)
	}
}
