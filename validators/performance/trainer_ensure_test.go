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
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestEnsureTrainerInstalled_CompleteInstallIsLeftAlone verifies a healthy
// pre-existing Trainer is neither reinstalled nor claimed for cleanup: returning
// resources here would make the benchmark delete a Trainer it does not own.
func TestEnsureTrainerInstalled_CompleteInstallIsLeftAlone(t *testing.T) {
	client := newTrainerFakeClient(completeTrainerInstall()...)

	refs, err := ensureTrainerInstalled(context.Background(), client, fake.NewClientset(), nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0 (a Trainer we did not install must not be claimed for cleanup)", len(refs))
	}
}

// TestEnsureTrainerInstalled_WaitsOnDiscoveredControllerName pins the discovered
// controller name to the readiness wait. The probe locates the Deployment by label
// because a pre-existing chart installation can use a custom name even though the
// in-tree values pin fullnameOverride. Waiting on the fixed self-install name would
// poll a Deployment that does not exist, and waitForDeploymentReady treats NotFound
// as not-ready-yet: a healthy controller would be reported as never ready after the
// full timeout.
func TestEnsureTrainerInstalled_WaitsOnDiscoveredControllerName(t *testing.T) {
	const discoveredName = "kft-custom-release-controller-manager"

	// A complete chart-style install in kubeflow whose controller carries a custom
	// name rather than the self-install overlay's fixed one.
	objs := append(
		withoutObject(trainerInstallIn("kubeflow"), func(o runtime.Object) bool {
			u, ok := o.(*unstructured.Unstructured)
			return ok && u.GetKind() == "Deployment"
		}),
		readyTrainerDeploymentNamed("kubeflow", discoveredName),
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
		if get.GetName() != discoveredName {
			cancel()
		}
		return false, nil, nil
	})

	refs, err := ensureTrainerInstalled(ctx, client, fake.NewClientset(), nil, false)
	if err != nil {
		t.Fatalf("unexpected error waiting on the discovered controller %q (polled %v): %v",
			discoveredName, polled, err)
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
		if name != discoveredName {
			t.Errorf("readiness wait polled %q, want the discovered name %q", name, discoveredName)
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

	refs, err := ensureTrainerInstalled(ctx, client, fake.NewClientset(), nil, false)
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

	refs, err := ensureTrainerInstalled(context.Background(), client, fake.NewClientset(), nil, false)
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

	_, err := ensureTrainerInstalled(context.Background(), client, fake.NewClientset(), nil, false)
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
			got := foldCleanupError(tt.bench, tt.cleanup, "NCCL benchmark succeeded but Kubeflow Trainer cleanup failed")
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

	got := foldCleanupError(nil, cleanupErr, "fallback message")
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

			refs, err := ensureTrainerInstalled(context.Background(), client, fake.NewClientset(), nil, tt.declared)

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
	trainerInstallWaitTimeout = 200 * time.Millisecond
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
// catalog timeout, a canceled phase, a killed Job — which is not.
//
// The cancellation has to land *after* a probe completes, not before. Canceling up
// front is caught by getTrainerObject's own ctx.Err() check on its first read, which
// returns a "canceled before checking" Timeout through the err path — so the select,
// and the guard being tested, are never reached. An earlier version of this test did
// exactly that and passed with the guard deleted.
//
// The reactor below makes it deterministic: it cancels and returns NotFound on the
// first CRD read, and a missing CRD makes isTrainerInstalled return immediately, so
// that probe is the only one and the next stop is the select.
func TestWaitForDeclaredTrainer_CanceledRunIsNotADeploymentDefect(t *testing.T) {
	defer withShortTrainerWait(t)()

	client := newTrainerFakeClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client.PrependReactor("get", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			cancel()
			return true, nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
				"trainjobs.trainer.kubeflow.org")
		})

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
	// Witness that the guard ran rather than getTrainerObject's own pre-read check.
	// Without this the test passes on the probe-error path and would not notice the
	// guard being removed — which is how the earlier version of it was vacuous.
	if strings.Contains(err.Error(), "canceled before checking") {
		t.Errorf("error %q came from getTrainerObject's pre-read check, not the "+
			"cancellation guard in the select; this test is not exercising the guard", err)
	}
	if !strings.Contains(err.Error(), "canceled while waiting") {
		t.Errorf("error %q is not the cancellation guard's message", err)
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

	// A nil discovery client is not what catches a fall-through here: installTrainer
	// fetches the release archive from GitHub before it touches discovery, so on a
	// machine with egress a fall-through would download tens of megabytes first. The
	// assertions below are what catch it — no error, no claimed resources, and a
	// probe count proving the wait was entered.
	refs, err := ensureTrainerInstalled(context.Background(), client, fake.NewClientset(), nil, true)
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

// TestWaitForDeclaredTrainer_SlowProbeCannotOutrunTheDeadline pins the rollout
// allowance as a bound on the whole wait, not just on the sleeps between probes.
//
// An earlier revision probed with the parent context so a deadline landing
// mid-probe would not surface as a bare timeout. That fixed the classification and
// removed the bound: each probe makes several sequential reads with their own
// timeouts, so a slow probe could cross the allowance and still report success if
// the installation happened to complete meanwhile. The probe runs under pollCtx
// again, with the expiry classified rather than propagated blind.
//
// Here the first read sleeps past the allowance and the installation is complete
// underneath, so a wait that respects its deadline must fail rather than succeed.
func TestWaitForDeclaredTrainer_SlowProbeCannotOutrunTheDeadline(t *testing.T) {
	oldTimeout, oldInterval := trainerInstallWaitTimeout, trainerInstallPollInterval
	trainerInstallWaitTimeout = 20 * time.Millisecond
	trainerInstallPollInterval = time.Millisecond
	defer func() {
		trainerInstallWaitTimeout, trainerInstallPollInterval = oldTimeout, oldInterval
	}()

	client := newTrainerFakeClient(completeTrainerInstall()...)
	client.PrependReactor("get", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			time.Sleep(60 * time.Millisecond) // outlives the allowance
			return false, nil, nil            // then let the complete install answer
		})

	_, err := waitForDeclaredTrainer(context.Background(), client)
	if err == nil {
		t.Fatal("wait returned success after its own deadline had passed; the rollout " +
			"allowance must bound the probe, not only the sleeps between probes")
	}
	// Timeout, not NotFound. The allowance is still enforced — the wait fails — but
	// nothing here was ever read as missing: the installation underneath is complete
	// and the only thing that went wrong is a read slower than the budget. Filing
	// that as NotFound would send an operator to fix a deployment that is fine.
	if stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeNotFound, "")) {
		t.Errorf("slow read reported as NotFound (%v); no probe observed anything "+
			"incomplete, so there is no deployment defect to file", err)
	}
	var se *aicrErrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrErrors.ErrCodeTimeout {
		t.Errorf("reported code = %v, want Timeout", err)
	}
}

// TestWaitForDeclaredTrainer_TransportErrorKeepsItsClassification pins the narrow
// case where the rollout deadline and a real read failure land together.
//
// A read that starts before the deadline and fails for a genuine reason after it —
// an apiserver 503 — must keep its transport classification. Reporting it as "the
// installation did not become complete" would file a control-plane outage as a
// customer deployment defect, and it is precisely when the apiserver is degraded
// that the transport signal is worth the most.
//
// Expiry may win only for context-derived errors, which is what the stderrors.Is
// guard on the probe-error branch enforces.
func TestWaitForDeclaredTrainer_TransportErrorKeepsItsClassification(t *testing.T) {
	oldTimeout, oldInterval := trainerInstallWaitTimeout, trainerInstallPollInterval
	trainerInstallWaitTimeout = 20 * time.Millisecond
	trainerInstallPollInterval = time.Millisecond
	defer func() {
		trainerInstallWaitTimeout, trainerInstallPollInterval = oldTimeout, oldInterval
	}()

	client := newTrainerFakeClient()
	client.PrependReactor("get", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			// Outlive the allowance, then fail for a real reason rather than a
			// context one: the deadline has passed by the time this returns.
			time.Sleep(40 * time.Millisecond)
			return true, nil, apierrors.NewServiceUnavailable("apiserver is having a bad day")
		})

	_, err := waitForDeclaredTrainer(context.Background(), client)
	if err == nil {
		t.Fatal("expected an error")
	}
	if stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeNotFound, "")) {
		t.Errorf("transport failure reported as NotFound (%v); a degraded control plane "+
			"is not a deployment that never completed, and swallowing that signal sends "+
			"the operator to fix the wrong thing", err)
	}
	// Assert the code a consumer actually reads. ExitCodeFromError resolves the
	// outermost StructuredError, so that is the value which reaches the exit status —
	// errors.Is would match Unavailable anywhere in the chain and would still pass if
	// the verdict were flattened to Internal, which is observable, not cosmetic.
	//
	// stderrors.As walks from the outermost and assigns the first match, which is the
	// same resolution ExitCodeFromError performs. The pattern is already used for this
	// purpose in pkg/chainsaw's tests.
	var se *aicrErrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrErrors.ErrCodeUnavailable {
		t.Errorf("reported code = %v, want Unavailable: a degraded control plane must not "+
			"reach the operator as a deployment that never completed", err)
	}
}

// TestWaitForDeclaredTrainer_LateSuccessDoesNotOutrunTheDeadline covers the other
// end of the probe from the slow-first-read case.
//
// Delaying the first read makes a later read notice expiry, so the err path catches
// it. Delaying the *last* read does not: the probe returns ok, and accepting that
// without rechecking would let a wait return success after its allowance had passed.
// The bound has to cover the answer, not only the question.
func TestWaitForDeclaredTrainer_LateSuccessDoesNotOutrunTheDeadline(t *testing.T) {
	oldTimeout, oldInterval := trainerInstallWaitTimeout, trainerInstallPollInterval
	trainerInstallWaitTimeout = 20 * time.Millisecond
	trainerInstallPollInterval = time.Millisecond
	defer func() {
		trainerInstallWaitTimeout, trainerInstallPollInterval = oldTimeout, oldInterval
	}()

	client := newTrainerFakeClient(completeTrainerInstall()...)
	// The Service read is the probe's last step, so stalling it past the allowance
	// makes the probe succeed with the deadline already gone.
	client.PrependReactor("get", "services",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			time.Sleep(60 * time.Millisecond)
			return false, nil, nil // then let the seeded complete install answer
		})

	_, err := waitForDeclaredTrainer(context.Background(), client)
	if err == nil {
		t.Fatal("wait returned success after its allowance had passed; expiry must be " +
			"rechecked before accepting a probe's result, not only before issuing it")
	}
	// The probe returned complete, so no incomplete installation was ever observed:
	// the verdict is the timeout that happened, not a deployment defect.
	if stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeNotFound, "")) {
		t.Errorf("late success reported as NotFound (%v); the probe found the "+
			"installation complete, so nothing was missing", err)
	}
	var se *aicrErrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrErrors.ErrCodeTimeout {
		t.Errorf("reported code = %v, want Timeout", err)
	}
	// The reason must describe this probe, not the previous one. The installation is
	// complete here, so blaming a missing object would point the operator at something
	// that is present; the finding is a rollout slower than its budget.
	if !strings.Contains(err.Error(), "only after the allowance expired") {
		t.Errorf("reason contradicts the probe that just succeeded: %v", err)
	}
	if strings.Contains(err.Error(), "no complete installation was found") {
		t.Errorf("reason claims nothing was found, but the probe returned complete: %v", err)
	}
}

// TestWaitForDeclaredTrainer_ExpiryNamesTheProbeThatJustRan pins the reason reported
// when the allowance expires on an *incomplete* probe.
//
// The recheck before accepting a result runs on every iteration, not only the
// successful ones. When the probe that just ran came back incomplete, its own
// finding — the object that is actually missing — is what the operator needs. An
// earlier revision carried the previous iteration's result into the diagnosis, so
// the first probe of a wait reported the zero value and the operator got a generic
// "no complete installation was found" instead of the missing CRD.
//
// Stalling the CRD read past the allowance and answering NotFound produces exactly
// that shape: one probe, incomplete, deadline already gone.
func TestWaitForDeclaredTrainer_ExpiryNamesTheProbeThatJustRan(t *testing.T) {
	oldTimeout, oldInterval := trainerInstallWaitTimeout, trainerInstallPollInterval
	trainerInstallWaitTimeout = 20 * time.Millisecond
	trainerInstallPollInterval = time.Millisecond
	defer func() {
		trainerInstallWaitTimeout, trainerInstallPollInterval = oldTimeout, oldInterval
	}()

	client := newTrainerFakeClient()
	client.PrependReactor("get", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			// Outlive the allowance, then answer NotFound. A missing CRD short-circuits
			// the probe, so it returns incomplete with no error and the recheck below
			// is the next thing that runs.
			time.Sleep(60 * time.Millisecond)
			return true, nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
				"trainjobs.trainer.kubeflow.org")
		})

	_, err := waitForDeclaredTrainer(context.Background(), client)
	if err == nil {
		t.Fatal("expected an error once the allowance expired with the installation incomplete")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeNotFound, "")) {
		t.Errorf("error code = %v, want ErrCodeNotFound: the deadline expired locally "+
			"with the parent still live", err)
	}
	if !strings.Contains(err.Error(), "CRD trainjobs.trainer.kubeflow.org is missing") {
		t.Errorf("expiry did not name the object the probe just found missing: %v", err)
	}
	if strings.Contains(err.Error(), "no complete installation was found") {
		t.Errorf("expiry fell back to the generic reason even though the probe that just "+
			"ran reported a specific missing object: %v", err)
	}
}

// TestWaitForDeclaredTrainer_LateSuccessClaimsOnlyWhatWasObserved pins the wording of
// the late-success reason to what the code can actually know.
//
// The recheck learns that the probe returned complete and that the deadline has
// already passed. It cannot know *when* the installation became complete — only when
// it was observed to be. Claiming it "became complete after the allowance expired"
// asserts a transition time nobody measured, and would mislead an operator whose
// Trainer was healthy all along behind a slow apiserver.
func TestWaitForDeclaredTrainer_LateSuccessClaimsOnlyWhatWasObserved(t *testing.T) {
	oldTimeout, oldInterval := trainerInstallWaitTimeout, trainerInstallPollInterval
	trainerInstallWaitTimeout = 20 * time.Millisecond
	trainerInstallPollInterval = time.Millisecond
	defer func() {
		trainerInstallWaitTimeout, trainerInstallPollInterval = oldTimeout, oldInterval
	}()

	client := newTrainerFakeClient(completeTrainerInstall()...)
	client.PrependReactor("get", "services",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			time.Sleep(60 * time.Millisecond)
			return false, nil, nil
		})

	_, err := waitForDeclaredTrainer(context.Background(), client)
	if err == nil {
		t.Fatal("expected an error once the allowance expired")
	}
	if strings.Contains(err.Error(), "became complete") {
		t.Errorf("reason asserts when the installation became complete, which the wait "+
			"never measured; it only observed completeness after expiry: %v", err)
	}
	if !strings.Contains(err.Error(), "observed complete only after the allowance expired") {
		t.Errorf("reason does not state what was actually observed: %v", err)
	}
}

// TestWaitForDeclaredTrainer_ReadTimeoutsAloneAreNotADeploymentDefect pins the case
// where the allowance runs out without any probe ever reaching a conclusion.
//
// Every read the probe issues is cut short by the poll deadline, so no probe ever
// observes an incomplete installation — there is nothing on the cluster the wait can
// point at. Synthesizing NotFound from that empty observation would file a degraded
// or slow apiserver as a customer deployment defect, which is the same
// misclassification the NotFound/Unavailable split exists to prevent. With no
// observation to stand on, the honest verdict is the timeout that actually happened.
func TestWaitForDeclaredTrainer_ReadTimeoutsAloneAreNotADeploymentDefect(t *testing.T) {
	oldTimeout, oldInterval := trainerInstallWaitTimeout, trainerInstallPollInterval
	trainerInstallWaitTimeout = 20 * time.Millisecond
	trainerInstallPollInterval = time.Millisecond
	defer func() {
		trainerInstallWaitTimeout, trainerInstallPollInterval = oldTimeout, oldInterval
	}()

	// Seed only the first CRD the probe reads, and stall that read past the
	// allowance. The read itself succeeds, so the probe learns nothing incomplete;
	// the deadline is already gone by the time the second read is issued, and
	// getTrainerObject's pre-read check turns it into a Timeout. That is the only
	// thing the wait ever hears back.
	client := newTrainerFakeClient(establishedCRD(trainerCRDTrainJobs))
	client.PrependReactor("get", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			time.Sleep(60 * time.Millisecond) // outlives the allowance
			return false, nil, nil            // then let the tracker answer
		})

	_, err := waitForDeclaredTrainer(context.Background(), client)
	if err == nil {
		t.Fatal("wait returned success after its allowance had passed")
	}
	if stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeNotFound, "")) {
		t.Errorf("read timeouts reported as NotFound (%v); no probe ever observed an "+
			"incomplete installation, so there is no deployment defect to file", err)
	}
	var se *aicrErrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrErrors.ErrCodeTimeout {
		t.Errorf("reported code = %v, want Timeout", err)
	}
	if strings.Contains(err.Error(), "no complete installation was found") {
		t.Errorf("verdict claims nothing was found, but nothing was ever read: %v", err)
	}
}
