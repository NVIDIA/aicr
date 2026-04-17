// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

const (
	// trainerArchiveURL is the GitHub tar.gz archive for Kubeflow Trainer v2.1.0.
	trainerArchiveURL = "https://github.com/kubeflow/trainer/archive/refs/tags/v2.1.0.tar.gz"

	// trainerKustomizePath is the path within the extracted archive to the manager overlay.
	trainerKustomizePath = "manifests/overlays/manager"

	// trainerCRDName is the CRD that signals the Trainer operator is installed.
	trainerCRDName = "trainjobs.trainer.kubeflow.org"

	// trainerControllerDeployment is the Deployment name for the Trainer controller-manager.
	trainerControllerDeployment = "kubeflow-trainer-controller-manager"

	// trainerNamespace is the namespace where the Trainer operator is installed.
	trainerNamespace = "kubeflow-system"

	// maxExtractedFileSize caps individual file sizes during tar extraction (50 MB).
	maxExtractedFileSize = 50 * 1024 * 1024
)

// trainerResourceRef identifies a Kubernetes resource applied during Trainer installation,
// so it can be deleted during cleanup.
type trainerResourceRef struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
}

// isTrainerInstalled returns true when the Kubeflow Trainer CRD is present in the cluster.
func isTrainerInstalled(ctx context.Context, dynamicClient dynamic.Interface) (bool, error) {
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	_, err := dynamicClient.Resource(crdGVR).Get(ctx, trainerCRDName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false, nil
		}
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to check for Kubeflow Trainer CRD", err)
	}
	return true, nil
}

// installTrainer downloads the Kubeflow Trainer v2.1.0 archive from GitHub, builds the
// kustomize manager overlay entirely in Go (no CLI), and applies every resource to the
// cluster via the dynamic client.  It returns the list of resources it created so the
// caller can defer deleteTrainer for cleanup.
func installTrainer(ctx context.Context, dynamicClient dynamic.Interface, discoveryClient discovery.DiscoveryInterface) ([]trainerResourceRef, error) {
	slog.Info("Downloading Kubeflow Trainer archive", "url", trainerArchiveURL)

	extractedDir, cleanup, err := downloadAndExtractGitHubArchive(ctx, trainerArchiveURL)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to download Trainer archive", err)
	}
	defer cleanup()

	kustomizePath := filepath.Join(extractedDir, trainerKustomizePath)
	slog.Info("Building Trainer kustomize manifests", "path", kustomizePath)

	// LoadRestrictionsNone lets krusty follow the ../../base references in the overlay.
	opts := krusty.MakeDefaultOptions()
	opts.LoadRestrictions = types.LoadRestrictionsNone

	k := krusty.MakeKustomizer(opts)
	fSys := filesys.MakeFsOnDisk()

	resMap, err := k.Run(fSys, kustomizePath)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to build Trainer manifests", err)
	}

	// Build a REST mapper from live discovery so we can resolve GVK → GVR for each resource.
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))

	applied := make([]trainerResourceRef, 0, len(resMap.Resources()))
	for _, res := range resMap.Resources() {
		// Convert to unstructured via YAML round-trip (guarantees plain Go types).
		yamlBytes, err := res.AsYAML()
		if err != nil {
			return applied, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to marshal Trainer resource to YAML", err)
		}

		obj := &unstructured.Unstructured{}
		if unmarshalErr := yaml.Unmarshal(yamlBytes, obj); unmarshalErr != nil {
			return applied, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse Trainer resource YAML", unmarshalErr)
		}

		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" {
			continue
		}

		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return applied, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("failed to resolve REST mapping for %s", gvk), err)
		}

		ref := trainerResourceRef{GVR: mapping.Resource, Name: obj.GetName()}

		applyCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
		if mapping.Scope.Name() == apimeta.RESTScopeNameNamespace {
			ref.Namespace = obj.GetNamespace()
			_, err = dynamicClient.Resource(mapping.Resource).Namespace(ref.Namespace).Create(applyCtx, obj, metav1.CreateOptions{})
		} else {
			_, err = dynamicClient.Resource(mapping.Resource).Create(applyCtx, obj, metav1.CreateOptions{})
		}
		cancel()

		if err != nil {
			if k8serrors.IsAlreadyExists(err) {
				// Update: enforce current resource state even when left from a prior partial install.
				updateCtx, updateCancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
				if mapping.Scope.Name() == apimeta.RESTScopeNameNamespace {
					_, err = dynamicClient.Resource(mapping.Resource).Namespace(ref.Namespace).Update(updateCtx, obj, metav1.UpdateOptions{})
				} else {
					_, err = dynamicClient.Resource(mapping.Resource).Update(updateCtx, obj, metav1.UpdateOptions{})
				}
				updateCancel()
				if err != nil {
					slog.Warn("Failed to update existing Trainer resource, continuing", "kind", gvk.Kind, "name", obj.GetName(), "error", err)
				} else {
					slog.Info("Updated existing Trainer resource", "kind", gvk.Kind, "name", obj.GetName())
				}
				continue
			}
			return applied, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("failed to create %s %q", gvk.Kind, obj.GetName()), err)
		}

		applied = append(applied, ref)
		slog.Info("Applied Trainer resource", "kind", gvk.Kind, "name", obj.GetName(), "namespace", ref.Namespace)
	}

	// Wait for the CRDs the NCCL test needs before returning.
	if err := waitForTrainerCRDsEstablished(ctx, dynamicClient); err != nil {
		return applied, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "Trainer CRDs not ready after install", err)
	}

	// Wait for the controller-manager to be ready so the ValidatingWebhookConfiguration
	// backing Service can serve requests before the caller creates TrainingRuntime resources.
	if err := waitForTrainerControllerReady(ctx, dynamicClient); err != nil {
		return applied, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "Trainer controller not ready after install", err)
	}

	return applied, nil
}

// deleteTrainer removes every resource that was created by installTrainer, in reverse
// application order so dependents are deleted before their owners.
// Uses context.Background() because the parent context may already be canceled at
// defer time; cleanup must still complete.
func deleteTrainer(dynamicClient dynamic.Interface, resources []trainerResourceRef) {
	slog.Info("Deleting installed Kubeflow Trainer resources", "count", len(resources))
	for _, ref := range slices.Backward(resources) {
		deleteCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)

		var err error
		if ref.Namespace != "" {
			err = dynamicClient.Resource(ref.GVR).Namespace(ref.Namespace).Delete(deleteCtx, ref.Name, metav1.DeleteOptions{})
		} else {
			err = dynamicClient.Resource(ref.GVR).Delete(deleteCtx, ref.Name, metav1.DeleteOptions{})
		}
		cancel()

		if err != nil && !k8serrors.IsNotFound(err) {
			slog.Error("Failed to delete Trainer resource", "gvr", ref.GVR.Resource, "name", ref.Name, "error", err)
		} else {
			slog.Info("Deleted Trainer resource", "gvr", ref.GVR.Resource, "name", ref.Name)
		}
	}
}

// waitForTrainerCRDsEstablished waits for the two CRDs that the NCCL test requires
// to reach the Established condition after Trainer installation.
func waitForTrainerCRDsEstablished(ctx context.Context, dynamicClient dynamic.Interface) error {
	crds := []string{
		"trainjobs.trainer.kubeflow.org",
		"trainingruntimes.trainer.kubeflow.org",
	}
	waitCtx, cancel := context.WithTimeout(ctx, defaults.TrainerCRDEstablishedTimeout)
	defer cancel()

	for _, crd := range crds {
		slog.Info("Waiting for Trainer CRD to be established", "crd", crd)
		if err := waitForCRDEstablished(waitCtx, dynamicClient, crd); err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, fmt.Sprintf("CRD %s not established", crd), err)
		}
	}
	return nil
}

// waitForTrainerControllerReady polls the controller-manager Deployment until at
// least one replica is ready, ensuring the ValidatingWebhookConfiguration can
// serve admission requests before the caller creates Trainer custom resources.
func waitForTrainerControllerReady(ctx context.Context, dynamicClient dynamic.Interface) error {
	slog.Info("Waiting for Trainer controller-manager to become ready",
		"deployment", trainerControllerDeployment, "namespace", trainerNamespace)

	deployGVR := schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "deployments",
	}

	waitCtx, cancel := context.WithTimeout(ctx, defaults.TrainerControllerReadyTimeout)
	defer cancel()

	for {
		deploy, err := dynamicClient.Resource(deployGVR).Namespace(trainerNamespace).
			Get(waitCtx, trainerControllerDeployment, metav1.GetOptions{})
		if err == nil {
			readyReplicas, _, _ := unstructured.NestedInt64(deploy.Object, "status", "readyReplicas")
			if readyReplicas >= 1 {
				slog.Info("Trainer controller-manager is ready", "readyReplicas", readyReplicas)
				return nil
			}
		}

		select {
		case <-waitCtx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
				"timed out waiting for Trainer controller-manager to become ready", waitCtx.Err())
		case <-time.After(defaults.TrainerControllerPollInterval):
		}
	}
}

// waitForCRDEstablished watches a CRD until its Established condition is True.
// It checks the current state first so the fast path (already established) returns
// immediately without starting a watch.
func waitForCRDEstablished(ctx context.Context, dynamicClient dynamic.Interface, crdName string) error {
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}

	existing, err := dynamicClient.Resource(crdGVR).Get(ctx, crdName, metav1.GetOptions{})
	if err == nil && isCRDEstablished(existing) {
		return nil
	}

	watcher, err := dynamicClient.Resource(crdGVR).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + crdName,
	})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch CRD", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for CRD to be established", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return aicrErrors.New(aicrErrors.ErrCodeInternal, "CRD watch channel closed unexpectedly")
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			if isCRDEstablished(obj) {
				slog.Info("CRD established", "crd", crdName)
				return nil
			}
		}
	}
}

// isCRDEstablished returns true when the CRD's status contains an Established condition
// with status "True".
func isCRDEstablished(obj *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		condition, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if condition["type"] == "Established" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

// downloadAndExtractGitHubArchive fetches a GitHub tar.gz release archive over HTTP and
// extracts it to a temp directory.  Returns the path to the top-level directory inside
// the archive and a cleanup function to remove the temp dir.
func downloadAndExtractGitHubArchive(ctx context.Context, archiveURL string) (string, func(), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to build request", err)
	}

	// Use a bounded HTTP client — http.DefaultClient has no timeout.
	client := defaults.NewHTTPClient(defaults.NCCLTrainerArchiveDownloadTimeout)
	resp, err := client.Do(req) //nolint:gosec // archiveURL is a compile-time constant, not user input
	if err != nil {
		return "", nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, fmt.Sprintf("failed to download archive from %s", archiveURL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("unexpected HTTP %d downloading %s", resp.StatusCode, archiveURL))
	}

	tmpDir, err := os.MkdirTemp("", "aicr-trainer-*")
	if err != nil {
		return "", nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create temp dir", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	if extractErr := extractTarGz(resp.Body, tmpDir); extractErr != nil {
		cleanup()
		return "", nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to extract archive", extractErr)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil || len(entries) == 0 {
		cleanup()
		return "", nil, aicrErrors.New(aicrErrors.ErrCodeInternal, "extracted archive is empty or unreadable")
	}

	return filepath.Join(tmpDir, entries[0].Name()), cleanup, nil
}

// extractTarGz decompresses and extracts a gzipped tar stream into targetDir.
func extractTarGz(r io.Reader, targetDir string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create gzip reader", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "tar read error", err)
		}

		path, err := sanitizeTarPath(targetDir, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0750); err != nil { //nolint:gosec // G703 -- path sanitized by sanitizeTarPath above
				return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, fmt.Sprintf("failed to create directory %s", path), err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil { //nolint:gosec // G703 -- path sanitized by sanitizeTarPath above
				return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, fmt.Sprintf("failed to create parent dir for %s", path), err)
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640) //nolint:gosec // G703 -- path sanitized by sanitizeTarPath above
			if err != nil {
				return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, fmt.Sprintf("failed to create file %s", path), err)
			}
			_, copyErr := io.Copy(f, io.LimitReader(tr, maxExtractedFileSize))
			closeErr := f.Close()
			if copyErr != nil {
				return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, fmt.Sprintf("failed to write file %s", path), copyErr)
			}
			if closeErr != nil {
				return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, fmt.Sprintf("failed to close file %s", path), closeErr)
			}
		}
	}
	return nil
}

// sanitizeTarPath validates a tar entry path against the target directory to prevent
// path traversal attacks.
func sanitizeTarPath(targetDir, entryPath string) (string, error) {
	cleanPath := filepath.Join(targetDir, filepath.FromSlash(entryPath))
	if !strings.HasPrefix(cleanPath, filepath.Clean(targetDir)+string(os.PathSeparator)) {
		return "", aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, fmt.Sprintf("invalid tar entry %q: potential path traversal", entryPath))
	}
	return cleanPath, nil
}
