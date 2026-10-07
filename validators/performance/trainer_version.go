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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Masterminds/semver/v3"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// trainerVersionEnv overrides the Kubeflow Trainer release the NCCL checks
	// self-install. Set it through the catalog entry's env block.
	trainerVersionEnv = "AICR_NCCL_TRAINER_VERSION"

	// trainerArchiveURLFormat takes a validated semantic version. The kubeflow-trainer
	// Helm chart and the source tree share one version, tagged with a leading v.
	trainerArchiveURLFormat = "https://github.com/kubeflow/trainer/archive/refs/tags/v%s.tar.gz"
)

// resolveTrainerVersion returns the validated Kubeflow Trainer release to
// self-install. Precedence is the recipe's kubeflow-trainer version, then
// trainerVersionEnv, then the kubeflow-trainer pin in the component registry. A
// recipe that enables kubeflow-trainer never reaches the self-install, so in
// practice the recipe version comes from a ref the recipe disables.
func resolveTrainerVersion(ctx *validators.Context) (string, error) {
	raw, source := recipeTrainerPin(ctx), "recipe component "+kubeflowTrainerComponent
	if raw == "" {
		raw, source = strings.TrimSpace(os.Getenv(trainerVersionEnv)), trainerVersionEnv
	}
	if raw == "" {
		pin, err := registryTrainerPin()
		if err != nil {
			return "", err
		}
		raw, source = pin, "component registry"
	}

	// The strict parse also keeps arbitrary input out of the archive URL.
	v, err := semver.StrictNewVersion(strings.TrimPrefix(raw, "v"))
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Kubeflow Trainer version %q from %s: not a semantic version", raw, source), err)
	}
	slog.Info("Resolved Kubeflow Trainer self-install version", "version", v.String(), "source", source)
	return v.String(), nil
}

// recipeTrainerPin returns the recipe's kubeflow-trainer version, or "" when the
// recipe has no such ref or the ref carries no version.
func recipeTrainerPin(ctx *validators.Context) string {
	if ctx == nil || ctx.ValidationInput == nil {
		return ""
	}
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		if ref.Name == kubeflowTrainerComponent {
			return strings.TrimSpace(ref.Version)
		}
	}
	return ""
}

// registryTrainerPin returns the kubeflow-trainer chart version from the
// embedded component registry.
func registryTrainerPin() (string, error) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to load the component registry", err)
	}
	if comp := registry.Get(kubeflowTrainerComponent); comp != nil && comp.Helm.DefaultVersion != "" {
		return comp.Helm.DefaultVersion, nil
	}
	return "", aicrErrors.New(aicrErrors.ErrCodeInternal,
		fmt.Sprintf("component registry has no %s version pin", kubeflowTrainerComponent))
}

// checkTrainerOverlay fails with ErrCodeInvalidRequest when the release archive has
// no manager overlay directory at kustomizePath.
func checkTrainerOverlay(kustomizePath, version string) error {
	info, err := os.Stat(kustomizePath)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && !info.IsDir():
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"Kubeflow Trainer %s archive has no manager overlay directory at %s", version, trainerKustomizePath))
	case err != nil:
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to inspect the Trainer manager overlay", err)
	}
	return nil
}

const webhookServiceRefSuffix = " backed by a Service in the manifests"

func webhookEntryKey(kind, config, entry string) string {
	return kind + " " + config + " webhook " + entry
}

// checkTrainerManifestsSupported fails with ErrCodeInvalidRequest when the built
// manifests lack an object the installer or the benchmark depends on.
func checkTrainerManifestsSupported(objs []*unstructured.Unstructured, version string) error {
	found := map[string]bool{}
	serviceRefs := map[string]string{}
	for _, obj := range objs {
		switch obj.GetKind() {
		case "CustomResourceDefinition":
			versions, _, _ := unstructured.NestedSlice(obj.Object, "spec", "versions")
			for _, raw := range versions {
				if v, ok := raw.(map[string]any); ok && v["served"] == true {
					found[obj.GetName()+"@"+fmt.Sprint(v[keyName])] = true
				}
			}
		case kindDeployment, kindService:
			found[obj.GetKind()+" "+obj.GetNamespace()+"/"+obj.GetName()] = true
		case kindValidatingWebhook, kindMutatingWebhook:
			entries, _, _ := unstructured.NestedSlice(obj.Object, "webhooks")
			for _, raw := range entries {
				entry, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				key := webhookEntryKey(obj.GetKind(), obj.GetName(), fmt.Sprint(entry[keyName]))
				found[key] = true
				namespace, _, _ := unstructured.NestedString(entry, "clientConfig", "service", "namespace")
				service, _, _ := unstructured.NestedString(entry, "clientConfig", "service", keyName)
				if namespace != "" && service != "" {
					serviceRefs[key] = kindService + " " + namespace + "/" + service
				}
			}
		}
	}
	for key, service := range serviceRefs {
		found[key+webhookServiceRefSuffix] = found[service]
	}

	served := func(gvr schema.GroupVersionResource) string {
		return gvr.Resource + "." + gvr.Group + "@" + gvr.Version
	}
	// The installed-state probe locates the installation through the validating
	// entry's Service reference and requires that Service to exist.
	required := []string{
		served(trainJobGVR),
		served(trainingRuntimeGVR),
		webhookEntryKey(kindValidatingWebhook, trainerValidatingWebhookConfig,
			trainerValidatingWebhookName) + webhookServiceRefSuffix,
		webhookEntryKey(kindMutatingWebhook, trainerMutatingWebhookConfig, trainerMutatingWebhookName),
		kindDeployment + " " + trainerNamespace + "/" + trainerControllerDeployment,
	}
	var missing []string
	for _, r := range required {
		if !found[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"Kubeflow Trainer %s manifests lack what the NCCL self-installer needs: %s",
			version, strings.Join(missing, ", ")))
	}
	return nil
}
