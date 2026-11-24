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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/validator/labels"
	"aicr.run/validators"
)

var namespacesResource = schema.GroupResource{Resource: "namespaces"}

func testProbeNamespace(t *testing.T) *probeNamespace {
	t.Helper()
	ns, err := newProbeNamespace()
	if err != nil {
		t.Fatalf("newProbeNamespace() error = %v", err)
	}
	return ns
}

func TestNewProbeNamespace(t *testing.T) {
	t.Parallel()
	a, b := testProbeNamespace(t), testProbeNamespace(t)
	if a.token == b.token {
		t.Errorf("two runs drew the same token %s", a.token)
	}
	if len(a.token) != 32 || a.name != defaults.GPUTestNamespacePrefix+a.token {
		t.Errorf("namespace %q with token %q, want %s plus a 32-hex token (the shape aicr sweep accepts)",
			a.name, a.token, defaults.GPUTestNamespacePrefix)
	}
}

func TestProbeNamespaceLabels(t *testing.T) {
	tests := []struct {
		name      string
		runID     string
		wantRunID string
	}{
		{name: "no run ID", runID: ""},
		{name: "valid run ID", runID: "20261123-abc", wantRunID: "20261123-abc"},
		{name: "invalid run ID is dropped", runID: "not a label value!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(runIDEnv, tt.runID)
			ns := &probeNamespace{token: "tok"}
			got := ns.labels()
			if got[labels.GPUTestRun] != "tok" {
				t.Errorf("labels = %v, want %s=tok", got, labels.GPUTestRun)
			}
			if got[labels.RunID] != tt.wantRunID {
				t.Errorf("labels[%s] = %q, want %q", labels.RunID, got[labels.RunID], tt.wantRunID)
			}
		})
	}
}

// TestProbeNamespaceCreateAndDelete covers the ownership outcomes: a clean
// create is deleted; an existing namespace with this run's token is adopted
// and deleted; a foreign one is refused and never touched; a definitive
// rejection leaves nothing to delete; an ambiguous create that persisted is
// found by token and deleted, and one that did not persist leaves nothing.
func TestProbeNamespaceCreateAndDelete(t *testing.T) {
	t.Parallel()

	const token = "0123456789abcdef0123456789abcdef"
	name := defaults.GPUTestNamespacePrefix + token
	existing := func(owner string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{labels.GPUTestRun: owner}}}
	}

	tests := []struct {
		name          string
		objects       []runtime.Object
		createErr     error
		persistOnFail bool
		wantCode      errors.ErrorCode
		wantRemaining bool
	}{
		{name: "clean create"},
		{name: "own namespace adopted", objects: []runtime.Object{existing(token)}},
		{name: "foreign namespace refused and kept", objects: []runtime.Object{existing("someone-else")},
			wantCode: errors.ErrCodeConflict, wantRemaining: true},
		{name: "forbidden create leaves nothing", createErr: k8serrors.NewForbidden(namespacesResource, name, nil),
			wantCode: errors.ErrCodeInternal},
		{name: "ambiguous create that persisted is deleted", createErr: k8serrors.NewTimeoutError("slow", 1),
			persistOnFail: true, wantCode: errors.ErrCodeInternal},
		{name: "ambiguous create that did not persist leaves nothing", createErr: k8serrors.NewTimeoutError("slow", 1),
			wantCode: errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := k8sfake.NewClientset(tt.objects...)
			if tt.createErr != nil {
				client.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
					if tt.persistOnFail {
						obj := action.(k8stesting.CreateAction).GetObject()
						if err := client.Tracker().Add(obj); err != nil {
							return true, nil, err
						}
					}
					return true, nil, tt.createErr
				})
			}
			ns := &probeNamespace{token: token, name: name}

			err := ns.create(context.Background(), client)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("create() error = %v", err)
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantCode)
			}

			if derr := ns.delete(client); derr != nil {
				t.Fatalf("delete() error = %v", derr)
			}
			_, getErr := client.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
			if remaining := getErr == nil; remaining != tt.wantRemaining {
				t.Errorf("namespace remaining = %v (get error %v), want %v", remaining, getErr, tt.wantRemaining)
			}
		})
	}
}

// TestProbeNamespaceDeleteIsUIDGuarded proves the delete names the UID the
// create observed, so it can never remove a namespace recreated at the same
// name.
func TestProbeNamespaceDeleteIsUIDGuarded(t *testing.T) {
	t.Parallel()
	const uid = types.UID("created-uid")
	client := k8sfake.NewClientset()
	client.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace)
		if !ok {
			return false, nil, nil
		}
		created := ns.DeepCopy()
		created.UID = uid
		if err := client.Tracker().Add(created); err != nil {
			return true, nil, err
		}
		return true, created, nil
	})
	var gotUID *types.UID
	client.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if pre := action.(k8stesting.DeleteAction).GetDeleteOptions().Preconditions; pre != nil {
			gotUID = pre.UID
		}
		return false, nil, nil
	})

	ns := testProbeNamespace(t)
	if err := ns.create(context.Background(), client); err != nil {
		t.Fatalf("create() error = %v", err)
	}
	if err := ns.delete(client); err != nil {
		t.Fatalf("delete() error = %v", err)
	}
	if gotUID == nil || *gotUID != uid {
		t.Errorf("delete precondition UID = %v, want %s", gotUID, uid)
	}
}

// TestProbeNamespaceAmbiguousCreateUnreadable proves an ambiguous create
// whose namespace cannot then be read is a cleanup failure, not "never
// created".
func TestProbeNamespaceAmbiguousCreateUnreadable(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset()
	client.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewTimeoutError("slow", 1)
	})
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewServiceUnavailable("apiserver restarting")
	})
	ns := testProbeNamespace(t)
	if err := ns.create(context.Background(), client); err == nil {
		t.Fatal("create() error = nil, want the timeout")
	}
	if err := ns.delete(client); err == nil || !strings.Contains(err.Error(), "CLEANUP FAILED") {
		t.Fatalf("delete() error = %v, want CLEANUP FAILED", err)
	}
}

func TestProbeNamespaceDeleteFailureIsReported(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewClientset()
	client.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(namespacesResource, "x", nil)
	})
	ns := testProbeNamespace(t)
	if err := ns.create(context.Background(), client); err != nil {
		t.Fatalf("create() error = %v", err)
	}
	if err := ns.delete(client); err == nil || !strings.Contains(err.Error(), "CLEANUP FAILED") {
		t.Fatalf("delete() error = %v, want CLEANUP FAILED", err)
	}
}

// TestProbeNamespaceCopiesImagePullSecrets proves a forwarded pull Secret is
// copied into the probe namespace and referenced by the probes, and a named
// Secret that does not exist fails the create.
func TestProbeNamespaceCopiesImagePullSecrets(t *testing.T) {
	t.Parallel()

	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mirror", Namespace: "aicr-validation"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}

	t.Run("copied", func(t *testing.T) {
		t.Parallel()
		client := k8sfake.NewClientset(source)
		ns := testProbeNamespace(t)
		ns.imagePullSecrets, ns.validatorNamespace = []string{"mirror"}, "aicr-validation"
		if err := ns.create(context.Background(), client); err != nil {
			t.Fatalf("create() error = %v", err)
		}
		copied, err := client.CoreV1().Secrets(ns.name).Get(context.Background(), "mirror", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("copied secret: %v", err)
		}
		if copied.Type != source.Type || string(copied.Data[corev1.DockerConfigJsonKey]) != `{"auths":{}}` ||
			copied.Labels[labels.GPUTestRun] != ns.token {

			t.Errorf("copied secret = %+v", copied)
		}
		if refs := ns.imagePullSecretRefs(); len(refs) != 1 || refs[0].Name != "mirror" {
			t.Errorf("imagePullSecretRefs() = %v", refs)
		}
	})

	t.Run("missing source fails", func(t *testing.T) {
		t.Parallel()
		client := k8sfake.NewClientset()
		ns := testProbeNamespace(t)
		ns.imagePullSecrets, ns.validatorNamespace = []string{"mirror"}, "aicr-validation"
		errorstest.WantReportedCode(t, ns.create(context.Background(), client), errors.ErrCodeNotFound)
	})
}

// TestProbeNamespaceCopiesEachPullSecretOnce proves a Secret named twice in
// the forwarded list is copied once, where a second copy would fail on
// AlreadyExists before any probe ran.
func TestProbeNamespaceCopiesEachPullSecretOnce(t *testing.T) {
	t.Setenv(validators.ImagePullSecretsEnv, "mirror, mirror,other,mirror")
	t.Setenv("AICR_NAMESPACE", "aicr-validation")
	ns, err := newProbeNamespace()
	if err != nil {
		t.Fatalf("newProbeNamespace() error = %v", err)
	}
	if got := strings.Join(ns.imagePullSecrets, ","); got != "mirror,other" {
		t.Fatalf("imagePullSecrets = %q, want mirror,other", got)
	}
	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.validatorNamespace}, Type: corev1.SecretTypeDockerConfigJson}
	}
	client := k8sfake.NewClientset(secret("mirror"), secret("other"))
	if err := ns.create(context.Background(), client); err != nil {
		t.Fatalf("create() error = %v", err)
	}
	if refs := ns.imagePullSecretRefs(); len(refs) != 2 {
		t.Errorf("imagePullSecretRefs() = %v, want one per distinct Secret", refs)
	}
}

// TestProbeNamespaceUnverifiedOwnershipIsResolvedAtDelete proves a create
// that met AlreadyExists and could not read the namespace leaves ownership
// open: delete reads it again, and deletes it when it carries this run's
// token.
func TestProbeNamespaceUnverifiedOwnershipIsResolvedAtDelete(t *testing.T) {
	t.Parallel()
	ns := testProbeNamespace(t)
	client := k8sfake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: ns.name, Labels: map[string]string{labels.GPUTestRun: ns.token},
	}})
	var gets int
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return true, nil, k8serrors.NewServiceUnavailable("apiserver restarting")
		}
		return false, nil, nil
	})

	if err := ns.create(context.Background(), client); err == nil {
		t.Fatal("create() error = nil, want the unverified ownership error")
	}
	if ns.foreign {
		t.Fatal("create() marked the namespace foreign after an ownership read failed")
	}
	if err := ns.delete(client); err != nil {
		t.Fatalf("delete() error = %v", err)
	}
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), ns.name, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("namespace still present after delete (get error %v)", err)
	}
}
