// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bundler

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
)

const (
	slinkySlurmComponentName           = "slinky-slurm"
	slinkySharedStoragePreManifestPath = "components/slinky-slurm/manifests/shared-storage-pvcs.yaml"
	slinkyNameKey                      = "name"
	slinkyStorageClassNameKey          = "storageClassName"
	slinkySizeKey                      = "size"
)

type slinkySharedVolume struct {
	claimName  string
	volumeName string
	mountPath  string
}

type slinkyMountTarget struct {
	path      string
	parent    map[string]any
	key       string
	makeEntry func(slinkySharedVolume) map[string]any
}

func newSlinkySharedMountTargets(values map[string]any) ([]slinkyMountTarget, error) {
	definitions := []struct {
		setsKey      string
		containerKey string
	}{
		{setsKey: "loginsets", containerKey: "login"},
		{setsKey: "nodesets", containerKey: "slurmd"},
	}

	targets := make([]slinkyMountTarget, 0)
	for _, definition := range definitions {
		setsValue, exists := values[definition.setsKey]
		if !exists || setsValue == nil {
			continue
		}
		sets, ok := setsValue.(map[string]any)
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("slinky-slurm %s must be an object", definition.setsKey))
		}
		names := make([]string, 0, len(sets))
		for name := range sets {
			names = append(names, name)
		}
		sort.Strings(names)

		// Prepare every configured set, including sets with enabled=false. This
		// preserves the shared-storage contract when an installer enables a set
		// later through Helm value overrides.
		for _, name := range names {
			setPath := definition.setsKey + "." + name
			set, ok := sets[name].(map[string]any)
			if !ok {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("slinky-slurm %s must be an object", setPath))
			}
			container, err := objectField(set, definition.containerKey, setPath)
			if err != nil {
				return nil, err
			}
			podSpec, err := objectField(set, "podSpec", setPath)
			if err != nil {
				return nil, err
			}
			targets = append(targets,
				slinkyMountTarget{
					path:   setPath + "." + definition.containerKey + ".volumeMounts",
					parent: container,
					key:    "volumeMounts",
					makeEntry: func(volume slinkySharedVolume) map[string]any {
						return map[string]any{slinkyNameKey: volume.volumeName, "mountPath": volume.mountPath}
					},
				},
				slinkyMountTarget{
					path:   setPath + ".podSpec.volumes",
					parent: podSpec,
					key:    "volumes",
					makeEntry: func(volume slinkySharedVolume) map[string]any {
						return map[string]any{
							slinkyNameKey:           volume.volumeName,
							"persistentVolumeClaim": map[string]any{"claimName": volume.claimName},
						}
					},
				},
			)
		}
	}
	return targets, nil
}

// materializeSlinkySharedStorage validates enabled shared-storage values and
// appends the matching volumes and mounts to Slinky's LoginSet and NodeSet
// values. The PVC objects themselves are rendered by the component's
// shared-storage-pvcs pre-manifest.
func materializeSlinkySharedStorage(values map[string]any, supported bool) error {
	storageValue, exists := values["storage"]
	if !exists {
		return nil
	}
	storage, ok := storageValue.(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInvalidRequest,
			"slinky-slurm storage must be an object")
	}
	unsupportedKeys := make([]string, 0)
	for key := range storage {
		switch key {
		case "enabled", "home", "data":
			// Supported shared-storage fields.
		default:
			unsupportedKeys = append(unsupportedKeys, key)
		}
	}
	if len(unsupportedKeys) > 0 {
		sort.Strings(unsupportedKeys)
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm storage contains unsupported fields: %s; supported fields are enabled, home, and data",
				strings.Join(unsupportedKeys, ", ")))
	}

	enabledValue, exists := storage["enabled"]
	if !exists {
		return nil
	}
	enabled, ok := enabledValue.(bool)
	if !ok {
		return errors.New(errors.ErrCodeInvalidRequest,
			"slinky-slurm storage.enabled must be a boolean")
	}
	if !enabled {
		return nil
	}
	if !supported {
		return errors.New(errors.ErrCodeInvalidRequest,
			"slinky-slurm shared storage is not supported by this recipe")
	}

	// Validate and normalize a copy so an error never partially mutates the
	// caller's values. The normalized strings are what the PVC template renders.
	updatedValues := component.DeepCopyMap(values)
	storage = updatedValues["storage"].(map[string]any)

	volumes := make([]slinkySharedVolume, 0, 2)
	for _, definition := range []struct {
		key        string
		volumeName string
		mountPath  string
	}{
		{key: "home", volumeName: "shared-home", mountPath: "/home"},
		{key: "data", volumeName: "shared-data", mountPath: "/scratch/fsw"},
	} {
		volume, err := parseSlinkySharedVolume(storage, definition.key, definition.volumeName, definition.mountPath)
		if err != nil {
			return err
		}
		volumes = append(volumes, volume)
	}
	if volumes[0].claimName == volumes[1].claimName {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm storage.home.name and storage.data.name must be different; both resolve to %q",
				volumes[0].claimName))
	}

	targets, err := newSlinkySharedMountTargets(updatedValues)
	if err != nil {
		return err
	}
	for _, target := range targets {
		entries, err := namedObjectList(target.parent, target.key, target.path)
		if err != nil {
			return err
		}
		for _, volume := range volumes {
			entries, err = appendNamedObject(entries, target.makeEntry(volume), target.path)
			if err != nil {
				return err
			}
		}
		target.parent[target.key] = entries
	}

	clear(values)
	maps.Copy(values, updatedValues)
	return nil
}

func hasSlinkySharedStoragePreManifest(paths []string) bool {
	return slices.Contains(paths, slinkySharedStoragePreManifestPath)
}

func parseSlinkySharedVolume(
	storage map[string]any,
	key string,
	volumeName string,
	mountPath string,
) (slinkySharedVolume, error) {

	value, exists := storage[key]
	if !exists {
		return slinkySharedVolume{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm storage.%s is required when shared storage is enabled", key))
	}
	config, ok := value.(map[string]any)
	if !ok {
		return slinkySharedVolume{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm storage.%s must be an object", key))
	}

	claimName, err := requiredString(config, slinkyNameKey, "storage."+key)
	if err != nil {
		return slinkySharedVolume{}, err
	}
	if validationErrors := validation.IsDNS1123Subdomain(claimName); len(validationErrors) > 0 {
		return slinkySharedVolume{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm storage.%s.name %q is invalid: %s",
				key, claimName, strings.Join(validationErrors, "; ")))
	}

	storageClassName, err := requiredString(config, slinkyStorageClassNameKey, "storage."+key)
	if err != nil {
		return slinkySharedVolume{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm shared storage requires an RWX StorageClass for storage.%s; "+
				"use --shared-storage-class <name> or set both per-volume storageClassName values", key))
	}
	if validationErrors := validation.IsDNS1123Subdomain(storageClassName); len(validationErrors) > 0 {
		return slinkySharedVolume{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm storage.%s.storageClassName %q is invalid: %s",
				key, storageClassName, strings.Join(validationErrors, "; ")))
	}
	size, err := requiredString(config, slinkySizeKey, "storage."+key)
	if err != nil {
		return slinkySharedVolume{}, err
	}
	quantity, parseErr := resource.ParseQuantity(size)
	if parseErr != nil || quantity.Sign() <= 0 {
		return slinkySharedVolume{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm storage.%s.size %q must be a positive Kubernetes quantity", key, size))
	}
	config[slinkyNameKey] = claimName
	config[slinkyStorageClassNameKey] = storageClassName
	config[slinkySizeKey] = size

	return slinkySharedVolume{
		claimName:  claimName,
		volumeName: volumeName,
		mountPath:  mountPath,
	}, nil
}

func requiredString(values map[string]any, key, parentPath string) (string, error) {
	value, exists := values[key]
	if !exists {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm %s.%s is required", parentPath, key))
	}
	result, ok := value.(string)
	if !ok || strings.TrimSpace(result) == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm %s.%s must be a non-empty string", parentPath, key))
	}
	return strings.TrimSpace(result), nil
}

func objectField(parent map[string]any, key, parentPath string) (map[string]any, error) {
	value, exists := parent[key]
	if !exists || value == nil {
		result := map[string]any{}
		parent[key] = result
		return result, nil
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm %s.%s must be an object", parentPath, key))
	}
	return result, nil
}

func namedObjectList(parent map[string]any, key, path string) ([]any, error) {
	value, exists := parent[key]
	if !exists || value == nil {
		return []any{}, nil
	}
	entries, ok := value.([]any)
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm %s must be a list", path))
	}
	return append([]any(nil), entries...), nil
}

func appendNamedObject(entries []any, entry map[string]any, path string) ([]any, error) {
	name := entry[slinkyNameKey]
	for _, existing := range entries {
		existingMap, ok := existing.(map[string]any)
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("slinky-slurm %s entries must be objects", path))
		}
		if existingMap[slinkyNameKey] != name {
			continue
		}
		if reflect.DeepEqual(existingMap, entry) {
			return entries, nil
		}
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("slinky-slurm %s contains a conflicting %q entry", path, name))
	}
	return append(entries, entry), nil
}
