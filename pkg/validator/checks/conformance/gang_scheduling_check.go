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

package conformance

import (
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kaiSchedulerDeployments are the required KAI scheduler components.
var kaiSchedulerDeployments = []string{
	"kai-scheduler-default",
	"admission",
	"binder",
	"kai-operator",
	"pod-grouper",
	"podgroup-controller",
	"queue-controller",
}

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "gang-scheduling",
		Description: "Verify KAI scheduler components and gang scheduling CRDs",
		Phase:       phaseConformance,
		Func:        CheckGangScheduling,
		TestName:    "TestGangScheduling",
	})
}

// CheckGangScheduling validates CNCF requirement #7: Gang Scheduling.
// Verifies all KAI scheduler component deployments are running and
// the required gang scheduling CRDs exist.
func CheckGangScheduling(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. All KAI scheduler deployments available
	for _, name := range kaiSchedulerDeployments {
		if err := verifyDeploymentAvailable(ctx, "kai-scheduler", name); err != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("KAI scheduler component %s check failed", name), err)
		}
	}

	// 2. Required CRDs for gang scheduling
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	requiredCRDs := []string{
		"queues.scheduling.run.ai",
		"podgroups.scheduling.run.ai",
	}
	for _, crd := range requiredCRDs {
		_, err := dynClient.Resource(crdGVR).Get(ctx.Context, crd, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("gang scheduling CRD %s not found", crd), err)
		}
	}

	return nil
}
