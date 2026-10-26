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

package bundler

import (
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/bundler/drainscope"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// ApplyNVSentinelDrainScope runs drainscope.Apply on the nvsentinel values
// with the recipe's declared namespaces.
func ApplyNVSentinelDrainScope(values map[string]any, recipeResult *recipe.RecipeResult) error {
	// Checked first so a recipe without node-drainer never fails on a
	// component that has no namespace.
	if !drainscope.NodeDrainerRenders(values) {
		return nil
	}
	namespaces, err := drainscope.RecipeNamespaces(recipeResult)
	if err != nil {
		return err
	}
	if err := drainscope.Apply(values, namespaces); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			"failed to set NVSentinel node-drainer drain scope")
	}
	scope, _ := component.GetValueByPath(values, drainscope.SystemNamespacesPath)
	user, _ := component.GetValueByPath(values, drainscope.UserNamespacesPath)
	slog.Debug("NVSentinel node-drainer drain scope set",
		"systemNamespaces", scope, "userNamespaces", user)
	return nil
}
