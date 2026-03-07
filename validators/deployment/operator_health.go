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

package main

import (
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	gpuOperatorNamespace = "gpu-operator"
	gpuOperatorLabel     = "app=gpu-operator"
)

// checkOperatorHealth verifies GPU operator pods are running and healthy.
func checkOperatorHealth(ctx *validators.Context) error {
	slog.Info("listing pods", "namespace", gpuOperatorNamespace, "label", gpuOperatorLabel)

	pods, err := ctx.Clientset.CoreV1().Pods(gpuOperatorNamespace).List(
		ctx.Ctx,
		metav1.ListOptions{LabelSelector: gpuOperatorLabel},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list gpu-operator pods", err)
	}

	if len(pods.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound, "no gpu-operator pods found")
	}

	// Evidence to stdout
	fmt.Printf("Found %d gpu-operator pod(s):\n", len(pods.Items))

	runningCount := 0
	for _, pod := range pods.Items {
		status := string(pod.Status.Phase)
		fmt.Printf("  %s: %s\n", pod.Name, status)
		if pod.Status.Phase == v1.PodRunning {
			runningCount++
		}
	}

	fmt.Printf("Running: %d/%d\n", runningCount, len(pods.Items))

	if runningCount == 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("no gpu-operator pods are in Running state (%d total)", len(pods.Items)))
	}

	slog.Info("operator-health: passed", "running", runningCount)
	return nil
}
