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

package validator

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"github.com/NVIDIA/aicr/pkg/validator/job"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// validatorFieldManager identifies AICR's server-side-apply writes against the
// validator namespace. Matches the value used in pkg/validator/job/rbac.go so
// namespace, RBAC, and Job objects share a single conflict domain.
const validatorFieldManager = "aicr"

// checkReadiness evaluates top-level validation constraints against the snapshot.
// Returns an error if any constraint fails, nil if all pass or no constraints exist.
func checkReadiness(validationInput *v1.ValidationInput, snap *snapshotter.Snapshot) error {
	if validationInput == nil || snap == nil || len(validationInput.Constraints) == 0 {
		return nil
	}

	slog.Info("readiness pre-flight", "constraints", len(validationInput.Constraints))

	for _, c := range validationInput.Constraints {
		result := constraints.Evaluate(c, snap)
		if result.Error != nil {
			return errors.WrapWithContext(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("readiness check could not evaluate: %s", c.Name),
				result.Error,
				map[string]any{"constraint": c.Name, "expected": c.Value})
		}
		if !result.Passed {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("readiness check failed: %s expected %s, got %s", c.Name, c.Value, result.Actual))
		}
		slog.Info("readiness constraint passed", "name", c.Name, "expected", c.Value, "actual", result.Actual)
	}

	return nil
}

// New creates a new Validator with the provided options.
func New(opts ...Option) *Validator {
	v := &Validator{
		Namespace:   "aicr-validation",
		RunID:       v1.GenerateRunID(),
		Cleanup:     true,
		Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// clusterState holds shared state from cluster preparation, used by both
// ValidatePhases and ValidatePhase.
type clusterState struct {
	clientset kubernetes.Interface
	factory   informers.SharedInformerFactory
	stopCh    chan struct{}
}

// prepareCluster sets up namespace, RBAC, data ConfigMaps, and informer factory.
// The caller must close stopCh and handle cleanup deferrals.
func (v *Validator) prepareCluster(
	ctx context.Context,
	validationInput *v1.ValidationInput,
	snap *snapshotter.Snapshot,
) (*clusterState, error) {

	clientset, _, err := k8sclient.GetKubeClient()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create kubernetes client", err)
	}

	if nsErr := ensureNamespace(ctx, clientset, v.Namespace); nsErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to ensure validation namespace", nsErr)
	}

	if rbacErr := job.EnsureRBAC(ctx, clientset, v.Namespace, v.RunID); rbacErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to ensure RBAC", rbacErr)
	}

	if cmErr := v.ensureDataConfigMaps(ctx, clientset, snap, validationInput); cmErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create data ConfigMaps", cmErr)
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset, 0, informers.WithNamespace(v.Namespace),
	)
	stopCh := make(chan struct{})
	factory.Start(stopCh)

	return &clusterState{
		clientset: clientset,
		factory:   factory,
		stopCh:    stopCh,
	}, nil
}

// deferClusterCleanup registers deferred cleanup for RBAC and data ConfigMaps.
// Both cleanup steps share a single deadline so a stalled apiserver cannot
// extend total post-run blocking time to 2 * K8sCleanupTimeout. Cleanup
// failures are surfaced at structured-log level so operators see when
// resources may have been orphaned in the validator namespace.
func (v *Validator) deferClusterCleanup(clientset kubernetes.Interface) {
	if !v.Cleanup {
		return
	}
	//nolint:contextcheck // Fresh context: parent may be canceled during cleanup
	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
	defer cancel()

	if cleanupErr := job.CleanupRBAC(cleanupCtx, clientset, v.Namespace, v.RunID); cleanupErr != nil {
		slog.Warn("failed to cleanup RBAC; resources may be orphaned",
			"runID", v.RunID, "namespace", v.Namespace, "error", cleanupErr)
	}
	if cmErr := v.cleanupDataConfigMaps(cleanupCtx, clientset); cmErr != nil {
		slog.Warn("failed to cleanup ConfigMaps; resources may be orphaned",
			"runID", v.RunID, "namespace", v.Namespace, "error", cmErr)
	}
}

// ValidatePhases runs the specified phases sequentially and returns one
// PhaseResult per phase. Pass nil or empty phases to run all phases.
// By default all phases run and produce results regardless of failures.
// Set FailFast to stop after the first phase that reports StatusFailed.
func (v *Validator) ValidatePhases(
	ctx context.Context,
	phases []Phase,
	validationInput *v1.ValidationInput,
	snap *snapshotter.Snapshot,
) ([]*PhaseResult, error) {

	if len(phases) == 0 {
		phases = PhaseOrder
	}

	slog.Info("running validation phases", "runID", v.RunID, "phases", phases)

	// Pre-flight: evaluate top-level validation constraints against snapshot.
	// Fails fast before deploying any Jobs if prerequisites aren't met.
	if err := checkReadiness(validationInput, snap); err != nil {
		return nil, err
	}

	cat, err := catalog.LoadWithDataProvider(ctx, v.dataProvider, v.Version, v.Commit)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load validator catalog")
	}

	// --no-cluster: report all as skipped, no K8s calls
	if v.NoCluster {
		return v.phasesSkipped(cat, phases, "skipped - no-cluster mode"), nil
	}

	cs, err := v.prepareCluster(ctx, validationInput, snap)
	if err != nil {
		return nil, err
	}
	defer close(cs.stopCh)
	defer v.deferClusterCleanup(cs.clientset) //nolint:contextcheck // cleanup uses fresh context

	results, err := v.runPhases(ctx, func(phase Phase) (*PhaseResult, error) {
		return v.runPhase(ctx, cs.clientset, cs.factory, cat, phase, validationInput)
	}, cat, phases)
	if err != nil {
		return results, err
	}

	slog.Info("all phases completed", "runID", v.RunID, "phases", len(results))
	return results, nil
}

// runPhases drives the phase loop for ValidatePhases. It is extracted
// as a separate method so the orchestration logic (fail-fast, skip
// recording) can be unit-tested without a live cluster by injecting a
// fake runner.
func (v *Validator) runPhases(
	ctx context.Context,
	runner func(Phase) (*PhaseResult, error),
	cat *catalog.ValidatorCatalog,
	phases []Phase,
) ([]*PhaseResult, error) {

	results := make([]*PhaseResult, 0, len(phases))
	anyFailed := false

	for _, phase := range phases {
		select {
		case <-ctx.Done():
			return results, errors.Wrap(errors.ErrCodeTimeout, "context canceled during phase iteration", ctx.Err())
		default:
		}

		if anyFailed && v.FailFast {
			pr := v.phaseSkipped(cat, phase, "skipped due to previous phase failure")
			results = append(results, pr)
			slog.Info("skipping phase due to fail-fast", "phase", phase)
			continue
		}

		pr, phaseErr := runner(phase)
		if phaseErr != nil {
			return results, phaseErr
		}
		results = append(results, pr)

		if pr.Status == ctrf.StatusFailed {
			anyFailed = true
		}
	}
	return results, nil
}

// ValidatePhase runs a single validation phase.
func (v *Validator) ValidatePhase(
	ctx context.Context,
	phase Phase,
	validationInput *v1.ValidationInput,
	snap *snapshotter.Snapshot,
) (*PhaseResult, error) {

	cat, err := catalog.LoadWithDataProvider(ctx, v.dataProvider, v.Version, v.Commit)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load validator catalog")
	}

	if v.NoCluster {
		return v.phaseSkipped(cat, phase, "skipped - no-cluster mode"), nil
	}

	cs, err := v.prepareCluster(ctx, validationInput, snap)
	if err != nil {
		return nil, err
	}
	defer close(cs.stopCh)
	defer v.deferClusterCleanup(cs.clientset) //nolint:contextcheck // cleanup uses fresh context

	return v.runPhase(ctx, cs.clientset, cs.factory, cat, phase, validationInput)
}

// runPhase executes all validators for a single phase sequentially.
//
//nolint:funlen // Orchestration function with sequential lifecycle steps
func (v *Validator) runPhase(
	ctx context.Context,
	clientset kubernetes.Interface,
	factory informers.SharedInformerFactory,
	cat *catalog.ValidatorCatalog,
	phase Phase,
	validationInput *v1.ValidationInput,
) (*PhaseResult, error) {

	start := time.Now()
	allEntries := cat.ForPhase(phase)

	// Filter catalog entries by checks declared in the validation for this phase.
	// Returns an empty set if no checks are declared for the phase.
	entries := v1.FilterEntriesByValidation(allEntries, phase, validationInput)
	slog.Info("running validation phase", "phase", phase,
		"catalog", len(allEntries), "selected", len(entries))

	builder := ctrf.NewBuilder("aicr", v.Version, string(phase))

	// Pre-flight: validate all dependencyAffinity for required components
	// resolve before any Job is deployed. This honors the per-validator
	// contract (BuildOrchestratorAffinity returns ErrCodeInvalidRequest for
	// missing required components) at phase scope, so a single misconfigured
	// entry doesn't strand a partial deploy of earlier entries.
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, errors.Wrap(errors.ErrCodeTimeout, "context canceled during dependencyAffinity pre-flight", ctx.Err())
		default:
		}
		if err := v1.ValidateDependencyAffinity(entry.DependencyAffinity, validationInput.GetComponentRefs()); err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("dependencyAffinity pre-flight failed for validator %q", entry.Name))
		}
	}

	// TODO(perf): entries within a phase are independent Jobs and can be
	// fan-out with errgroup + a small worker pool. Deferred from the
	// principal-review sweep because parallelism interacts with shared-
	// namespace ConfigMap writes, RBAC cleanup ordering, and GPU resource
	// contention; the change needs its own PR with e2e validation.
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, errors.Wrap(errors.ErrCodeTimeout, "context canceled during entry evaluation", ctx.Err())
		default:
		}

		slog.Info("running validator", "name", entry.Name, "phase", phase)

		deployer := job.NewDeployer(job.Config{
			Clientset:             clientset,
			Factory:               factory,
			Namespace:             v.Namespace,
			RunID:                 v.RunID,
			Entry:                 entry,
			CLIVersion:            v.Version,
			CLICommit:             v.Commit,
			ImagePullSecrets:      v.ImagePullSecrets,
			Tolerations:           v.Tolerations,
			NodeSelector:          v.NodeSelector,
			ImageRegistryOverride: v.ImageRegistryOverride,
			ImageTagOverride:      v.ImageTagOverride,
			ComponentRefs:         validationInput.GetComponentRefs(),
		})

		// Deploy
		if deployErr := deployer.DeployJob(ctx); deployErr != nil {
			slog.Warn("failed to deploy validator Job", "name", entry.Name, "error", deployErr)
			builder.AddResult(&ctrf.ValidatorResult{
				Name:           entry.Name,
				Phase:          entry.Phase,
				ExitCode:       -1,
				TerminationMsg: fmt.Sprintf("failed to deploy Job: %v", deployErr),
			})
			continue
		}

		// Wait
		timeout := entry.Timeout
		if timeout == 0 {
			timeout = defaults.ValidatorDefaultTimeout
		}

		waitErr := deployer.WaitForCompletion(ctx, timeout)

		var result *ctrf.ValidatorResult
		if waitErr != nil {
			// Timeout or infra error — extract what we can with a fresh context
			captureCtx, captureCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout) //nolint:contextcheck // Fresh context: parent may be canceled
			result = deployer.HandleTimeout(captureCtx)                                                        //nolint:contextcheck // Uses fresh context above
			captureCancel()
		} else {
			// Normal completion — extract exit code, termination msg, stdout
			result = deployer.ExtractResult(ctx)
		}

		builder.AddResult(result)
		slog.Info("validator completed", "name", entry.Name, "status", result.CTRFStatus())

		// Surface per-check summary lines to the CLI's own output. The preceding
		// "validator completed" line already names the validator, so echoed
		// summaries are emitted without a redundant key.
		for _, summary := range extractResultSummaries(result.Stdout) {
			slog.Info(summary)
		}

		// Cleanup Job
		if v.Cleanup {
			if cleanupErr := deployer.CleanupJob(ctx); cleanupErr != nil {
				slog.Warn("failed to cleanup Job", "name", entry.Name, "error", cleanupErr)
			}
			termCtx, termCancel := context.WithTimeout(context.Background(), defaults.K8sPodTerminationWaitTimeout) //nolint:contextcheck // Fresh context: parent may be canceled
			if termErr := deployer.WaitForPodTermination(termCtx); termErr != nil {                                 //nolint:contextcheck // Uses fresh context above
				slog.Warn("failed to wait for pod termination", "name", entry.Name, "error", termErr)
			}
			termCancel()
		}
	}

	report := builder.Build()

	// Write CTRF ConfigMap
	if writeErr := ctrf.WriteCTRFConfigMap(ctx, clientset, v.Namespace, v.RunID, string(phase), report); writeErr != nil {
		slog.Warn("failed to write CTRF ConfigMap", "phase", phase, "error", writeErr)
	}

	// Derive phase status from summary
	var status string
	switch {
	case report.Results.Summary.Failed > 0:
		status = ctrf.StatusFailed
	case report.Results.Summary.Other > 0:
		status = ctrf.StatusOther
	case report.Results.Summary.Tests == 0:
		status = ctrf.StatusSkipped
	default:
		status = ctrf.StatusPassed
	}

	duration := time.Since(start)
	slog.Info("phase completed",
		"phase", phase,
		"status", status,
		"validators", report.Results.Summary.Tests,
		"passed", report.Results.Summary.Passed,
		"failed", report.Results.Summary.Failed,
		"duration", duration)

	return &PhaseResult{
		Phase:    phase,
		Status:   status,
		Report:   report,
		Duration: duration,
	}, nil
}

// resultSummaryPrefix marks check stdout lines that should be surfaced to the
// CLI's own output (not only the CTRF stdout array). Any check may emit one
// or more lines like `RESULT: <human-readable summary>` and the validator
// runtime will echo the trailing text at INFO level so users see key metrics
// (throughput, bandwidth, TTFT, etc.) in the live CLI output. Non-prefixed
// stdout stays in the CTRF report only.
const resultSummaryPrefix = "RESULT: "

// extractResultSummaries returns the trailing text of every stdout line that
// begins with resultSummaryPrefix, preserving order and de-duplicating empty
// leftovers. Pure function — extracted so the echo behavior is unit-testable
// without a full validator run.
func extractResultSummaries(stdout []string) []string {
	summaries := make([]string, 0, len(stdout))
	for _, line := range stdout {
		if summary, ok := strings.CutPrefix(line, resultSummaryPrefix); ok && summary != "" {
			summaries = append(summaries, summary)
		}
	}
	return summaries
}

func (v *Validator) phasesSkipped(cat *catalog.ValidatorCatalog, phases []Phase, reason string) []*PhaseResult {
	results := make([]*PhaseResult, 0, len(phases))
	for _, phase := range phases {
		results = append(results, v.phaseSkipped(cat, phase, reason))
	}
	return results
}

func (v *Validator) phaseSkipped(cat *catalog.ValidatorCatalog, phase Phase, reason string) *PhaseResult {
	builder := ctrf.NewBuilder("aicr", v.Version, string(phase))
	for _, entry := range cat.ForPhase(phase) {
		builder.AddSkipped(entry.Name, entry.Phase, reason)
	}
	report := builder.Build()

	return &PhaseResult{
		Phase:  phase,
		Status: ctrf.StatusSkipped,
		Report: report,
	}
}

// EnsureDataConfigMaps creates or updates snapshot and validation ConfigMaps.
// Creates ConfigMaps named aicr-snapshot-{runID} and aicr-validation-{runID} with
// create-or-update semantics. External controllers should call this after generating
// a runID and before rendering validator Jobs. The Jobs mount these ConfigMaps at
// /data/snapshot and /data/validation.
func EnsureDataConfigMaps(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace string,
	runID string,
	snap *snapshotter.Snapshot,
	validationInput *v1.ValidationInput,
) error {

	snapshotYAML, err := yaml.Marshal(snap)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize snapshot", err)
	}

	validationYAML, err := yaml.Marshal(validationInput)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize validation", err)
	}

	snapshotCMName := fmt.Sprintf("aicr-snapshot-%s", runID)
	validationCMName := fmt.Sprintf("aicr-validation-%s", runID)

	for _, cm := range []struct {
		name string
		key  string
		data string
	}{
		{snapshotCMName, "snapshot.yaml", string(snapshotYAML)},
		{validationCMName, "validation.yaml", string(validationYAML)},
	} {
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cm.name,
				Namespace: namespace,
				Labels: map[string]string{
					labels.Name:      labels.ValueAICR,
					labels.Component: labels.ValueValidation,
					labels.ManagedBy: labels.ValueAICR,
					labels.RunID:     runID,
				},
			},
			Data: map[string]string{
				cm.key: cm.data,
			},
		}

		_, createErr := clientset.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
		if createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create ConfigMap %s", cm.name), createErr)
		}
		if apierrors.IsAlreadyExists(createErr) {
			// Fetch existing ConfigMap and mutate it in place to preserve metadata
			existing, getErr := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, cm.name, metav1.GetOptions{})
			if getErr != nil {
				return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to get ConfigMap %s", cm.name), getErr)
			}
			// Update labels in place
			if existing.Labels == nil {
				existing.Labels = map[string]string{}
			}
			existing.Labels[labels.Name] = labels.ValueAICR
			existing.Labels[labels.Component] = labels.ValueValidation
			existing.Labels[labels.ManagedBy] = labels.ValueAICR
			existing.Labels[labels.RunID] = runID
			// Update data
			existing.Data = map[string]string{
				cm.key: cm.data,
			}
			_, updateErr := clientset.CoreV1().ConfigMaps(namespace).Update(ctx, existing, metav1.UpdateOptions{})
			if updateErr != nil {
				return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to update ConfigMap %s", cm.name), updateErr)
			}
		}
	}

	slog.Debug("data ConfigMaps ensured", "runID", runID)
	return nil
}

// ensureDataConfigMaps creates snapshot and validation ConfigMaps for this run.
// Delegates to the public EnsureDataConfigMaps function.
func (v *Validator) ensureDataConfigMaps(
	ctx context.Context,
	clientset kubernetes.Interface,
	snap *snapshotter.Snapshot,
	validationInput *v1.ValidationInput,
) error {

	return EnsureDataConfigMaps(ctx, clientset, v.Namespace, v.RunID, snap, validationInput)
}

// cleanupDataConfigMaps removes snapshot and validation ConfigMaps for this run.
// Returns a joined error covering every delete that failed for a reason other
// than NotFound, so the caller can decide log severity and operators see when
// ConfigMaps may have been left behind in the validator namespace.
func (v *Validator) cleanupDataConfigMaps(ctx context.Context, clientset kubernetes.Interface) error {
	var errs []error
	for _, name := range []string{
		fmt.Sprintf("aicr-snapshot-%s", v.RunID),
		fmt.Sprintf("aicr-validation-%s", v.RunID),
	} {
		err := clientset.CoreV1().ConfigMaps(v.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to delete ConfigMap %s", name), err))
		}
	}

	// Also cleanup CTRF ConfigMaps
	for _, phase := range PhaseOrder {
		if err := ctrf.DeleteCTRFConfigMap(ctx, clientset, v.Namespace, v.RunID, string(phase)); err != nil {
			errs = append(errs, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to delete CTRF ConfigMap for phase %s", phase), err))
		}
	}

	return stderrors.Join(errs...)
}

// ensureNamespace ensures the validator namespace exists with the current
// label schema using server-side apply. SSA is idempotent and conflict-free
// for label reconciliation, so concurrent `aicr validate` runs against a
// shared cluster don't race each other into update-conflict failures the way
// a get-then-update sequence would. Namespace names are immutable; only the
// labels are reconciled.
func ensureNamespace(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	ns := applycorev1.Namespace(namespace).
		WithLabels(map[string]string{
			labels.Name:      labels.ValueAICR,
			labels.Component: labels.ValueValidation,
			labels.ManagedBy: labels.ValueAICR,
		})
	_, err := clientset.CoreV1().Namespaces().Apply(
		ctx, ns, metav1.ApplyOptions{FieldManager: validatorFieldManager, Force: true},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply namespace", err)
	}
	return nil
}
