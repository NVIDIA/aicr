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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func trainerRefCtx(refs ...recipe.ComponentRef) *validators.Context {
	return &validators.Context{
		ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{ComponentRefs: refs}),
	}
}

func TestResolveTrainerVersion(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	registryPin := registry.Get(kubeflowTrainerComponent).Helm.DefaultVersion

	disabledRef := func(v string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: kubeflowTrainerComponent, Version: v,
			Overrides: map[string]any{"enabled": false}}
	}
	hydratedDisabledRef := disabledRef("")
	hydratedDisabledRef.ApplyRegistryDefaults(registry.Get(kubeflowTrainerComponent))
	tests := []struct {
		name    string
		ctx     *validators.Context
		env     string
		want    string
		wantErr bool
	}{
		{name: "nil context uses registry pin", ctx: nil, want: registryPin},
		{name: "no recipe version or env uses registry pin", ctx: trainerRefCtx(), want: registryPin},
		{name: "blank env falls through to registry pin", ctx: trainerRefCtx(), env: "   ", want: registryPin},
		{name: "env beats registry pin", ctx: trainerRefCtx(), env: "2.3.0", want: "2.3.0"},
		{name: "recipe version beats env", ctx: trainerRefCtx(disabledRef("2.4.1")), env: "2.3.0", want: "2.4.1"},
		{name: "resolved recipe ref carries registry pin and beats env",
			ctx: trainerRefCtx(hydratedDisabledRef), env: "2.3.0", want: registryPin},
		{name: "leading v accepted", ctx: trainerRefCtx(), env: "v2.3.0", want: "2.3.0"},
		{name: "prerelease accepted", ctx: trainerRefCtx(), env: "2.3.0-rc.0", want: "2.3.0-rc.0"},
		{name: "non-semver rejected", ctx: trainerRefCtx(), env: "latest", wantErr: true},
		{name: "partial version rejected", ctx: trainerRefCtx(), env: "2.3", wantErr: true},
		{name: "path injection rejected", ctx: trainerRefCtx(), env: "2.2.0/../../evil", wantErr: true},
		{name: "invalid recipe version rejected", ctx: trainerRefCtx(disabledRef("not-a-version")), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(trainerVersionEnv, tt.env)
			got, err := resolveTrainerVersion(tt.ctx)
			if tt.wantErr {
				if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
					t.Fatalf("resolveTrainerVersion() = %q, %v; want ErrCodeInvalidRequest", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("resolveTrainerVersion() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func trainerManifestObj(kind, namespace, name string, servedVersions ...string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"kind": kind}}
	obj.SetName(name)
	obj.SetNamespace(namespace)
	if len(servedVersions) > 0 {
		versions := make([]any, 0, len(servedVersions))
		for _, v := range servedVersions {
			versions = append(versions, map[string]any{"name": v, "served": true})
		}
		obj.Object["spec"] = map[string]any{"versions": versions}
	}
	return obj
}

func TestCheckTrainerManifestsSupported(t *testing.T) {
	crd := func(name, version string) *unstructured.Unstructured {
		return trainerManifestObj("CustomResourceDefinition", "", name, version)
	}
	complete := func() []*unstructured.Unstructured {
		return []*unstructured.Unstructured{
			crd(trainerCRDTrainJobs, trainJobGVR.Version),
			crd(trainerCRDTrainingRuntimes, trainingRuntimeGVR.Version),
			webhookConfigIn(kindValidatingWebhook, trainerValidatingWebhookConfig,
				trainerValidatingWebhookName, trainerNamespace),
			webhookConfigIn(kindMutatingWebhook, trainerMutatingWebhookConfig,
				trainerMutatingWebhookName, trainerNamespace),
			trainerManifestObj("Deployment", trainerNamespace, trainerControllerDeployment),
			trainerManifestObj(kindService, trainerNamespace, trainerControllerService),
		}
	}
	without := func(i int) []*unstructured.Unstructured {
		objs := complete()
		return append(objs[:i], objs[i+1:]...)
	}
	withEntry := func(i int, entry map[string]any) []*unstructured.Unstructured {
		objs := complete()
		objs[i].Object["webhooks"] = []any{entry}
		return objs
	}

	unserved := complete()
	unserved[0].Object["spec"] = map[string]any{"versions": []any{
		map[string]any{"name": trainJobGVR.Version, "served": false},
	}}
	otherNamespace := complete()
	otherNamespace[4].SetNamespace("elsewhere")
	otherServiceNamespace := complete()
	otherServiceNamespace[5].SetNamespace("elsewhere")
	urlBacked := map[string]any{
		keyName:        trainerValidatingWebhookName,
		"clientConfig": map[string]any{"url": "https://example.invalid/validate"},
	}

	tests := []struct {
		name        string
		objs        []*unstructured.Unstructured
		wantMissing string
	}{
		{name: "complete manifest set", objs: complete()},
		{name: "TrainJob CRD at another version only", wantMissing: trainerCRDTrainJobs,
			objs: append(without(0), crd(trainerCRDTrainJobs, "v9"))},
		{name: "TrainJob version present but not served", objs: unserved, wantMissing: trainerCRDTrainJobs},
		{name: "TrainingRuntime CRD missing", objs: without(1), wantMissing: trainerCRDTrainingRuntimes},
		{name: "validating webhook configuration renamed", objs: without(2), wantMissing: trainerValidatingWebhookConfig},
		{name: "mutating webhook configuration renamed", objs: without(3), wantMissing: trainerMutatingWebhookConfig},
		{name: "validating webhook entry renamed", wantMissing: trainerValidatingWebhookName,
			objs: withEntry(2, map[string]any{keyName: "renamed.trainer.kubeflow.org"})},
		{name: "mutating webhook entry renamed", wantMissing: trainerMutatingWebhookName,
			objs: withEntry(3, map[string]any{keyName: "renamed.trainer.kubeflow.org"})},
		{name: "validating webhook entry without a Service reference", objs: withEntry(2, urlBacked),
			wantMissing: trainerValidatingWebhookName + webhookServiceRefSuffix},
		{name: "referenced Service missing", objs: without(5),
			wantMissing: trainerValidatingWebhookName + webhookServiceRefSuffix},
		{name: "referenced Service in another namespace", objs: otherServiceNamespace,
			wantMissing: trainerValidatingWebhookName + webhookServiceRefSuffix},
		{name: "controller in another namespace", objs: otherNamespace, wantMissing: trainerControllerDeployment},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkTrainerManifestsSupported(tt.objs, "9.9.9")
			if tt.wantMissing == "" {
				if err != nil {
					t.Fatalf("checkTrainerManifestsSupported() = %v, want nil", err)
				}
				return
			}
			if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
				t.Fatalf("checkTrainerManifestsSupported() = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.wantMissing) {
				t.Errorf("error %q does not name %q", err, tt.wantMissing)
			}
		})
	}
}

func TestCheckTrainerOverlay(t *testing.T) {
	root := t.TempDir()
	overlay := filepath.Join(root, trainerKustomizePath)
	if err := os.MkdirAll(overlay, 0o750); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(root, "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "overlay directory present", path: overlay},
		{name: "overlay missing", path: filepath.Join(root, "missing"), wantErr: true},
		{name: "overlay is a file", path: notDir, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkTrainerOverlay(tt.path, "1.9.4")
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("checkTrainerOverlay() = %v, want nil", err)
				}
				return
			}
			if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
				t.Fatalf("checkTrainerOverlay() = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestDownloadTrainerArchiveNotFoundIsInvalidRequest(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, _, err := downloadAndExtractGitHubArchive(context.Background(), srv.URL)
	if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Fatalf("downloadAndExtractGitHubArchive() error = %v, want ErrCodeInvalidRequest", err)
	}
}
