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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"

	v1 "github.com/NVIDIA/aicr/pkg/api/validator/v1"
	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"github.com/NVIDIA/aicr/pkg/validator/job"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
)

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
		RunID:       generateRunID(),
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

	if rbacErr := job.EnsureRBAC(ctx, clientset, v.Namespace); rbacErr != nil {
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
func (v *Validator) deferClusterCleanup(clientset kubernetes.Interface) {
	if v.Cleanup {
		//nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cancel()
		if cleanupErr := job.CleanupRBAC(cleanupCtx, clientset, v.Namespace); cleanupErr != nil {
			slog.Warn("failed to cleanup RBAC", "error", cleanupErr)
		}
		//nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cmCtx, cmCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cmCancel()
		v.cleanupDataConfigMaps(cmCtx, clientset)
	}
}

// ValidatePhases runs the specified phases sequentially. If a phase fails,
// subsequent phases are skipped. Returns one PhaseResult per phase.
// Pass nil or empty phases to run all phases.
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

	cat, err := catalog.Load(v.Version, v.Commit)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load validator catalog", err)
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

	results := make([]*PhaseResult, 0, len(phases))
	overallFailed := false

	for _, phase := range phases {
		select {
		case <-ctx.Done():
			return results, errors.Wrap(errors.ErrCodeTimeout, "context canceled during phase iteration", ctx.Err())
		default:
		}

		if overallFailed {
			// Skip with a CTRF report showing all validators as skipped
			pr := v.phaseSkipped(cat, phase, "skipped due to previous phase failure")
			results = append(results, pr)
			slog.Info("skipping phase due to previous failure", "phase", phase)
			continue
		}

		pr, phaseErr := v.runPhase(ctx, cs.clientset, cs.factory, cat, phase, validationInput)
		if phaseErr != nil {
			return results, phaseErr
		}
		results = append(results, pr)

		if pr.Status == ctrf.StatusFailed {
			overallFailed = true
		}
	}

	slog.Info("all phases completed", "runID", v.RunID, "phases", len(results))
	return results, nil
}

// ValidatePhase runs a single validation phase.
func (v *Validator) ValidatePhase(
	ctx context.Context,
	phase Phase,
	validationInput *v1.ValidationInput,
	snap *snapshotter.Snapshot,
) (*PhaseResult, error) {

	cat, err := catalog.Load(v.Version, v.Commit)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load validator catalog", err)
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

// filterEntriesByValidation returns only catalog entries that the validation declares
// for the given phase. If the validation has no phase configuration or the phase
// has no checks declared, no entries are returned (skip the phase).
// The validation is the source of truth — only explicitly declared checks run.
func filterEntriesByValidation(entries []catalog.ValidatorEntry, phase Phase, validationInput *v1.ValidationInput) []catalog.ValidatorEntry {
	if validationInput == nil {
		return nil
	}

	var phaseChecks []string
	switch phase {
	case PhaseDeployment:
		if validationInput.Config.Deployment != nil {
			phaseChecks = validationInput.Config.Deployment.Checks
		}
	case PhasePerformance:
		if validationInput.Config.Performance != nil {
			phaseChecks = validationInput.Config.Performance.Checks
		}
	case PhaseConformance:
		if validationInput.Config.Conformance != nil {
			phaseChecks = validationInput.Config.Conformance.Checks
		}
	}

	// No checks declared for this phase → skip it.
	if len(phaseChecks) == 0 {
		return nil
	}

	// Build set for O(1) lookup.
	allowed := make(map[string]bool, len(phaseChecks))
	for _, name := range phaseChecks {
		allowed[name] = true
	}

	filtered := make([]catalog.ValidatorEntry, 0, len(phaseChecks))
	for _, entry := range entries {
		if allowed[entry.Name] {
			filtered = append(filtered, entry)
		}
	}

	return filtered
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
	allEntries := cat.ForPhase(string(phase))

	// Filter catalog entries by what the validation declares.
	// If the validation has checks for this phase, only run those.
	// If no checks are declared, run all catalog entries for the phase.
	entries := filterEntriesByValidation(allEntries, phase, validationInput)
	slog.Info("running validation phase", "phase", phase,
		"catalog", len(allEntries), "selected", len(entries))

	builder := ctrf.NewBuilder("aicr", v.Version, string(phase))

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

		deployer := job.NewDeployer(
			clientset, factory, v.Namespace, v.RunID, v.Version, v.Commit, entry,
			v.ImagePullSecrets, v.Tolerations, v.NodeSelector,
		)

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
	for _, entry := range cat.ForPhase(string(phase)) {
		builder.AddSkipped(entry.Name, entry.Phase, reason)
	}
	report := builder.Build()

	return &PhaseResult{
		Phase:  phase,
		Status: ctrf.StatusSkipped,
		Report: report,
	}
}

// ensureDataConfigMaps creates snapshot and validation ConfigMaps for this run.
// The validation payload is marshaled as v1.ValidationInput (the wire shape
// that validator containers consume) with phase configs nested under `config:`.
func (v *Validator) ensureDataConfigMaps(
	ctx context.Context,
	clientset kubernetes.Interface,
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

	snapshotCMName := fmt.Sprintf("aicr-snapshot-%s", v.RunID)
	validationCMName := fmt.Sprintf("aicr-validation-%s", v.RunID)

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
				Namespace: v.Namespace,
				Labels: map[string]string{
					labels.Name:      labels.ValueAICR,
					labels.Component: labels.ValueValidation,
					labels.ManagedBy: labels.ValueAICR,
					labels.RunID:     v.RunID,
				},
			},
			Data: map[string]string{
				cm.key: cm.data,
			},
		}

		_, createErr := clientset.CoreV1().ConfigMaps(v.Namespace).Create(ctx, configMap, metav1.CreateOptions{})
		if createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create ConfigMap %s", cm.name), createErr)
		}
		if apierrors.IsAlreadyExists(createErr) {
			_, updateErr := clientset.CoreV1().ConfigMaps(v.Namespace).Update(ctx, configMap, metav1.UpdateOptions{})
			if updateErr != nil {
				return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to update ConfigMap %s", cm.name), updateErr)
			}
		}
	}

	slog.Debug("data ConfigMaps ensured", "runID", v.RunID)
	return nil
}

// cleanupDataConfigMaps removes snapshot and validation ConfigMaps for this run.
func (v *Validator) cleanupDataConfigMaps(ctx context.Context, clientset kubernetes.Interface) {
	for _, name := range []string{
		fmt.Sprintf("aicr-snapshot-%s", v.RunID),
		fmt.Sprintf("aicr-validation-%s", v.RunID),
	} {
		err := clientset.CoreV1().ConfigMaps(v.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("failed to delete ConfigMap", "name", name, "error", err)
		}
	}

	// Also cleanup CTRF ConfigMaps
	for _, phase := range PhaseOrder {
		if err := ctrf.DeleteCTRFConfigMap(ctx, clientset, v.Namespace, v.RunID, string(phase)); err != nil {
			slog.Warn("failed to delete CTRF ConfigMap", "phase", phase, "error", err)
		}
	}
}

// ensureNamespace creates the namespace if it does not exist, or updates its
// labels to the current schema if it does. Namespace names are immutable but
// labels and annotations are not, so a stale namespace from a prior AICR
// version would otherwise keep outdated labels.
func ensureNamespace(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	desired := map[string]string{
		labels.Name:      labels.ValueAICR,
		labels.Component: labels.ValueValidation,
		labels.ManagedBy: labels.ValueAICR,
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: desired,
		},
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", err)
	}
	// Already exists — fetch and reconcile labels if they have drifted.
	existing, getErr := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if getErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get existing namespace", getErr)
	}
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	changed := false
	for k, v := range desired {
		if existing.Labels[k] != v {
			existing.Labels[k] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if _, err := clientset.CoreV1().Namespaces().Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to update namespace labels", err)
	}
	return nil
}

func generateRunID() string {
	timestamp := time.Now().Format("20060102-150405")
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		return timestamp
	}
	return fmt.Sprintf("%s-%s", timestamp, hex.EncodeToString(randomBytes))
}
