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
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// checkGPUOperatorVersion detects the deployed GPU operator version and evaluates
// it against the constraint from the recipe (if present).
func checkGPUOperatorVersion(ctx *validators.Context) error {
	version, err := getGPUOperatorVersion(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to detect GPU operator version", err)
	}

	fmt.Printf("Detected GPU Operator version: %s\n", version)

	// Find the version constraint in the recipe
	constraintExpr, found := findDeploymentConstraint(ctx, "Deployment.gpu-operator.version")
	if !found {
		slog.Info("no version constraint in recipe, reporting detected version only")
		return nil
	}

	// Evaluate constraint
	parsed, err := constraints.ParseConstraintExpression(constraintExpr)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid constraint expression", err)
	}

	passed, err := parsed.Evaluate(version)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "constraint evaluation failed", err)
	}

	fmt.Printf("Constraint: %s → %v\n", constraintExpr, passed)

	if !passed {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("GPU Operator version %s does not satisfy constraint %q", version, constraintExpr))
	}

	return nil
}

// findDeploymentConstraint looks up a constraint by name in the recipe's deployment phase.
func findDeploymentConstraint(ctx *validators.Context, name string) (string, bool) {
	if ctx.Recipe == nil || ctx.Recipe.Validation == nil || ctx.Recipe.Validation.Deployment == nil {
		return "", false
	}
	for _, c := range ctx.Recipe.Validation.Deployment.Constraints {
		if c.Name == name {
			return c.Value, true
		}
	}
	return "", false
}

func getGPUOperatorVersion(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	deploymentNames := []string{gpuOperatorNamespace, "nvidia-gpu-operator"}
	namespaces := []string{gpuOperatorNamespace, "nvidia-gpu-operator", "kube-system"}

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

	return "", errors.Wrap(errors.ErrCodeNotFound,
		"could not find GPU operator deployment in common namespaces", lastErr)
}

func getVersionFromDeployment(ctx context.Context, clientset kubernetes.Interface, namespace, name string) (string, error) {
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeNotFound,
			fmt.Sprintf("failed to get deployment %s/%s", namespace, name), err)
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

	return "", errors.New(errors.ErrCodeNotFound,
		fmt.Sprintf("could not determine version from deployment %s/%s", namespace, name))
}

func extractVersionFromImage(image string) string {
	idx := strings.LastIndex(image, ":")
	if idx == -1 {
		return ""
	}
	tag := image[idx+1:]
	if tag == "" {
		return ""
	}
	if dashIdx := strings.Index(tag, "-"); dashIdx != -1 {
		tag = tag[:dashIdx]
	}
	return normalizeVersion(tag)
}

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
