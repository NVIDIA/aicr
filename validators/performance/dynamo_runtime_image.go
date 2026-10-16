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
	"log/slog"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/validators"
)

const dynamoPlatformComponent = "dynamo-platform"

// Dynamo runtime image names under defaults.DynamoImageRepository.
const (
	dynamoVLLMRuntimeImageName = "vllm-runtime"
	dynamoFrontendImageName    = "dynamo-frontend"
)

// resolveDynamoRuntimeTag returns the tag for the Dynamo runtime images. It is
// the recipe's dynamo-platform chart version, so the runtime matches the
// deployed operator when an overlay or --data overrides the chart pin, and
// defaults.DynamoRuntimeImageTag when the recipe's ref carries no version.
func resolveDynamoRuntimeTag(ctx *validators.Context) (string, error) {
	raw, source := recipeDynamoPlatformVersion(ctx), "recipe component "+dynamoPlatformComponent
	if raw == "" {
		raw, source = defaults.DynamoRuntimeImageTag, "defaults.DynamoRuntimeImageTag"
	}

	// The strict parse also keeps arbitrary input out of the image reference.
	// Build metadata is rejected because "+" is not valid in an image tag.
	v, err := semver.StrictNewVersion(strings.TrimPrefix(raw, "v"))
	if err == nil && v.Metadata() != "" {
		err = errors.New(errors.ErrCodeInvalidRequest, "build metadata is not valid in an image tag")
	}
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s version %q from %s: not usable as a Dynamo runtime image tag",
				dynamoPlatformComponent, raw, source), err)
	}
	slog.Info("Resolved Dynamo runtime image tag", "tag", v.String(), "source", source)
	return v.String(), nil
}

// recipeDynamoPlatformVersion returns the recipe's dynamo-platform version, or
// "" when the recipe has no such ref or the ref carries no version.
func recipeDynamoPlatformVersion(ctx *validators.Context) string {
	if ctx == nil || ctx.ValidationInput == nil {
		return ""
	}
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		if ref.Name == dynamoPlatformComponent {
			return strings.TrimSpace(ref.Version)
		}
	}
	return ""
}

// dynamoRuntimeImage returns the reference for Dynamo runtime image name at
// tag, redirected by AICR_VALIDATOR_IMAGE_REGISTRY and never retagged by
// AICR_VALIDATOR_IMAGE_TAG.
func dynamoRuntimeImage(name, tag string) string {
	// The tag must match the deployed dynamo-platform operator, so only the
	// registry override applies.
	return catalog.ApplyRegistryOverride(defaults.DynamoImageRepository + "/" + name + ":" + tag)
}
