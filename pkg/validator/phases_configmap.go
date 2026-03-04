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
	"fmt"
	"log/slog"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// ensureDataConfigMaps creates ConfigMaps for snapshot and recipe data if they don't exist.
// Returns the names of the created ConfigMaps.
func (v *Validator) ensureDataConfigMaps(
	ctx context.Context,
	clientset kubernetes.Interface,
	snap *snapshotter.Snapshot,
	recipeResult *recipe.RecipeResult,
) error {

	// Use RunID to create unique ConfigMap names per validation run
	snapshotCMName := fmt.Sprintf("aicr-snapshot-%s", v.RunID)
	recipeCMName := fmt.Sprintf("aicr-recipe-%s", v.RunID)

	// Serialize snapshot to YAML
	snapshotYAML, err := yaml.Marshal(snap)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize snapshot", err)
	}

	// Resolve Chainsaw health check assert files from the component registry.
	// This must run before resolveExpectedResources so that components with
	// assert files skip auto-discovery (Chainsaw replaces typed replica checks).
	resolveHealthCheckAsserts(ctx, recipeResult)

	// Auto-discover expected resources from component manifests.
	// NOTE: This intentionally mutates recipeResult.ComponentRefs[].ExpectedResources
	// in place *before* serialization below, so the check pod sees the full
	// expected resources list (manual + auto-discovered) in the deployed ConfigMap.
	if resolveErr := resolveExpectedResources(ctx, recipeResult, kubeVersionFromSnapshot(snap)); resolveErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to resolve expected resources", resolveErr)
	}

	// Serialize recipe to YAML
	recipeYAML, err := yaml.Marshal(recipeResult)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize recipe", err)
	}

	// Create snapshot ConfigMap
	snapshotCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotCMName,
			Namespace: v.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "aicr",
				"app.kubernetes.io/component": "validation",
				"aicr.nvidia.com/data-type":   "snapshot",
				"aicr.nvidia.com/run-id":      v.RunID,
				"aicr.nvidia.com/created-at":  time.Now().Format("20060102-150405"),
			},
		},
		Data: map[string]string{
			"snapshot.yaml": string(snapshotYAML),
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create snapshot ConfigMap", err)
	}
	if apierrors.IsAlreadyExists(err) {
		// Update existing ConfigMap
		_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Update(ctx, snapshotCM, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update snapshot ConfigMap", err)
		}
	}

	// Create recipe ConfigMap
	recipeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      recipeCMName,
			Namespace: v.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "aicr",
				"app.kubernetes.io/component": "validation",
				"aicr.nvidia.com/data-type":   "recipe",
				"aicr.nvidia.com/run-id":      v.RunID,
				"aicr.nvidia.com/created-at":  time.Now().Format("20060102-150405"),
			},
		},
		Data: map[string]string{
			"recipe.yaml": string(recipeYAML),
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create recipe ConfigMap", err)
	}
	if apierrors.IsAlreadyExists(err) {
		// Update existing ConfigMap
		_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Update(ctx, recipeCM, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update recipe ConfigMap", err)
		}
	}

	slog.Debug("ensured data ConfigMaps",
		"snapshot", snapshotCMName,
		"recipe", recipeCMName,
		"namespace", v.Namespace)

	return nil
}

// determineStartPhase analyzes existing ValidationResult to determine where to resume.
// Returns the first phase that needs to run (failed or incomplete).
func determineStartPhase(existingResult *ValidationResult) ValidationPhaseName {
	// Check each phase in order
	for _, phase := range PhaseOrder {
		phaseResult, exists := existingResult.Phases[string(phase)]

		// Phase not yet run or incomplete
		if !exists {
			slog.Info("resuming from phase (not started)", "phase", phase)
			return phase
		}

		// Phase failed - resume from here
		if phaseResult.Status == ValidationStatusFail {
			slog.Info("resuming from phase (previously failed)", "phase", phase)
			return phase
		}

		// Phase passed - skip to next
		slog.Debug("skipping phase (already passed)", "phase", phase, "status", phaseResult.Status)
	}

	// All phases passed - start from beginning (shouldn't happen in normal resume)
	slog.Warn("all phases already passed, starting from beginning")
	return PhaseReadiness
}

// createValidationResultConfigMap creates an empty ValidationResult ConfigMap for this validation run.
func (v *Validator) createValidationResultConfigMap(ctx context.Context, clientset kubernetes.Interface) error {
	resultCMName := fmt.Sprintf("aicr-validation-result-%s", v.RunID)

	// Initialize empty ValidationResult structure
	result := NewValidationResult()
	result.Init(header.KindValidationResult, APIVersion, v.Version)
	result.RunID = v.RunID

	// Serialize to YAML
	resultYAML, err := yaml.Marshal(result)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize validation result", err)
	}

	// Create ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resultCMName,
			Namespace: v.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "aicr",
				"app.kubernetes.io/component": "validation",
				"aicr.nvidia.com/data-type":   "validation-result",
				"aicr.nvidia.com/run-id":      v.RunID,
				"aicr.nvidia.com/created-at":  time.Now().Format("20060102-150405"),
			},
		},
		Data: map[string]string{
			"result.yaml": string(resultYAML),
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create validation result ConfigMap", err)
	}

	slog.Debug("created validation result ConfigMap",
		"name", resultCMName,
		"namespace", v.Namespace)

	return nil
}

// updateValidationResultConfigMap updates the ValidationResult ConfigMap with results from a completed phase.
func (v *Validator) updateValidationResultConfigMap(ctx context.Context, clientset kubernetes.Interface, result *ValidationResult) error {
	resultCMName := fmt.Sprintf("aicr-validation-result-%s", v.RunID)

	// Serialize updated result to YAML
	resultYAML, err := yaml.Marshal(result)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize validation result", err)
	}

	// Get existing ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(v.Namespace).Get(ctx, resultCMName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get validation result ConfigMap", err)
	}

	// Update data
	cm.Data["result.yaml"] = string(resultYAML)

	// Update ConfigMap
	_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to update validation result ConfigMap", err)
	}

	slog.Debug("updated validation result ConfigMap",
		"name", resultCMName,
		"phases", len(result.Phases))

	return nil
}

// readValidationResultConfigMap reads the existing ValidationResult ConfigMap for resume.
func (v *Validator) readValidationResultConfigMap(ctx context.Context, clientset kubernetes.Interface) (*ValidationResult, error) {
	resultCMName := fmt.Sprintf("aicr-validation-result-%s", v.RunID)

	// Get ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(v.Namespace).Get(ctx, resultCMName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("validation result not found for RunID %s", v.RunID), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to get validation result ConfigMap", err)
	}

	// Parse YAML
	resultYAML, ok := cm.Data["result.yaml"]
	if !ok {
		return nil, errors.New(errors.ErrCodeInternal, "result.yaml not found in ConfigMap")
	}

	var result ValidationResult
	if err := yaml.Unmarshal([]byte(resultYAML), &result); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse validation result", err)
	}

	slog.Debug("read validation result ConfigMap",
		"name", resultCMName,
		"phases", len(result.Phases))

	return &result, nil
}

// cleanupValidationResultConfigMap removes the ValidationResult ConfigMap for this validation run.
func (v *Validator) cleanupValidationResultConfigMap(ctx context.Context, clientset kubernetes.Interface) {
	resultCMName := fmt.Sprintf("aicr-validation-result-%s", v.RunID)

	err := clientset.CoreV1().ConfigMaps(v.Namespace).Delete(ctx, resultCMName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete validation result ConfigMap", "name", resultCMName, "error", err)
	}

	slog.Debug("cleaned up validation result ConfigMap", "name", resultCMName)
}

// cleanupDataConfigMaps removes the snapshot and recipe ConfigMaps for this validation run.
func (v *Validator) cleanupDataConfigMaps(ctx context.Context, clientset kubernetes.Interface) {
	// Use RunID to identify ConfigMaps for this validation run
	snapshotCMName := fmt.Sprintf("aicr-snapshot-%s", v.RunID)
	recipeCMName := fmt.Sprintf("aicr-recipe-%s", v.RunID)

	// Delete snapshot ConfigMap
	err := clientset.CoreV1().ConfigMaps(v.Namespace).Delete(ctx, snapshotCMName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete snapshot ConfigMap", "name", snapshotCMName, "error", err)
	}

	// Delete recipe ConfigMap
	err = clientset.CoreV1().ConfigMaps(v.Namespace).Delete(ctx, recipeCMName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete recipe ConfigMap", "name", recipeCMName, "error", err)
	}

	slog.Debug("cleaned up data ConfigMaps", "namespace", v.Namespace)
}
