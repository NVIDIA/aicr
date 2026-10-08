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

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const nvsentinelMongoDBStorageClassPath = "replsets.rs0.volumeSpec.pvc.storageClassName"

// CheckNVSentinelDatastorePrerequisites enforces the snapshot-driven cluster
// prerequisites of nvsentinel-mongodb: a default StorageClass for its
// classless volumes, and no other Percona Operator for MongoDB whose CRDs
// psmdb-operator would replace. A non-empty
// replsets.rs0.volumeSpec.pvc.storageClassName or bundle --storage-class
// waives the StorageClass requirement. Empty metadata means the recipe was
// not resolved from a snapshot carrying this evidence and produces
// non-blocking warnings.
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

func nvsentinelStorageClassFinding(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, keys []string) (string, error) {
	state := recipeResult.Metadata.DefaultStorageClassState
	switch state {
	case recipe.DefaultStorageClassStatePresent:
		return "", nil
	case "":
		return fmt.Sprintf("%s: no metadata.defaultStorageClassState snapshot evidence was recorded, so the "+
			"default StorageClass its volumes need was not evaluated. Regenerate the recipe from a current "+
			"snapshot to verify the target cluster before deployment", componentName), nil
	case recipe.DefaultStorageClassStateMultiple:
		return fmt.Sprintf("%s: the snapshot found more than one default StorageClass; Kubernetes binds its "+
			"volumes to the newest one. Leave a single StorageClass annotated "+
			"storageclass.kubernetes.io/is-default-class=true, or set "+
			"--set nvsentinelmongodb:%s=<class>", componentName, nvsentinelMongoDBStorageClassPath), nil
	case recipe.DefaultStorageClassStateAbsent, recipe.DefaultStorageClassStateUnknown:
	default:
		return "", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s: metadata.defaultStorageClassState=%q is not recognized. Regenerate the recipe with this "+
				"AICR version before bundling", componentName, state))
	}

	named, err := nvsentinelMongoDBNamesStorageClass(ctx, recipeResult, bundlerConfig, componentName, keys)
	if err != nil {
		return "", err
	}
	if named {
		return "", nil
	}

	if state == recipe.DefaultStorageClassStateAbsent {
		return "", aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
			"%s: the snapshot found no default StorageClass, so its three volumes, which name no "+
				"StorageClass, would stay Pending and NVSentinel would not start. Annotate a StorageClass "+
				"with storageclass.kubernetes.io/is-default-class=true and regenerate the recipe from a "+
				"fresh snapshot, or name one with --storage-class <class> or --set nvsentinelmongodb:%s=<class>",
			componentName, nvsentinelMongoDBStorageClassPath))
	}
	return "", aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
		"%s: default StorageClass evidence is inconclusive, so its volumes cannot be shown to bind. "+
			"Capture a fresh snapshot with permission to list storageclasses (storage.k8s.io), or name a "+
			"StorageClass with --storage-class <class> or --set nvsentinelmongodb:%s=<class>",
		componentName, nvsentinelMongoDBStorageClassPath))
}

// nvsentinelMongoDBNamesStorageClass reports whether the bundle names a
// StorageClass for the volumes: through the effective values, or through
// bundle --storage-class, which the bundler injects into the registry's
// storageClassPaths unless a scalar --set names the path explicitly.
func nvsentinelMongoDBNamesStorageClass(ctx context.Context, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, componentName string, keys []string) (bool, error) {
	if bundlerConfig != nil && bundlerConfig.StorageClass() != "" {
		explicit := mergeOverridesAcrossKeys(bundlerConfig.ValueOverrides(), keys)
		if _, set := explicit[nvsentinelMongoDBStorageClassPath]; !set {
			return true, nil
		}
	}
	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, keys,
		"NVSentinel datastore prerequisites")
	if err != nil {
		return false, err
	}
	class, ok, _ := resolvedStringValue(values, nvsentinelMongoDBStorageClassPath)
	return ok && class != "", nil
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
			"%s: the snapshot found a Percona Operator for MongoDB running outside the nvsentinel namespace. "+
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
