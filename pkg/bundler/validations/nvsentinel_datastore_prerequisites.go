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

package validations

import (
	"context"
	"fmt"
	"slices"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const nvsentinelMongoDBStorageClassPath = "replsets.rs0.volumeSpec.pvc.storageClassName"

// CheckNVSentinelDatastorePrerequisites enforces the snapshot-driven cluster
// prerequisites of nvsentinel-mongodb: a default StorageClass for its
// classless volumes, and no other Percona Operator for MongoDB whose CRDs
// psmdb-operator would replace. A non-empty
// replsets.rs0.volumeSpec.pvc.storageClassName or bundle --storage-class
// waives the StorageClass requirement and its warnings. Empty metadata means
// the recipe was not resolved from a snapshot carrying this evidence and
// produces non-blocking warnings.
func CheckNVSentinelDatastorePrerequisites(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	ref := recipeResult.GetComponentRef(componentName)
	if ref == nil {
		return nil, nil
	}
	keys := componentOverrideKeys(componentName, recipeResult.DataProvider())
	if componentDisabled(ref, bundlerConfig, keys) {
		return nil, nil
	}

	var warnings []string
	var errs []error

	storageWarning, storageErr := nvsentinelStorageClassFinding(ctx, componentName, recipeResult, bundlerConfig, keys)
	if storageWarning != "" {
		warnings = append(warnings, storageWarning)
	}
	if storageErr != nil {
		errs = append(errs, storageErr)
	}

	perconaWarning, perconaErr := nvsentinelPerconaFinding(componentName, recipeResult.Metadata.PerconaOperatorState)
	if perconaWarning != "" {
		warnings = append(warnings, perconaWarning)
	}
	if perconaErr != nil {
		errs = append(errs, perconaErr)
	}
	return warnings, errs
}

// nvsentinelMongoDBClaim is nvsentinel-mongodb's classless volumes.
var nvsentinelMongoDBClaim = storageClassClaim{
	path:        nvsentinelMongoDBStorageClassPath,
	overrideKey: "nvsentinelmongodb",
	volumes:     "its volumes",
	pending:     "its three volumes, which name no StorageClass, would stay Pending and NVSentinel would not start",
	purpose:     "NVSentinel datastore prerequisites",
}

func nvsentinelStorageClassFinding(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, keys []string) (string, error) {
	return storageClassFinding(ctx, componentName, recipeResult, bundlerConfig, keys, nvsentinelMongoDBClaim)
}

// storageClassClaim is a volume claim that names no StorageClass unless the
// bundle does, so whether it binds depends on the cluster's default.
type storageClassClaim struct {
	// path is the values path that names the claim's StorageClass, and
	// overrideKey the --set key of the component that owns it.
	path        string
	overrideKey string
	// volumes names the claim as a sentence subject, and pending what a
	// Pending claim stops.
	volumes string
	pending string
	purpose string
}

// storageClassFinding applies the snapshot's metadata.defaultStorageClassState
// to claim. A bundle that names a class for the claim has no finding;
// otherwise no evidence or more than one default warns, and none or
// inconclusive evidence is an error. An unrecognized state is an error either
// way.
func storageClassFinding(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, keys []string, claim storageClassClaim) (string, error) {
	state := recipeResult.Metadata.DefaultStorageClassState
	switch state {
	case recipe.DefaultStorageClassStatePresent:
		return "", nil
	case "", recipe.DefaultStorageClassStateMultiple, recipe.DefaultStorageClassStateAbsent,
		recipe.DefaultStorageClassStateUnknown:
	default:
		return "", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s: metadata.defaultStorageClassState=%q is not recognized. Regenerate the recipe with this "+
				"AICR version before bundling", componentName, state))
	}

	named, err := bundleNamesStorageClass(ctx, recipeResult, bundlerConfig, componentName, keys, claim)
	if err != nil {
		return "", err
	}
	if named {
		return "", nil
	}

	switch state {
	case "":
		return fmt.Sprintf("%s: no metadata.defaultStorageClassState snapshot evidence was recorded, so the "+
			"default StorageClass for %s was not evaluated. Regenerate the recipe from a current "+
			"snapshot to verify the target cluster before deployment", componentName, claim.volumes), nil
	case recipe.DefaultStorageClassStateMultiple:
		return fmt.Sprintf("%s: the snapshot found more than one default StorageClass; Kubernetes binds "+
			"%s to the newest one. Leave a single StorageClass annotated "+
			"storageclass.kubernetes.io/is-default-class=true, or set "+
			"--set %s:%s=<class>", componentName, claim.volumes, claim.overrideKey, claim.path), nil
	case recipe.DefaultStorageClassStateAbsent:
		return "", aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
			"%s: the snapshot found no default StorageClass, so %s. Annotate a StorageClass "+
				"with storageclass.kubernetes.io/is-default-class=true and regenerate the recipe from a "+
				"fresh snapshot, or name one with --storage-class <class> or --set %s:%s=<class>",
			componentName, claim.pending, claim.overrideKey, claim.path))
	}
	return "", aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
		"%s: default StorageClass evidence is inconclusive, so %s cannot be shown to bind. "+
			"Capture a fresh snapshot with permission to list storageclasses (storage.k8s.io), or name a "+
			"StorageClass with --storage-class <class> or --set %s:%s=<class>",
		componentName, claim.volumes, claim.overrideKey, claim.path))
}

// bundleNamesStorageClass reports whether the bundle names a StorageClass
// for claim. It models the bundler's order: recipe values, then --set, then
// --storage-class injected into the component's registry storageClassPaths
// unless --set names the path, then --set-json/--set-file, which can still
// clear the class (an empty or null value, or a parent object without it).
func bundleNamesStorageClass(ctx context.Context, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, componentName string, keys []string, claim storageClassClaim) (bool, error) {
	values, err := recipeResult.GetValuesForComponentWithContext(ctx, componentName)
	if err != nil {
		return false, aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("cannot verify %s for component %q: failed to resolve its values", claim.purpose, componentName))
	}
	if bundlerConfig == nil {
		class, ok, _ := resolvedStringValue(values, claim.path)
		return ok && class != "", nil
	}

	explicit := mergeOverridesAcrossKeys(bundlerConfig.ValueOverrides(), keys)
	delete(explicit, config.ComponentEnabledKey)
	if len(explicit) > 0 {
		if applyErr := component.ApplyMapOverrides(values, explicit); applyErr != nil {
			return false, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("cannot verify %s for component %q: failed to apply --set overrides", claim.purpose,
					componentName), applyErr)
		}
	}
	if class := bundlerConfig.StorageClass(); class != "" {
		if _, set := explicit[claim.path]; !set && storageClassPathRegistered(recipeResult, componentName, claim.path) {
			// The bundler logs and skips a path it cannot write, so a
			// failure here leaves the class unnamed, as it does there.
			_ = component.ApplyMapOverrides(values, map[string]string{claim.path: class})
		}
	}
	typed := mergeOverridesAcrossKeys(bundlerConfig.ValueOverridesTyped(), keys)
	delete(typed, config.ComponentEnabledKey)
	if len(typed) > 0 {
		if applyErr := component.ApplyTypedOverrides(values, typed); applyErr != nil {
			return false, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("cannot verify %s for component %q: failed to apply --set-json/--set-file overrides",
					claim.purpose, componentName), applyErr)
		}
	}
	class, ok, _ := resolvedStringValue(values, claim.path)
	return ok && class != "", nil
}

// storageClassPathRegistered reports whether path is one of the component's
// registry storageClassPaths, the only paths --storage-class reaches.
func storageClassPathRegistered(recipeResult *recipe.RecipeResult, componentName, path string) bool {
	registry, err := recipe.GetComponentRegistryFor(recipeResult.DataProvider())
	if err != nil {
		return false
	}
	comp := registry.Get(componentName)
	return comp != nil && slices.Contains(comp.GetStorageClassPaths(), path)
}

func nvsentinelPerconaFinding(componentName, state string) (string, error) {
	switch state {
	case recipe.PerconaOperatorStateAbsent, recipe.PerconaOperatorStateAICROwned:
		return "", nil
	case "":
		return fmt.Sprintf("%s: no metadata.perconaOperatorState snapshot evidence was recorded, so an "+
			"existing Percona Operator for MongoDB was not ruled out. Regenerate the recipe from a current "+
			"snapshot to verify the target cluster before deployment", componentName), nil
	case recipe.PerconaOperatorStateAPIDetected:
		return fmt.Sprintf("%s: the cluster already serves psmdb.percona.com with no PerconaServerMongoDB "+
			"resources. psmdb-operator owns those CRDs and will replace them with its own version; this is "+
			"safe when no other Percona operator runs. If one does, stop: AICR does not support sharing the "+
			"cluster with another Percona Operator for MongoDB", componentName), nil
	case recipe.PerconaOperatorStateCRsDetected:
		return "", aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
			"%s: the snapshot found PerconaServerMongoDB resources other than AICR's own. psmdb-operator "+
				"would replace the psmdb.percona.com CRDs they depend on. Remove the other Percona "+
				"installation, or leave the nvsentinel remediation step mixins out of the recipe",
			componentName))
	case recipe.PerconaOperatorStateOperatorDetected:
		return "", aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
			"%s: the snapshot found a Percona Operator for MongoDB that AICR did not install (outside the "+
				"nvsentinel namespace, or in it under another Helm release). "+
				"psmdb-operator would replace the psmdb.percona.com CRDs it depends on. Remove the other "+
				"operator, or leave the nvsentinel remediation step mixins out of the recipe",
			componentName))
	case recipe.PerconaOperatorStateUnknown:
		return "", aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
			"%s: Percona Operator for MongoDB conflict evidence is inconclusive. Capture a fresh snapshot "+
				"with sufficient Kubernetes discovery permissions (list psmdb.percona.com "+
				"perconaservermongodbs and pods)", componentName))
	default:
		return "", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s: metadata.perconaOperatorState=%q is not recognized. Regenerate the recipe with this AICR "+
				"version before bundling", componentName, state))
	}
}
