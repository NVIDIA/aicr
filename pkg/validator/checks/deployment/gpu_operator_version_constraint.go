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
	"strings"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/validator"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func init() {
	checks.RegisterConstraintValidator(&checks.ConstraintValidator{
		Pattern:     "Deployment.gpu-operator.version",
		Description: "Validates GPU Operator version by querying deployed resources",
		Func:        ValidateGPUOperatorVersion,
		TestName:    "TestGPUOperatorVersion",
		Phase:       "deployment",
	})
}

// ValidateGPUOperatorVersion checks the deployed GPU operator version against constraints.
// This validator queries multiple sources to determine the operator version:
// 1. GPU Operator deployment in gpu-operator namespace
// 2. Deployment labels and annotations
// 3. Container image tags
//
// Constraint format: "Deployment.gpu-operator.version"
// Constraint value examples: ">= v24.6.0", "== v25.10.1", "~= v24.6"
func ValidateGPUOperatorVersion(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
	if ctx.Clientset == nil {
		return "", false, errors.New(errors.ErrCodeInvalidRequest, "kubernetes client not available")
	}

	version, err := getGPUOperatorVersion(ctx.Context, ctx.Clientset)
	if err != nil {
		return "", false, errors.Wrap(errors.ErrCodeInternal, "failed to detect GPU operator version", err)
	}

	// Evaluate constraint expression against detected version
	passed, err := evaluateVersionConstraint(version, constraint.Value)
	if err != nil {
		return version, false, errors.Wrap(errors.ErrCodeInternal, "failed to evaluate version constraint", err)
	}

	return version, passed, nil
}

// getGPUOperatorVersion determines the GPU operator version from the cluster.
// It tries multiple strategies in order:
// 1. Check deployment's app.kubernetes.io/version label
// 2. Parse image tag from operator container
// 3. Check deployment annotations
func getGPUOperatorVersion(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	// Try to find the GPU operator deployment
	// Common deployment names: gpu-operator, nvidia-gpu-operator
	deploymentNames := []string{"gpu-operator", "nvidia-gpu-operator"}
	namespaces := []string{"gpu-operator", "nvidia-gpu-operator", "kube-system"}

	var lastErr error
	for _, ns := range namespaces {
		for _, name := range deploymentNames {
			version, err := getVersionFromDeployment(ctx, clientset, ns, name)
			if err == nil && version != "" {
				return version, nil
			}
			if err != nil {
				lastErr = err
			}
		}
	}

	return "", errors.Wrap(errors.ErrCodeNotFound, "could not find GPU operator deployment in common namespaces", lastErr)
}

// getVersionFromDeployment extracts version from a specific deployment.
func getVersionFromDeployment(ctx context.Context, clientset kubernetes.Interface, namespace, name string) (string, error) {
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err != nil {
		return "", err
	}

	// Strategy 1: Check standard version label
	if version, ok := deployment.Labels["app.kubernetes.io/version"]; ok && version != "" {
		return normalizeVersion(version), nil
	}

	// Strategy 2: Parse version from container image tag
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if strings.Contains(container.Image, "gpu-operator") {
			version := extractVersionFromImage(container.Image)
			if version != "" {
				return version, nil
			}
		}
	}

	// Strategy 3: Check annotations
	if version, ok := deployment.Annotations["nvidia.com/gpu-operator-version"]; ok && version != "" {
		return normalizeVersion(version), nil
	}

	return "", errors.New(errors.ErrCodeNotFound, fmt.Sprintf("could not determine version from deployment %s/%s", namespace, name))
}

// extractVersionFromImage parses version from container image.
// Examples:
//   - nvcr.io/nvidia/gpu-operator:v24.6.0 → v24.6.0
//   - nvcr.io/nvidia/gpu-operator:v24.6.0-ubuntu22.04 → v24.6.0
//   - docker.io/nvidia/gpu-operator:24.6.0 → v24.6.0
func extractVersionFromImage(image string) string {
	// Split by ':'
	parts := strings.Split(image, ":")
	if len(parts) != 2 {
		return ""
	}

	tag := parts[1]

	// Handle tags like "v24.6.0-ubuntu22.04" - take version before hyphen
	if idx := strings.Index(tag, "-"); idx != -1 {
		tag = tag[:idx]
	}

	return normalizeVersion(tag)
}

// normalizeVersion ensures version has 'v' prefix.
func normalizeVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ""
	}
	if !strings.HasPrefix(version, "v") {
		return "v" + version
	}
	return version
}

// evaluateVersionConstraint evaluates a version constraint expression.
// Uses the existing constraint expression parser from validator package.
func evaluateVersionConstraint(actualVersion, constraintExpr string) (bool, error) {
	// Parse the constraint expression (e.g., ">= v24.6.0", "== v25.10.1")
	parsed, err := validator.ParseConstraintExpression(constraintExpr)
	if err != nil {
		return false, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid constraint expression", err)
	}

	// Evaluate the constraint
	passed, err := parsed.Evaluate(actualVersion)
	if err != nil {
		return false, errors.Wrap(errors.ErrCodeInternal, "constraint evaluation failed", err)
	}

	return passed, nil
}
