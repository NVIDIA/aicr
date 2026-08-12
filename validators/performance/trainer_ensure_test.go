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
	stderrors "errors"
	"strings"
	"testing"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// TestEnsureTrainerInstalled_CompleteInstallIsLeftAlone verifies a healthy
// pre-existing Trainer is neither reinstalled nor claimed for cleanup: returning
// resources here would make the benchmark delete a Trainer it does not own.
func TestEnsureTrainerInstalled_CompleteInstallIsLeftAlone(t *testing.T) {
	client := newTrainerFakeClient(completeTrainerInstall()...)

	refs, err := ensureTrainerInstalled(context.Background(), client, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0 (a Trainer we did not install must not be claimed for cleanup)", len(refs))
	}
}

// TestEnsureTrainerInstalled_WaitsForPreexistingController covers the
// already-installed branch: the probe checks presence, not readiness, so a
// still-rolling controller must be waited out and reported distinctly rather
// than driving TrainJobs at a controller with no webhook endpoints.
func TestEnsureTrainerInstalled_WaitsForPreexistingController(t *testing.T) {
	// Replace the ready controller with one reporting zero ready replicas. Drop by
	// kind, not name: upstream gives the Deployment and its Service the same name.
	objs := append(
		withoutObject(completeTrainerInstall(), func(o runtime.Object) bool {
			u, ok := o.(*unstructured.Unstructured)
			return ok && u.GetKind() == "Deployment"
		}),
		notReadyTrainerDeployment(),
	)
	client := newTrainerFakeClient(objs...)

	ctx, cancel := context.WithCancel(context.Background())
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel() // the controller never becomes ready; stop the poll deterministically
		return false, nil, nil
	})
	defer cancel()

	refs, err := ensureTrainerInstalled(ctx, client, nil)
	if err == nil {
		t.Fatal("expected a not-ready pre-existing controller to fail, got nil error")
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0", len(refs))
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeTimeout, "")) {
		t.Errorf("error code is not Timeout: %v", err)
	}
}

// TestEnsureTrainerInstalled_RefusesToInstallOverForeignNamespace is the guard for
// the destructive case: an incomplete Trainer installed elsewhere (a partial chart
// install, a mid-upgrade, a non-default release name) must abort the install rather
// than apply on top of it.
//
// Kustomize applies CRDs and RBAC before webhook configurations, so the per-object
// ownership guard fires too late — a shared-name ClusterRoleBinding would already
// have been repointed at our namespace, and updates are excluded from the rollback
// set, so nothing restores it. Refusing before the first apply is the only point
// where this is still reversible.
func TestEnsureTrainerInstalled_RefusesToInstallOverForeignNamespace(t *testing.T) {
	// A chart installation in kubeflow, incomplete enough that the probe rejects it
	// (no CRDs), but discoverable through its admission configuration.
	client := newTrainerFakeClient(
		webhookConfigIn("ValidatingWebhookConfiguration", trainerValidatingWebhookConfig,
			trainerValidatingWebhookName, "kubeflow"),
	)

	refs, err := ensureTrainerInstalled(context.Background(), client, nil)
	if err == nil {
		t.Fatal("expected the installer to refuse installing over an installation in another namespace")
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0", len(refs))
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
		t.Errorf("error code is not Conflict: %v", err)
	}
	if !strings.Contains(err.Error(), "kubeflow") {
		t.Errorf("error does not name the live installation's namespace: %v", err)
	}
}

// TestEnsureTrainerInstalled_PreservesProbeErrorCode is the regression guard for
// the classification the probe added: re-wrapping with a hardcoded Internal here
// would report a transient control-plane outage to the verdict as a product defect.
func TestEnsureTrainerInstalled_PreservesProbeErrorCode(t *testing.T) {
	client := newTrainerFakeClient(completeTrainerInstall()...)
	client.PrependReactor("get", resourceCRDs, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	_, err := ensureTrainerInstalled(context.Background(), client, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeUnavailable, "")) {
		t.Errorf("probe classification was overwritten; want Unavailable, got: %v", err)
	}
}

// TestFoldCleanupError pins the fail-closed teardown contract: a cleanup failure
// fails an otherwise-passing check (leaked cluster-scoped objects poison the next
// run), but never masks a real benchmark failure.
func TestFoldCleanupError(t *testing.T) {
	benchErr := aicrErrors.New(aicrErrors.ErrCodeTimeout, "launcher pod never completed")
	cleanupErr := aicrErrors.New(aicrErrors.ErrCodeUnavailable, "failed to delete 1 Trainer resource(s)")

	tests := []struct {
		name    string
		bench   error
		cleanup error
		want    error
	}{
		{name: "clean run reports success", bench: nil, cleanup: nil, want: nil},
		{name: "cleanup failure fails a passing benchmark", bench: nil, cleanup: cleanupErr, want: cleanupErr},
		{name: "benchmark failure outranks cleanup failure", bench: benchErr, cleanup: cleanupErr, want: benchErr},
		{name: "benchmark failure survives clean teardown", bench: benchErr, cleanup: nil, want: benchErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := foldCleanupError(tt.bench, tt.cleanup)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if !stderrors.Is(got, tt.want) {
				t.Errorf("got %v, want it to wrap %v", got, tt.want)
			}
		})
	}
}

// TestFoldCleanupError_PreservesCleanupCode verifies a transient teardown blip
// stays retryable rather than being flattened to an internal fault.
func TestFoldCleanupError_PreservesCleanupCode(t *testing.T) {
	cleanupErr := aicrErrors.New(aicrErrors.ErrCodeUnavailable, "apiserver is down")

	got := foldCleanupError(nil, cleanupErr)
	if !stderrors.Is(got, aicrErrors.New(aicrErrors.ErrCodeUnavailable, "")) {
		t.Errorf("cleanup error code was flattened: %v", got)
	}
}
