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
	"context"
	"fmt"
	"strings"

	"aicr.run/pkg/constraints"
	"aicr.run/pkg/errors"
	"aicr.run/validators"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const gpuOperatorVersionConstraint = "Deployment.gpu-operator.version"

// checkGPUOperatorVersion detects the deployed GPU operator version and evaluates
// it against the constraint from the recipe (if present).
func checkGPUOperatorVersion(ctx *validators.Context) error {
	constraintExpr, hasConstraint := findDeploymentConstraint(ctx, gpuOperatorVersionConstraint)

	version, pinnedImage, err := getGPUOperatorVersion(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to detect GPU operator version", err)
	}

	// A digest is not a version. When the operator image is pinned by digest
	// alone and the Deployment carries no version label or annotation, a
	// declared constraint cannot be evaluated and fails closed (#1995).
	if version == "" {
		fmt.Printf("Detected GPU Operator version: unknown (image %s is pinned by digest only)\n", pinnedImage)
		if !hasConstraint {
			return validators.SkipWithReason(validators.SkipReasonNoConstraint,
				fmt.Sprintf("no %s constraint in recipe (version unknown: image %s is pinned by digest only)",
					gpuOperatorVersionConstraint, pinnedImage))
		}
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("%s %q is set but the GPU Operator version could not be read: image %s is pinned by digest "+
				"only and the Deployment has no app.kubernetes.io/version label or nvidia.com/gpu-operator-version "+
				"annotation; reference the image as repo:tag@sha256:<digest>, or set the label (or the annotation) "+
				"to the deployed version",
				gpuOperatorVersionConstraint, constraintExpr, pinnedImage))
	}

	fmt.Printf("Detected GPU Operator version: %s\n", version)
	emitExtraOrWarn(map[string]string{validators.ExtraKeyDetectedVersion: version})

	if !hasConstraint {
		return validators.SkipWithReason(validators.SkipReasonNoConstraint,
			fmt.Sprintf("no %s constraint in recipe (detected %s)", gpuOperatorVersionConstraint, version))
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
	emitExtraOrWarn(map[string]string{validators.ExtraKeyVersionSatisfied: validators.FormatBool(passed)})

	if !passed {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("GPU Operator version %s does not satisfy constraint %q", version, constraintExpr))
	}

	return nil
}

// findDeploymentConstraint looks up a constraint by name in the validation's deployment phase.
func findDeploymentConstraint(ctx *validators.Context, name string) (string, bool) {
	if ctx.ValidationInput == nil || ctx.ValidationInput.Config.Deployment == nil {
		return "", false
	}
	for _, c := range ctx.ValidationInput.Config.Deployment.Constraints {
		if c.Name == name {
			return c.Value, true
		}
	}
	return "", false
}

// getGPUOperatorVersion returns the version from the first GPU operator
// deployment, in search order, that has a version source. When that
// deployment's only source is an operator image pinned by digest alone, it
// returns an empty version and that image: the search stops at that
// deployment instead of falling through to a later one.
func getGPUOperatorVersion(ctx context.Context, clientset kubernetes.Interface) (string, string, error) {
	deploymentNames := []string{gpuOperatorNamespace, "nvidia-gpu-operator"}
	namespaces := []string{gpuOperatorNamespace, "nvidia-gpu-operator", "kube-system"}

	var lastErr error
	for _, ns := range namespaces {
		for _, name := range deploymentNames {
			version, pinnedImage, err := getVersionFromDeployment(ctx, clientset, ns, name)
			if err == nil && (version != "" || pinnedImage != "") {
				return version, pinnedImage, nil
			}
			if err != nil {
				lastErr = err
			}
		}
	}

	return "", "", errors.Wrap(errors.ErrCodeNotFound,
		"could not find GPU operator deployment in common namespaces", lastErr)
}

// getVersionFromDeployment reads the operator version from one deployment: the
// app.kubernetes.io/version label, else the tag of the first gpu-operator image
// that carries a tag or a digest, else the nvidia.com/gpu-operator-version
// annotation. When that image is pinned by digest alone and the annotation is
// absent, it returns an empty version, the image, and no error.
func getVersionFromDeployment(ctx context.Context, clientset kubernetes.Interface, namespace, name string) (string, string, error) {
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", errors.Wrap(errors.ErrCodeNotFound,
			fmt.Sprintf("failed to get deployment %s/%s", namespace, name), err)
	}

	// Strategy 1: Check standard version label. A blank value counts as absent.
	if version := normalizeVersion(deployment.Labels["app.kubernetes.io/version"]); version != "" {
		return version, "", nil
	}

	// Strategy 2: Parse version from container image tag. The first
	// gpu-operator image with a tag or a digest decides; a digest-only image
	// leaves the version to the annotation.
	pinnedImage := ""
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if !strings.Contains(container.Image, "gpu-operator") {
			continue
		}
		if version := extractVersionFromImage(container.Image); version != "" {
			return version, "", nil
		}
		if _, digest := parseImageReference(container.Image); digest != "" {
			pinnedImage = container.Image
			break
		}
	}

	// Strategy 3: Check annotations. A blank value counts as absent, so a
	// digest-only image is still reported as pinned.
	if version := normalizeVersion(deployment.Annotations["nvidia.com/gpu-operator-version"]); version != "" {
		return version, "", nil
	}

	if pinnedImage != "" {
		return "", pinnedImage, nil
	}

	return "", "", errors.New(errors.ErrCodeNotFound,
		fmt.Sprintf("could not determine version from deployment %s/%s", namespace, name))
}

// parseImageReference splits an image reference into its tag and digest,
// either of which may be empty. It accepts repo:tag, repo@<algorithm>:<hex>
// and repo:tag@<algorithm>:<hex>, and does not mistake a registry port for a
// tag.
func parseImageReference(image string) (tag, digest string) {
	image = strings.TrimSpace(image)
	if at := strings.Index(image, "@"); at != -1 {
		digest = image[at+1:]
		image = image[:at]
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		tag = image[colon+1:]
	}
	return tag, digest
}

// extractVersionFromImage returns the normalized version from the image tag,
// or "" when the reference has no tag (including a digest-only reference).
func extractVersionFromImage(image string) string {
	tag, _ := parseImageReference(image)
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
