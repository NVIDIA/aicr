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

package ctrf

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigMapKeyReport is the key used to store the CTRF JSON in the ConfigMap.
	ConfigMapKeyReport = "report.json"
)

// ConfigMapName returns the canonical ConfigMap name for a CTRF report.
func ConfigMapName(runID, phase string) string {
	return fmt.Sprintf("aicr-ctrf-%s-%s", runID, phase)
}

// WriteCTRFConfigMap serializes a Report to JSON and writes it to a ConfigMap.
// Uses create-or-update semantics: creates the ConfigMap if it does not exist,
// updates it if it already exists from a previous run.
func WriteCTRFConfigMap(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, runID, phase string,
	report *Report,
) error {

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal CTRF report", err)
	}

	cmName := ConfigMapName(runID, phase)
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
			Labels: map[string]string{
				labels.Name:       labels.ValueAICR,
				labels.Component:  labels.ValueValidation,
				labels.ManagedBy:  labels.ValueAICR,
				labels.RunID:      runID,
				labels.Phase:      phase,
				labels.ReportType: "ctrf",
			},
		},
		Data: map[string]string{
			ConfigMapKeyReport: string(data),
		},
	}

	// Use server-side apply for idempotent create-or-update.
	patchData, marshalErr := json.Marshal(cm)
	if marshalErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal ConfigMap for apply", marshalErr)
	}
	force := true
	_, err = clientset.CoreV1().ConfigMaps(namespace).Patch(
		ctx, cmName, types.ApplyPatchType, patchData,
		metav1.PatchOptions{FieldManager: "aicr-validator", Force: &force},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply CTRF ConfigMap", err)
	}

	slog.Debug("wrote CTRF ConfigMap",
		"name", cmName,
		"namespace", namespace,
		"phase", phase,
		"tests", report.Results.Summary.Tests)

	return nil
}

// ReadCTRFConfigMap reads a CTRF Report from a ConfigMap.
func ReadCTRFConfigMap(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, runID, phase string,
) (*Report, error) {

	cmName := ConfigMapName(runID, phase)

	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("CTRF ConfigMap %q not found", cmName), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to get CTRF ConfigMap", err)
	}

	reportJSON, ok := cm.Data[ConfigMapKeyReport]
	if !ok {
		return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf("key %q not found in ConfigMap %q", ConfigMapKeyReport, cmName))
	}

	var report Report
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to unmarshal CTRF report", err)
	}

	return &report, nil
}

// DeleteCTRFConfigMap removes a CTRF ConfigMap. Ignores NotFound errors.
func DeleteCTRFConfigMap(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, runID, phase string,
) error {

	cmName := ConfigMapName(runID, phase)
	err := clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, cmName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to delete CTRF ConfigMap", err)
	}
	return nil
}
