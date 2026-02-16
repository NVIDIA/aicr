// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package deployment

import (
	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	// Register this check
	checks.RegisterCheck(&checks.Check{
		Name:        "operator-health",
		Description: "Verify GPU operator pods are running and healthy",
		Phase:       "deployment",
		Func:        CheckOperatorHealth,
		TestName:    "TestOperatorHealth",
	})
}

// CheckOperatorHealth validates that GPU operator is deployed and healthy.
// Returns nil if validation passes, error if it fails.
func CheckOperatorHealth(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// Query live cluster for gpu-operator pods
	pods, err := ctx.Clientset.CoreV1().Pods("gpu-operator").List(
		ctx.Context,
		metav1.ListOptions{
			LabelSelector: "app=gpu-operator",
		},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list gpu-operator pods", err)
	}

	if len(pods.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound, "no gpu-operator pods found")
	}

	// Check that at least one pod is running
	runningCount := 0
	for _, pod := range pods.Items {
		if pod.Status.Phase == "Running" {
			runningCount++
		}
	}

	if runningCount == 0 {
		return errors.New(errors.ErrCodeInternal, "no gpu-operator pods are in Running state")
	}

	return nil
}
