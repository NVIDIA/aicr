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

package validator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"github.com/NVIDIA/aicr/pkg/validator/job"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
)

// checkReadiness evaluates top-level recipe constraints against the snapshot.
// Returns an error if any constraint fails, nil if all pass or no constraints exist.
func checkReadiness(rec *recipe.RecipeResult, snap *snapshotter.Snapshot) error {
	if rec == nil || snap == nil || len(rec.Constraints) == 0 {
		return nil
	}

	slog.Info("readiness pre-flight", "constraints", len(rec.Constraints))

	for _, c := range rec.Constraints {
		result := constraints.Evaluate(c, snap)
		if result.Error != nil {
			slog.Warn("readiness constraint skipped", "name", c.Name, "error", result.Error)
			continue
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

// ValidatePhases runs the specified phases sequentially. If a phase fails,
// subsequent phases are skipped. Returns one PhaseResult per phase.
// Pass nil or empty phases to run all phases.
func (v *Validator) ValidatePhases(
	ctx context.Context,
	phases []Phase,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) ([]*PhaseResult, error) {

	if len(phases) == 0 {
		phases = PhaseOrder
	}

	slog.Info("running validation phases", "runID", v.RunID, "phases", phases)

	// Pre-flight: evaluate top-level recipe constraints against snapshot.
	// Fails fast before deploying any Jobs if prerequisites aren't met.
	if err := checkReadiness(recipeResult, snap); err != nil {
		return nil, err
	}

	cat, err := catalog.Load()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load validator catalog", err)
	}

	// --no-cluster: report all as skipped, no K8s calls
	if v.NoCluster {
		return v.phasesSkipped(cat, phases, "skipped - no-cluster mode"), nil
	}

	clientset, _, err := k8sclient.GetKubeClient()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create kubernetes client", err)
	}

	// Ensure validation namespace exists before starting informers or creating RBAC.
	if nsErr := ensureNamespace(ctx, clientset, v.Namespace); nsErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to ensure validation namespace", nsErr)
	}

	// Shared informer factory scoped to the validation namespace.
	// Started once and reused across all phases and deployers.
	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset, 0, informers.WithNamespace(v.Namespace),
	)
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	defer close(stopCh)

	// RBAC: create once, cleanup at end
	if rbacErr := job.EnsureRBAC(ctx, clientset, v.Namespace); rbacErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to ensure RBAC", rbacErr)
	}
	if v.Cleanup {
		//nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
			defer cancel()
			if cleanupErr := job.CleanupRBAC(cleanupCtx, clientset, v.Namespace); cleanupErr != nil {
				slog.Warn("failed to cleanup RBAC", "error", cleanupErr)
			}
		}()
	}

	// Data ConfigMaps: create once, cleanup at end
	if cmErr := v.ensureDataConfigMaps(ctx, clientset, snap, recipeResult); cmErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create data ConfigMaps", cmErr)
	}
	if v.Cleanup {
		//nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
			defer cancel()
			v.cleanupDataConfigMaps(cleanupCtx, clientset)
		}()
	}

	results := make([]*PhaseResult, 0, len(phases))
	overallFailed := false

	for _, phase := range phases {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		if overallFailed {
			// Skip with a CTRF report showing all validators as skipped
			pr := v.phaseSkipped(cat, phase, "skipped due to previous phase failure")
			results = append(results, pr)
			slog.Info("skipping phase due to previous failure", "phase", phase)
			continue
		}

		pr, phaseErr := v.runPhase(ctx, clientset, factory, cat, phase, recipeResult)
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
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*PhaseResult, error) {

	cat, err := catalog.Load()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load validator catalog", err)
	}

	if v.NoCluster {
		return v.phaseSkipped(cat, phase, "skipped - no-cluster mode"), nil
	}

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
	if v.Cleanup {
		//nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
			defer cancel()
			if cleanupErr := job.CleanupRBAC(cleanupCtx, clientset, v.Namespace); cleanupErr != nil {
				slog.Warn("failed to cleanup RBAC", "error", cleanupErr)
			}
		}()
	}

	if cmErr := v.ensureDataConfigMaps(ctx, clientset, snap, recipeResult); cmErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create data ConfigMaps", cmErr)
	}
	if v.Cleanup {
		//nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
			defer cancel()
			v.cleanupDataConfigMaps(cleanupCtx, clientset)
		}()
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset, 0, informers.WithNamespace(v.Namespace),
	)
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	defer close(stopCh)

	return v.runPhase(ctx, clientset, factory, cat, phase, recipeResult)
}

// filterEntriesByRecipe returns only catalog entries that the recipe declares
// for the given phase. If the recipe has no checks for this phase, all entries
// are returned (backward compatible — run everything).
func filterEntriesByRecipe(entries []catalog.ValidatorEntry, phase Phase, recipeResult *recipe.RecipeResult) []catalog.ValidatorEntry {
	if recipeResult == nil || recipeResult.Validation == nil {
		return entries
	}

	var phaseChecks []string
	switch phase {
	case PhaseDeployment:
		if recipeResult.Validation.Deployment != nil {
			phaseChecks = recipeResult.Validation.Deployment.Checks
		}
	case PhasePerformance:
		if recipeResult.Validation.Performance != nil {
			phaseChecks = recipeResult.Validation.Performance.Checks
		}
	case PhaseConformance:
		if recipeResult.Validation.Conformance != nil {
			phaseChecks = recipeResult.Validation.Conformance.Checks
		}
	}

	// No checks declared → run all catalog entries for this phase.
	if len(phaseChecks) == 0 {
		return entries
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
	recipeResult *recipe.RecipeResult,
) (*PhaseResult, error) {

	start := time.Now()
	allEntries := cat.ForPhase(string(phase))

	// Filter catalog entries by what the recipe declares.
	// If the recipe has checks for this phase, only run those.
	// If no checks are declared, run all catalog entries for the phase.
	entries := filterEntriesByRecipe(allEntries, phase, recipeResult)
	slog.Info("running validation phase", "phase", phase,
		"catalog", len(allEntries), "selected", len(entries))

	builder := ctrf.NewBuilder("aicr", v.Version, string(phase))

	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		slog.Info("running validator", "name", entry.Name, "phase", phase)

		deployer := job.NewDeployer(
			clientset, factory, v.Namespace, v.RunID, entry,
			v.ImagePullSecrets, v.Tolerations,
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

		// Cleanup Job
		if v.Cleanup {
			if cleanupErr := deployer.CleanupJob(ctx); cleanupErr != nil {
				slog.Warn("failed to cleanup Job", "name", entry.Name, "error", cleanupErr)
			}
			termCtx, termCancel := context.WithTimeout(context.Background(), defaults.K8sPodTerminationWaitTimeout) //nolint:contextcheck // Fresh context: parent may be canceled
			deployer.WaitForPodTermination(termCtx)                                                                 //nolint:contextcheck // Uses fresh context above
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

// ensureDataConfigMaps creates snapshot and recipe ConfigMaps for this run.
func (v *Validator) ensureDataConfigMaps(
	ctx context.Context,
	clientset kubernetes.Interface,
	snap *snapshotter.Snapshot,
	recipeResult *recipe.RecipeResult,
) error {

	snapshotYAML, err := yaml.Marshal(snap)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize snapshot", err)
	}

	recipeYAML, err := yaml.Marshal(recipeResult)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize recipe", err)
	}

	snapshotCMName := fmt.Sprintf("aicr-snapshot-%s", v.RunID)
	recipeCMName := fmt.Sprintf("aicr-recipe-%s", v.RunID)

	for _, cm := range []struct {
		name string
		key  string
		data string
	}{
		{snapshotCMName, "snapshot.yaml", string(snapshotYAML)},
		{recipeCMName, "recipe.yaml", string(recipeYAML)},
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

// cleanupDataConfigMaps removes snapshot and recipe ConfigMaps for this run.
func (v *Validator) cleanupDataConfigMaps(ctx context.Context, clientset kubernetes.Interface) {
	for _, name := range []string{
		fmt.Sprintf("aicr-snapshot-%s", v.RunID),
		fmt.Sprintf("aicr-recipe-%s", v.RunID),
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

// ensureNamespace creates the namespace if it does not exist.
// Uses create-or-ignore since namespaces are immutable.
func ensureNamespace(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
			Labels: map[string]string{
				labels.Name:      labels.ValueAICR,
				labels.Component: labels.ValueValidation,
				labels.ManagedBy: labels.ValueAICR,
			},
		},
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", err)
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
