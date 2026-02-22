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
	"context"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "platform-health",
		Description: "Verify all expected platform components are deployed and healthy",
		Phase:       phaseConformance,
		Func:        CheckPlatformHealth,
		TestName:    "TestPlatformHealth",
	})
}

// CheckPlatformHealth validates that all expected platform components from the recipe
// are deployed and healthy. It checks namespace existence and expectedResources health
// for each componentRef in the recipe.
func CheckPlatformHealth(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	if ctx.Recipe == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "recipe is not available")
	}

	var failures []string

	// 1. Verify all expected namespaces are Active
	seen := make(map[string]bool)
	for _, ref := range ctx.Recipe.ComponentRefs {
		if ref.Namespace != "" && !seen[ref.Namespace] {
			seen[ref.Namespace] = true
			ns, err := ctx.Clientset.CoreV1().Namespaces().Get(
				ctx.Context, ref.Namespace, metav1.GetOptions{})
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
	for _, ref := range ctx.Recipe.ComponentRefs {
		for _, er := range ref.ExpectedResources {
			if err := verifyExpectedResource(ctx.Context, ctx.Clientset, er); err != nil {
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

// verifyExpectedResource checks that a single expected resource exists and is healthy.
// This mirrors the logic in deployment/expected_resources_check.go:verifyResource.
func verifyExpectedResource(ctx context.Context, clientset kubernetes.Interface, er recipe.ExpectedResource) error {
	ctx, cancel := context.WithTimeout(ctx, defaults.ResourceVerificationTimeout)
	defer cancel()

	switch er.Kind {
	case "Deployment":
		deploy, err := clientset.AppsV1().Deployments(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}
		expected := int32(1)
		if deploy.Spec.Replicas != nil {
			expected = *deploy.Spec.Replicas
		}
		if deploy.Status.AvailableReplicas < expected {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("not healthy: %d/%d replicas available",
					deploy.Status.AvailableReplicas, expected))
		}

	case "DaemonSet":
		ds, err := clientset.AppsV1().DaemonSets(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}
		if ds.Status.NumberReady < ds.Status.DesiredNumberScheduled {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("not healthy: %d/%d pods ready",
					ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
		}

	case "StatefulSet":
		ss, err := clientset.AppsV1().StatefulSets(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}
		expected := int32(1)
		if ss.Spec.Replicas != nil {
			expected = *ss.Spec.Replicas
		}
		if ss.Status.ReadyReplicas < expected {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("not healthy: %d/%d replicas ready",
					ss.Status.ReadyReplicas, expected))
		}

	case "Service":
		_, err := clientset.CoreV1().Services(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}

	case "ConfigMap":
		_, err := clientset.CoreV1().ConfigMaps(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}

	case "Secret":
		_, err := clientset.CoreV1().Secrets(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}

	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported resource kind %q", er.Kind))
	}

	return nil
}
