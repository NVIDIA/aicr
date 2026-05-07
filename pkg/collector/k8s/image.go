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

package k8s

import (
	"context"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// collectContainerImages extracts unique container images from all pods.
//
// Pods are listed via paginated API calls (see defaults.K8sPodListPageSize)
// to bound peak memory on large clusters. Iteration honors ctx cancellation
// between pages and within each page's pod loop.
func (k *Collector) collectContainerImages(ctx context.Context) (map[string]measurement.Reading, error) {
	// Track unique images (map of image name to version)
	imageVersions := make(map[string]string)
	recordImage := func(imageRef string) {
		if imageRef == "" {
			return
		}
		// Strip registry prefix to get just image:tag
		imageNameTag := stripRegistryPrefix(imageRef)

		// Split image name and tag
		name, tag := splitImageNameTag(imageNameTag)
		if name != "" {
			imageVersions[name] = tag
		}
	}

	continueToken := ""
	pageCount := 0
	podCount := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "image collection cancelled", err)
		}

		podList, err := k.ClientSet.CoreV1().Pods("").List(ctx, v1.ListOptions{
			Limit:    defaults.K8sPodListPageSize,
			Continue: continueToken,
		})
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to list pods", err)
		}
		pageCount++

		for i := range podList.Items {
			// Check for context cancellation
			if err := ctx.Err(); err != nil {
				return nil, errors.Wrap(errors.ErrCodeTimeout, "image collection cancelled", err)
			}

			pod := &podList.Items[i]
			podCount++

			for _, container := range pod.Spec.Containers {
				recordImage(container.Image)
			}
			for _, container := range pod.Spec.InitContainers {
				recordImage(container.Image)
			}
			for _, container := range pod.Spec.EphemeralContainers {
				recordImage(container.Image)
			}
		}

		continueToken = podList.Continue
		if continueToken == "" {
			break
		}
	}

	// Convert to final result format
	images := make(map[string]measurement.Reading)
	for name, tag := range imageVersions {
		images[name] = measurement.Str(tag)
	}

	slog.Debug("collected container images",
		slog.Int("images", len(images)),
		slog.Int("pods", podCount),
		slog.Int("pages", pageCount))
	return images, nil
}

// stripRegistryPrefix removes the registry domain from image references.
// Examples:
//   - "registry.k8s.io/pause:3.9" -> "pause:3.9"
//   - "docker.io/library/nginx:latest" -> "nginx:latest"
//   - "ghcr.io/org/image:v1.0" -> "image:v1.0"
func stripRegistryPrefix(imageRef string) string {
	// Find the last slash which separates the image name from registry/org path
	lastSlash := strings.LastIndex(imageRef, "/")
	if lastSlash == -1 {
		// No slashes, already just image:tag
		return imageRef
	}
	return imageRef[lastSlash+1:]
}

// splitImageNameTag splits an image reference into name and tag.
// Examples:
//   - "nginx:1.21" -> ("nginx", "1.21")
//   - "argocd:v2.14.3" -> ("argocd", "v2.14.3")
//   - "nginx" -> ("nginx", "latest")
//   - "image:v1.0@sha256:abc123..." -> ("image", "v1.0")
func splitImageNameTag(imageRef string) (name, tag string) {
	// First, strip any digest (@sha256:...)
	atIdx := strings.Index(imageRef, "@")
	if atIdx != -1 {
		imageRef = imageRef[:atIdx]
	}

	// Split on the last colon to separate image name from tag
	colonIdx := strings.LastIndex(imageRef, ":")
	if colonIdx == -1 {
		// No tag specified, use "latest" as default
		return imageRef, "latest"
	}
	return imageRef[:colonIdx], imageRef[colonIdx+1:]
}
