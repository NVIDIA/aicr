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

// Command check-dynamo-runtime-image verifies that
// defaults.DynamoRuntimeImageTag equals the dynamo-platform chart version in
// recipes/registry.yaml, because an older Dynamo runtime under a newer
// operator is a known source of frontend discovery panics. It exits 1 on a
// mismatch, and 2 when the registry cannot be loaded or has no
// dynamo-platform pin.
//
// Usage:
//
//	go run ./tools/check-dynamo-runtime-image
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const componentName = "dynamo-platform"

func main() {
	reg, err := recipe.GetComponentRegistry()
	if err != nil {
		slog.Error("load component registry", "error", err)
		os.Exit(2)
	}
	pinned, err := pinnedChartVersion(reg)
	if err != nil {
		slog.Error("resolve pinned chart version", "error", err)
		os.Exit(2)
	}
	if err := checkTag(pinned, defaults.DynamoRuntimeImageTag); err != nil {
		slog.Error("dynamo runtime image drift", "error", err)
		os.Exit(1)
	}
	slog.Info("dynamo runtime image tag matches the dynamo-platform chart", "version", pinned)
}

// pinnedChartVersion returns the dynamo-platform chart version pinned in reg,
// or an error if reg has no such component or it has no pinned version.
func pinnedChartVersion(reg *recipe.ComponentRegistry) (string, error) {
	c := reg.Get(componentName)
	if c == nil {
		return "", errors.New(errors.ErrCodeNotFound, "component "+componentName+" not found in registry")
	}
	if c.Helm.DefaultVersion == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest, "component "+componentName+" has no pinned chart version")
	}
	return c.Helm.DefaultVersion, nil
}

// checkTag returns an ErrCodeConflict error if tag differs from pinned.
func checkTag(pinned, tag string) error {
	if tag == pinned {
		return nil
	}
	return errors.New(errors.ErrCodeConflict, fmt.Sprintf(
		"defaults.DynamoRuntimeImageTag is %q but recipes/registry.yaml pins %s chart %q. "+
			"Set DynamoRuntimeImageTag in pkg/defaults/images.go to %q",
		tag, componentName, pinned, pinned))
}
