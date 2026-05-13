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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CheckPlatformHealth validates that all expected platform components from the recipe
// are deployed and healthy. It checks namespace existence and expectedResources health
// for each componentRef in the recipe.
func CheckPlatformHealth(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	if ctx.ValidationInput == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "validation is not available")
	}

	var failures []string

	// 1. Verify all expected namespaces are Active
	seen := make(map[string]bool)
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		if ref.Namespace != "" && !seen[ref.Namespace] {
			seen[ref.Namespace] = true
			ns, err := ctx.Clientset.CoreV1().Namespaces().Get(
				ctx.Ctx, ref.Namespace, metav1.GetOptions{})
			if err != nil {
				failures = append(failures, fmt.Sprintf("namespace %s: not found", ref.Namespace))
				continue
			}
			if ns.Status.Phase != corev1.NamespaceActive {
				failures = append(failures, fmt.Sprintf("namespace %s: phase=%s (want Active)",
					ref.Namespace, ns.Status.Phase))
			}
		}
	}

	// 2. Verify all expectedResources are healthy
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		for _, er := range ref.ExpectedResources {
			if err := helper.VerifyResource(ctx.Ctx, ctx.Clientset, er); err != nil {
				failures = append(failures, fmt.Sprintf("%s %s/%s (%s): %s",
					er.Kind, er.Namespace, er.Name, ref.Name, err.Error()))
			}
		}
	}

	if len(failures) > 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("platform health check failed (%d issues):\n  %s",
				len(failures), strings.Join(failures, "\n  ")))
	}
	return nil
}
