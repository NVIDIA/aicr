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
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/chainsaw"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func init() {
	// Register this check
	checks.RegisterCheck(&checks.Check{
		Name:        "expected-resources",
		Description: "Verify expected Kubernetes resources exist and are healthy after component deployment",
		Phase:       "deployment",
		Func:        validateExpectedResources,
		TestName:    "TestCheckExpectedResources",
	})
}

// validateExpectedResources verifies that all expected Kubernetes resources declared
// in the recipe's componentRefs exist and are healthy in the live cluster.
// Components with HealthCheckAsserts use Chainsaw CLI assertions; others use
// the default auto-discovery + typed client replica checks.
func validateExpectedResources(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	if ctx.Recipe == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "recipe is not available")
	}

	var chainsawAsserts []chainsaw.ComponentAssert
	var failures []string

	for _, ref := range ctx.Recipe.ComponentRefs {
		// Manual expectedResources take precedence over Chainsaw health check asserts.
		// This allows users to override the registry's healthCheck.assertFile by
		// declaring expectedResources explicitly in the recipe.
		if ref.HealthCheckAsserts != "" && len(ref.ExpectedResources) == 0 {
			chainsawAsserts = append(chainsawAsserts, chainsaw.ComponentAssert{
				Name:       ref.Name,
				AssertYAML: ref.HealthCheckAsserts,
			})
			continue
		}

		for _, er := range ref.ExpectedResources {
			if err := verifyResource(ctx.Context, ctx.Clientset, er); err != nil {
				failures = append(failures, fmt.Sprintf("%s %s/%s (%s): %s",
					er.Kind, er.Namespace, er.Name, ref.Name, err.Error()))
			}
		}
	}

	// Run Chainsaw assertions for components that have them.
	if len(chainsawAsserts) > 0 {
		slog.Info("running chainsaw health checks", "components", len(chainsawAsserts))
		results := chainsaw.Run(ctx.Context, chainsawAsserts, defaults.ChainsawAssertTimeout)
		for _, r := range results {
			if !r.Passed {
				msg := fmt.Sprintf("%s: chainsaw health check failed", r.Component)
				if r.Output != "" {
					msg += fmt.Sprintf(":\n%s", r.Output)
				}
				if r.Error != nil {
					msg += fmt.Sprintf("\nerror: %v", r.Error)
				}
				failures = append(failures, msg)
			}
		}
	}

	if len(failures) > 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("expected resource check failed:\n  %s", strings.Join(failures, "\n  ")))
	}
	return nil
}

// verifyResource checks that a single expected resource exists and is healthy.
func verifyResource(ctx context.Context, clientset kubernetes.Interface, er recipe.ExpectedResource) error {
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
