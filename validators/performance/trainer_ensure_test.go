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
	"sync/atomic"
	"testing"
	"time"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// TestEnsureTrainerInstalled_CompleteInstallIsLeftAlone verifies a healthy
// pre-existing Trainer is neither reinstalled nor claimed for cleanup: returning
// resources here would make the benchmark delete a Trainer it does not own.
func TestEnsureTrainerInstalled_CompleteInstallIsLeftAlone(t *testing.T) {
	client := newTrainerFakeClient(completeTrainerInstall()...)

	refs, err := ensureTrainerInstalled(context.Background(), client, nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0 (a Trainer we did not install must not be claimed for cleanup)", len(refs))
	}
}

// TestEnsureTrainerInstalled_WaitsOnDiscoveredControllerName pins the discovered
// controller name to the readiness wait. The probe locates the Deployment by label
// because the Helm chart derives its name from the release, so a chart install
// under a non-default release name is found under a name the self-install overlay
// never uses. Waiting on the fixed overlay name instead would poll a Deployment
// that does not exist, and waitForDeploymentReady treats NotFound as
// not-ready-yet: a healthy controller would be reported as never ready after the
// full timeout.
func TestEnsureTrainerInstalled_WaitsOnDiscoveredControllerName(t *testing.T) {
	const releaseDerivedName = "kft-custom-release-controller-manager"

	// A complete chart-style install in kubeflow whose controller carries a
	// release-derived name rather than the overlay's fixed one.
	objs := append(
		withoutObject(trainerInstallIn("kubeflow"), func(o runtime.Object) bool {
			u, ok := o.(*unstructured.Unstructured)
			return ok && u.GetKind() == "Deployment"
		}),
		readyTrainerDeploymentNamed("kubeflow", releaseDerivedName),
	)
	client := newTrainerFakeClient(objs...)

	// Fail fast instead of polling out the readiness timeout: a Get for any other
	// name means the discovered name never reached the wait.
	var polled []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetActionImpl)
		if !ok {
			return false, nil, nil
		}
		polled = append(polled, get.GetName())
		if get.GetName() != releaseDerivedName {
			cancel()
		}
		return false, nil, nil
	})

	refs, err := ensureTrainerInstalled(ctx, client, nil, false)
	if err != nil {
		t.Fatalf("unexpected error waiting on the discovered controller %q (polled %v): %v",
			releaseDerivedName, polled, err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0", len(refs))
	}
	// Without this the name assertion below is a range over an empty slice: if the
	// readiness wait ever stops issuing Deployment Gets (a switch to the watch
	// pattern used elsewhere in this file), the loop body would never run and this
	// guard would pass green while verifying nothing.
	if len(polled) == 0 {
		t.Fatal("readiness wait issued no Deployment Get; the name assertion below would be vacuous")
	}
	for _, name := range polled {
		if name != releaseDerivedName {
			t.Errorf("readiness wait polled %q, want the discovered name %q", name, releaseDerivedName)
		}
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

	refs, err := ensureTrainerInstalled(ctx, client, nil, false)
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

	refs, err := ensureTrainerInstalled(context.Background(), client, nil, false)
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

	_, err := ensureTrainerInstalled(context.Background(), client, nil, false)
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

// TestEnsureTrainerInstalled_RecipeDrivenLifecycle covers the decision the recipe
// now drives. The rows that matter are the two where the recipe declares the
// component: a delivered installation is used as-is, and a missing one fails rather
// than being installed over — which is the whole point, because self-installing
// there would report a passing benchmark for a cluster whose delivered Trainer is
// broken.
//
// The not-declared + complete row is included because the guard could silently
// break it: a recipe that never claimed a Trainer must still reuse one that happens
// to be present, exactly as before.
//
// The not-declared + missing row is deliberately absent rather than overlooked: it
// reaches installTrainer, which downloads and kustomize-builds the upstream release
// archive, so it is covered by e2e rather than being unit-testable here.
func TestEnsureTrainerInstalled_RecipeDrivenLifecycle(t *testing.T) {
	tests := []struct {
		name            string
		declared        bool
		objects         []runtime.Object
		wantErr         bool
		wantErrCode     aicrErrors.ErrorCode
		wantErrContains string
	}{
		{
			name:     "declared and delivered: used as-is, never claimed for cleanup",
			declared: true,
			objects:  completeTrainerInstall(),
		},
		{
			name:            "declared but missing: fails instead of installing over it",
			declared:        true,
			objects:         nil,
			wantErr:         true,
			wantErrCode:     aicrErrors.ErrCodeNotFound,
			wantErrContains: kubeflowTrainerComponent,
		},
		{
			name:     "not declared but present: reused, as before",
			declared: false,
			objects:  completeTrainerInstall(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer withShortTrainerWait(t)()
			client := newTrainerFakeClient(tt.objects...)

			refs, err := ensureTrainerInstalled(context.Background(), client, nil, tt.declared)

			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			// Nothing is ever claimed for cleanup on these rows: a Trainer the
			// benchmark did not install must not be deleted by it.
			if len(refs) != 0 {
				t.Errorf("refs = %d, want 0", len(refs))
			}
			if !tt.wantErr {
				return
			}
			// NotFound, not Unavailable: the read succeeded and the answer was "not
			// deployed". Unavailable is this package's code for a transport failure
			// (see the decision table on validators.Require), and filing a product
			// defect under it tells whoever triages the failure to re-run rather than
			// to fix their deployment.
			if !stderrors.Is(err, aicrErrors.New(tt.wantErrCode, "")) {
				t.Errorf("error code = %v, want %s", err, tt.wantErrCode)
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Errorf("error %q does not name %q, so an operator cannot tell which "+
					"component failed to deploy", err, tt.wantErrContains)
			}
		})
	}
}

// withShortTrainerWait shrinks the recipe-declared rollout wait for the duration of
// a test and restores it afterwards.
func withShortTrainerWait(t *testing.T) func() {
	t.Helper()
	oldTimeout, oldInterval := trainerInstallWaitTimeout, trainerInstallPollInterval
	trainerInstallWaitTimeout = 20 * time.Millisecond
	trainerInstallPollInterval = time.Millisecond
	return func() {
		trainerInstallWaitTimeout, trainerInstallPollInterval = oldTimeout, oldInterval
	}
}

// TestWaitForDeclaredTrainer_CanceledRunIsNotADeploymentDefect pins the distinction
// the poll deadline and parent cancellation would otherwise blur.
//
// Both expire the poll context, but they mean opposite things: the deadline means
// the delivered Trainer never became complete, which is the customer's deployment
// defect this PR exists to surface; cancellation means the run was aborted — a
// catalog timeout, a canceled phase, a killed Job — which is not. Reporting the
// second as the first is the same misclassification that made ErrCodeUnavailable
// wrong for the deadline case.
func TestWaitForDeclaredTrainer_CanceledRunIsNotADeploymentDefect(t *testing.T) {
	defer withShortTrainerWait(t)()
	client := newTrainerFakeClient()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitForDeclaredTrainer(ctx, client)
	if err == nil {
		t.Fatal("expected an error when the run is canceled")
	}
	if stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeNotFound, "")) {
		t.Errorf("canceled run reported as NotFound (%v); an aborted run is not a "+
			"failed deployment and must not be filed as one", err)
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeTimeout, "")) {
		t.Errorf("error code = %v, want ErrCodeTimeout", err)
	}
}

// TestEnsureTrainerInstalled_DeclaredRolloutDoesNotFallThrough pins the branch the
// fall-through actually lived in.
//
// A table row seeded with completeTrainerInstall() cannot reach it: the first probe
// in ensureTrainerInstalled succeeds, so waitForDeclaredTrainer is never entered and
// the row exercises the same path as the one above it. The supported rollout state —
// initially incomplete, complete on a later poll — is the one that enters the wait
// and then must return rather than continuing into the install path.
func TestEnsureTrainerInstalled_DeclaredRolloutDoesNotFallThrough(t *testing.T) {
	defer withShortTrainerWait(t)()

	client := newTrainerFakeClient()
	var probes int32
	// Report incomplete on the first probe and complete afterwards, which is what a
	// chart still landing looks like. The reactor answers only the first read the
	// probe makes; a NotFound there is enough to make the whole probe incomplete.
	client.PrependReactor("get", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			if atomic.AddInt32(&probes, 1) == 1 {
				return true, nil, apierrors.NewNotFound(
					schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
					"trainjobs.trainer.kubeflow.org")
			}
			return false, nil, nil // fall through to the tracker
		})
	for _, obj := range completeTrainerInstall() {
		if err := client.Tracker().Add(obj); err != nil {
			t.Fatalf("seeding fake: %v", err)
		}
	}

	// A nil discovery client makes installTrainer panic or error immediately, so if
	// the code falls through to the install path this test fails loudly rather than
	// silently passing.
	refs, err := ensureTrainerInstalled(context.Background(), client, nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0: a Trainer the recipe delivered must never be "+
			"claimed for cleanup, and a non-empty result means the install path ran", len(refs))
	}
	if atomic.LoadInt32(&probes) < 2 {
		t.Errorf("probes = %d, want >= 2: the wait was never entered, so this test "+
			"does not cover the branch it claims to", probes)
	}
}
