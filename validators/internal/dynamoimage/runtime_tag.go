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

// Package dynamoimage resolves the tag of the Dynamo runtime images the
// validators deploy.
package dynamoimage

import (
	"fmt"
	"log/slog"
	"strings"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/validators"
	"github.com/Masterminds/semver/v3"
)

const dynamoPlatformComponent = "dynamo-platform"

// RuntimeTag returns the tag for the Dynamo runtime images. It is the recipe's
// dynamo-platform chart version, so the runtime matches the deployed operator
// when an overlay or --data overrides the chart pin, and
// defaults.DynamoRuntimeImageTag when the recipe's ref carries no version.
func RuntimeTag(ctx *validators.Context) (string, error) {
	raw, source := platformVersion(ctx), "recipe component "+dynamoPlatformComponent
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

// platformVersion returns the recipe's dynamo-platform version, or "" when the
// recipe has no such ref or the ref carries no version.
func platformVersion(ctx *validators.Context) string {
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
