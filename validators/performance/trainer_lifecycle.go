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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
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
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

const (
	// trainerArchiveURL is the GitHub tar.gz archive for Kubeflow Trainer v2.2.0.
	trainerArchiveURL = "https://github.com/kubeflow/trainer/archive/refs/tags/v2.2.0.tar.gz"

	// trainerKustomizePath is the path within the extracted archive to the manager overlay.
	trainerKustomizePath = "manifests/overlays/manager"

	// trainerCRDTrainJobs and trainerCRDTrainingRuntimes are the CRDs the NCCL
	// benchmark needs. Both must be Established for the installation to count.
	trainerCRDTrainJobs        = "trainjobs.trainer.kubeflow.org"
	trainerCRDTrainingRuntimes = "trainingruntimes.trainer.kubeflow.org"

	// trainerControllerDeployment is the Deployment name for the Trainer controller-manager.
	trainerControllerDeployment = "kubeflow-trainer-controller-manager"

	// trainerControllerService is the Service fronting the controller-manager's
	// webhook port. Without it the admission webhooks have no endpoints and every
	// TrainJob create is rejected.
	trainerControllerService = "kubeflow-trainer-controller-manager"

	// trainerNamespace is the namespace this validator's self-install uses. It is
	// NOT the only layout: the kubeflow-trainer Helm chart the recipes deploy pins
	// defaultNamespace: kubeflow (recipes/registry.yaml). The probe therefore
	// discovers the live namespace rather than assuming this one.
	trainerNamespace = "kubeflow-system"

	// trainerValidatingWebhookConfig and trainerMutatingWebhookConfig are the
	// admission configurations both supported deployment paths emit. The generic
	// kubebuilder names in base/webhook/manifests.yaml never reach a cluster:
	// base/webhook/kustomization.yaml patches /metadata/name to these, and the
	// Helm chart renders the same two names.
	trainerValidatingWebhookConfig = "validator.trainer.kubeflow.org"
	trainerMutatingWebhookConfig   = "defaulter.trainer.kubeflow.org"

	// trainerValidatingWebhookName and trainerMutatingWebhookName are Trainer-owned
	// entries inside those configurations, used to tell a real Trainer install from
	// an unrelated operator that happened to claim the same generic object name.
	trainerValidatingWebhookName = "validator.trainjob.trainer.kubeflow.org"
	trainerMutatingWebhookName   = "defaulter.trainjob.trainer.kubeflow.org"

	// trainerWebhookSuffix marks a webhook entry as Trainer-owned. Used to refuse
	// overwriting a generically-named admission configuration another operator owns.
	trainerWebhookSuffix = ".trainer.kubeflow.org"

	// jobSetCRDName identifies the JobSet dependency. TrainJobs run as JobSets, so
	// a Trainer whose JobSet controller never becomes ready fails opaquely later.
	jobSetCRDName = "jobsets.jobset.x-k8s.io"

	// trainerManagerLabels locate the Trainer controller Deployment across both
	// deployment paths. Neither its name nor app.kubernetes.io/name is stable: the
	// chart derives the name from the release ({{ .Release.Name }}-controller-manager)
	// and labels it app.kubernetes.io/name=kubeflow-trainer, while the kustomize
	// overlay uses a fixed name and app.kubernetes.io/name=trainer. These two labels
	// are set identically by both.
	trainerComponentLabel = "app.kubernetes.io/component"
	trainerComponentValue = "manager"
	trainerPartOfLabel    = "app.kubernetes.io/part-of"
	trainerPartOfValue    = "kubeflow"

	// installAttemptAnnotation marks objects created by one installation attempt, so
	// an ambiguous Create can only claim an object this attempt actually created.
	installAttemptAnnotation = "aicr.nvidia.com/install-attempt"

	// jobSetNameLabel/jobSetLabelValue locate the JobSet controller Deployment.
	// A name lookup cannot work across both deployment paths: the kustomize overlay
	// emits jobset-controller-manager, while the Helm chart's JobSet subchart derives
	// its name from the release ({{ .Release.Name }}-jobset-controller). Both paths
	// do set this label, so it is the only stable handle.
	jobSetNameLabel  = "app.kubernetes.io/name"
	jobSetLabelValue = "jobset"

	// maxExtractedFileSize caps individual file sizes during tar extraction (50 MB).
	maxExtractedFileSize = 50 * 1024 * 1024

	// jobSetStagingImageRepo is the JobSet controller image repository referenced by the
	// upstream Kubeflow Trainer v2.2.0 manifests. It points at the Kubernetes staging
	// registry, whose tags are garbage-collected (MANIFEST_UNKNOWN/404), so jobset-controller-manager
	// lands in ImagePullBackOff and its admission webhook has no endpoints.
	jobSetStagingImageRepo = "us-central1-docker.pkg.dev/k8s-staging-images/jobset/jobset"

	// jobSetPromotedImageRepo is the promoted, permanent JobSet image repository on the
	// production registry. The same tag (e.g. v0.11.0) exists here, so rewriting the repo
	// prefix is sufficient to make the controller pullable.
	jobSetPromotedImageRepo = "registry.k8s.io/jobset/jobset"
)

// GVRs for the objects the Trainer lifecycle probes and waits on.
var (
	trainerCRDGVR = schema.GroupVersionResource{
		Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs,
	}
	trainerDeploymentGVR = schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "deployments",
	}
	trainerServiceGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "services",
	}
	trainerValidatingWebhookGVR = schema.GroupVersionResource{
		Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations",
	}
	trainerMutatingWebhookGVR = schema.GroupVersionResource{
		Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations",
	}

	// requiredTrainerCRDs are the CRDs a usable Trainer install implies: the two the
	// benchmark consumes directly, plus JobSet, which TrainJobs are executed as.
	// Kept as one list so the installed-state probe and the post-install wait
	// cannot drift.
	requiredTrainerCRDs = []string{trainerCRDTrainJobs, trainerCRDTrainingRuntimes, jobSetCRDName}
)

// trainerInstall describes a live Kubeflow Trainer installation the probe found,
// including where it lives. The namespace is discovered rather than assumed
// because the self-install overlay and the Helm chart use different ones.
type trainerInstall struct {
	Namespace  string
	Service    string
	Deployment string
}

// trainerResourceRef identifies a Kubernetes resource applied during Trainer installation,
// so it can be deleted during cleanup.
type trainerResourceRef struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string

	// UID of the object this installation created. Cleanup passes it as a delete
	// precondition, so a same-named object recreated by another owner during a
	// long benchmark is never deleted on our behalf.
	UID k8stypes.UID
}

// String renders the resource identity for cleanup diagnostics.
func (r trainerResourceRef) String() string {
	if r.Namespace != "" {
		return fmt.Sprintf("%s %s/%s", r.GVR.Resource, r.Namespace, r.Name)
	}
	return fmt.Sprintf("%s %s", r.GVR.Resource, r.Name)
}

// isTrainerInstalled reports whether a complete Kubeflow Trainer installation is
// present: every CRD the benchmark needs is Established, the controller-manager
// Deployment and its webhook Service exist, and both admission configurations
// carry Trainer-owned webhook entries.
//
// A single-CRD probe is not enough. A failed install can leave that one CRD
// behind, and a later run would then drive TrainJobs at a controller that was
// never created (issue #2123). Anything short of the full set reports false so
// the caller reinstalls, and only an API error the probe cannot classify is
// returned as an error.
func isTrainerInstalled(ctx context.Context, dynamicClient dynamic.Interface) (trainerInstall, bool, error) {
	for _, crd := range requiredTrainerCRDs {
		obj, found, err := getTrainerObject(ctx, dynamicClient, trainerCRDGVR, "", crd)
		if err != nil {
			return trainerInstall{}, false, err
		}
		if !found {
			slog.Info("Kubeflow Trainer incomplete: CRD missing", "crd", crd)
			return trainerInstall{}, false, nil
		}
		if !isCRDEstablished(obj) {
			slog.Info("Kubeflow Trainer incomplete: CRD not established", "crd", crd)
			return trainerInstall{}, false, nil
		}
	}

	// The validating configuration is the authority on where Trainer lives: its
	// webhook entries name the controller Service and its namespace, which is what
	// makes this work for both the self-install overlay and the Helm chart.
	install, found, err := discoverTrainerInstall(ctx, dynamicClient,
		trainerValidatingWebhookGVR, trainerValidatingWebhookConfig, trainerValidatingWebhookName)
	if err != nil || !found {
		return trainerInstall{}, false, err
	}

	ok, err := hasTrainerWebhook(ctx, dynamicClient,
		trainerMutatingWebhookGVR, trainerMutatingWebhookConfig, trainerMutatingWebhookName)
	if err != nil {
		return trainerInstall{}, false, err
	}
	if !ok {
		slog.Info("Kubeflow Trainer incomplete: admission webhook missing",
			"configuration", trainerMutatingWebhookConfig, "webhook", trainerMutatingWebhookName)
		return trainerInstall{}, false, nil
	}

	// The controller Deployment is found by label: its name is release-derived on
	// the Helm path, so a hardcoded name would misreport a non-default release as
	// incomplete.
	controller, found, err := findTrainerController(ctx, dynamicClient, install.Namespace)
	if err != nil {
		return trainerInstall{}, false, err
	}
	if !found {
		slog.Info("Kubeflow Trainer incomplete: controller Deployment missing",
			"namespace", install.Namespace)
		return trainerInstall{}, false, nil
	}
	install.Deployment = controller

	if _, found, err := getTrainerObject(ctx, dynamicClient, trainerServiceGVR,
		install.Namespace, install.Service); err != nil {
		return trainerInstall{}, false, err
	} else if !found {
		slog.Info("Kubeflow Trainer incomplete: controller Service missing",
			"namespace", install.Namespace, "name", install.Service)
		return trainerInstall{}, false, nil
	}

	slog.Info("Kubeflow Trainer installation is complete",
		"namespace", install.Namespace, "service", install.Service, "deployment", install.Deployment)
	return install, true, nil
}

// discoverTrainerInstall locates a Trainer installation from its admission
// configuration, which names the controller Service and the namespace it lives in.
// Returns nil when the configuration is absent or carries no Trainer webhook.
//
// Discovery beats a hardcoded namespace because the two supported deployment paths
// disagree: the self-install kustomize overlay uses kubeflow-system, while the
// kubeflow-trainer Helm chart the recipes deploy uses kubeflow. Assuming either one
// reports the other as incomplete and triggers a reinstall that would rewrite the
// live installation's cluster-scoped objects to point at the wrong namespace.
func discoverTrainerInstall(ctx context.Context, dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource, configName, webhookName string) (trainerInstall, bool, error) {

	obj, found, err := getTrainerObject(ctx, dynamicClient, gvr, "", configName)
	if err != nil || !found {
		if err == nil {
			slog.Info("Kubeflow Trainer incomplete: admission configuration missing",
				"configuration", configName)
		}
		return trainerInstall{}, false, err
	}

	entries, _, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		return trainerInstall{}, false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to read webhooks from %s %q", gvr.Resource, configName), err)
	}

	for _, e := range entries {
		entry, ok := e.(map[string]interface{})
		if !ok || entry[keyName] != webhookName {
			continue
		}
		namespace, _, _ := unstructured.NestedString(entry, "clientConfig", "service", "namespace")
		service, _, _ := unstructured.NestedString(entry, "clientConfig", "service", keyName)
		if namespace == "" || service == "" {
			// A URL-backed webhook has no Service to locate. Treat as unusable
			// rather than guessing a namespace and reinstalling on top of it.
			slog.Info("Kubeflow Trainer webhook has no Service reference; cannot locate the installation",
				"configuration", configName, "webhook", webhookName)
			return trainerInstall{}, false, nil
		}
		return trainerInstall{Namespace: namespace, Service: service}, true, nil
	}

	slog.Info("Kubeflow Trainer incomplete: admission webhook missing",
		"configuration", configName, "webhook", webhookName)
	return trainerInstall{}, false, nil
}

// getTrainerObject fetches one object. NotFound reports found=false with no
// error, so callers can distinguish "absent" from "could not tell" and fail
// closed on the latter.
func getTrainerObject(ctx context.Context, dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource, namespace, name string) (obj *unstructured.Unstructured, found bool, err error) {

	// Single choke point for every read the probe makes, so this one check
	// covers each of its loops: stop as soon as the caller gives up rather than
	// working through the remaining resources.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
			fmt.Sprintf("canceled before checking %s %q", gvr.Resource, name), ctxErr)
	}

	getCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	obj, err = trainerResourceClient(dynamicClient, gvr, namespace).Get(getCtx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		// An object a concurrent run is tearing down is on its way out, so treat
		// it as absent rather than waiting on a dying installation.
		if obj.GetDeletionTimestamp() != nil {
			slog.Info("Trainer resource is terminating; treating as absent",
				"resource", gvr.Resource, "namespace", namespace, "name", name)
			return nil, false, nil
		}
		return obj, true, nil
	case k8serrors.IsNotFound(err):
		return nil, false, nil
	default:
		return nil, false, aicrErrors.Wrap(trainerAPIErrorCode(err),
			fmt.Sprintf("failed to check for %s %q", gvr.Resource, name), err)
	}
}

// trainerAPIErrorCode classifies a failed Kubernetes API call on any Trainer
// lifecycle path (probe, apply, teardown). These errors reach the validation
// verdict, so collapsing everything to Internal would report a transient cluster
// outage as a product defect.
func trainerAPIErrorCode(err error) aicrErrors.ErrorCode {
	switch {
	case aicrErrors.IsTransient(err):
		// Parent cancellation or our own DiagnosticTimeout expiring.
		return aicrErrors.ErrCodeTimeout
	case aicrErrors.IsNetworkError(err),
		k8serrors.IsServiceUnavailable(err),
		k8serrors.IsTooManyRequests(err),
		k8serrors.IsServerTimeout(err),
		k8serrors.IsTimeout(err):
		// The cluster is unreachable or shedding load, not a code fault.
		return aicrErrors.ErrCodeUnavailable
	default:
		return aicrErrors.ErrCodeInternal
	}
}

// hasTrainerWebhook reports whether the named admission configuration exists and
// serves the given Trainer webhook. The name check matters because the upstream
// manifests use generic, unprefixed configuration names that another operator on
// the cluster may already own.
func hasTrainerWebhook(ctx context.Context, dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource, configName, webhookName string) (bool, error) {

	obj, found, err := getTrainerObject(ctx, dynamicClient, gvr, "", configName)
	if err != nil || !found {
		return false, err
	}

	entries, _, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to read webhooks from %s %q", gvr.Resource, configName), err)
	}
	for _, e := range entries {
		entry, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if entry[keyName] == webhookName {
			return true, nil
		}
	}
	return false, nil
}

// trainerResourceClient returns the namespaced or cluster-scoped client for gvr.
func trainerResourceClient(dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource, namespace string) dynamic.ResourceInterface {

	if namespace != "" {
		return dynamicClient.Resource(gvr).Namespace(namespace)
	}
	return dynamicClient.Resource(gvr)
}

// ensureTrainerInstalled makes a usable Kubeflow Trainer available to the benchmark
// and returns the resources it created, so the caller can delete them when the run
// finishes. A complete pre-existing installation is left alone and reported as no
// resources, so the benchmark never deletes a Trainer it does not own.
func ensureTrainerInstalled(ctx context.Context, dynamicClient dynamic.Interface,
	discoveryClient discovery.DiscoveryInterface) ([]trainerResourceRef, error) {

	install, installed, err := isTrainerInstalled(ctx, dynamicClient)
	if err != nil {
		// PropagateOrWrap, not Wrap: the probe already classified this as
		// Unavailable or Timeout where it could, and verdict consumers read the
		// outermost code. Overwriting it here would report a transient
		// control-plane outage as a product defect.
		return nil, aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeInternal,
			"failed to check Kubeflow Trainer installation")
	}

	if !installed {
		// Before applying anything, check for a live installation somewhere else.
		// Kustomize applies CRDs and RBAC before webhook configurations, so by the
		// time the admission-config ownership guard could fire, a shared-name
		// ClusterRoleBinding would already have been repointed at our namespace —
		// and updates are excluded from the rollback set, so nothing restores it.
		// Refusing up front is the only point at which this is still reversible.
		if live, found, derr := discoverTrainerInstall(ctx, dynamicClient,
			trainerValidatingWebhookGVR, trainerValidatingWebhookConfig,
			trainerValidatingWebhookName); derr != nil {
			return nil, derr
		} else if found && live.Namespace != trainerNamespace {
			return nil, aicrErrors.New(aicrErrors.ErrCodeConflict, fmt.Sprintf(
				"a Kubeflow Trainer installation exists in namespace %q but is incomplete; "+
					"refusing to install into %q because that would rewrite its shared cluster-scoped resources",
				live.Namespace, trainerNamespace))
		}

		slog.Info("Kubeflow Trainer not found or incomplete, installing...")
		// installTrainer rolls back its own resources on failure, so there is
		// nothing to clean up on the error path.
		created, installErr := installTrainer(ctx, dynamicClient, discoveryClient)
		if installErr != nil {
			return nil, aicrErrors.PropagateOrWrap(installErr, aicrErrors.ErrCodeInternal,
				"failed to install Kubeflow Trainer")
		}
		slog.Info("Kubeflow Trainer installed", "resources", len(created))
		return created, nil
	}

	// The probe confirms every object exists; the controller may still be rolling.
	// Wait for it here rather than reinstalling over a healthy Trainer we do not own.
	slog.Info("Kubeflow Trainer already installed, waiting for controller readiness",
		"namespace", install.Namespace, "deployment", install.Deployment)
	if readyErr := waitForTrainerReady(ctx, dynamicClient, install.Namespace, install.Deployment); readyErr != nil {
		return nil, aicrErrors.PropagateOrWrap(readyErr, aicrErrors.ErrCodeTimeout,
			"pre-existing Kubeflow Trainer controller is not ready")
	}
	return nil, nil
}

// foldCleanupError decides the check's verdict when teardown fails. A cleanup
// failure leaks cluster-scoped CRDs, RBAC, and webhook configurations that would
// silently poison the next run, so it fails an otherwise-passing check — but it
// never masks a real benchmark failure, which is always the more useful signal.
func foldCleanupError(benchErr, cleanupErr error) error {
	if cleanupErr == nil || benchErr != nil {
		return benchErr
	}
	return aicrErrors.PropagateOrWrap(cleanupErr, aicrErrors.ErrCodeInternal,
		"NCCL benchmark succeeded but Kubeflow Trainer cleanup failed")
}

// installTrainer downloads the Kubeflow Trainer v2.2.0 archive from GitHub, builds the
// kustomize manager overlay entirely in Go (no CLI), and applies every resource to the
// cluster via the dynamic client.
//
// Installation is transactional: on success it returns the resources it created so
// the caller can defer deleteTrainer for cleanup; on any failure it rolls those
// resources back itself and returns none.
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

	objs, err := decodeTrainerObjects(resMap.Resources())
	if err != nil {
		return nil, err
	}

	// Build a REST mapper from live discovery so we can resolve GVK → GVR for each resource.
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))

	return installTrainerResources(ctx, dynamicClient, mapper, objs)
}

// decodeTrainerObjects converts kustomize build output into unstructured objects,
// repointing the JobSet controller image off the garbage-collected staging
// registry (issue #1430). Resources without a Kind are skipped. Decoding happens
// before the first apply so a malformed manifest cannot leave a partial install.
func decodeTrainerObjects(resources []*resource.Resource) ([]*unstructured.Unstructured, error) {
	objs := make([]*unstructured.Unstructured, 0, len(resources))
	for _, res := range resources {
		// Convert to unstructured via YAML round-trip (guarantees plain Go types).
		yamlBytes, err := res.AsYAML()
		if err != nil {
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to marshal Trainer resource to YAML", err)
		}

		yamlBytes = rewriteJobSetStagingImage(yamlBytes)

		obj := &unstructured.Unstructured{}
		if unmarshalErr := yaml.Unmarshal(yamlBytes, obj); unmarshalErr != nil {
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse Trainer resource YAML", unmarshalErr)
		}
		if obj.GroupVersionKind().Kind == "" {
			continue
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

// installTrainerResources applies objs and waits until the Trainer dependency is
// usable, rolling back every resource it created if any step fails. It is the
// cluster-only core of installTrainer, split out so failure injection is testable
// without reaching GitHub for the archive.
//
// On success it returns the resources it created, for the caller to delete once
// the benchmark finishes. On failure it returns no resources: rollback already
// removed them, so the caller has nothing left to clean up.
func installTrainerResources(ctx context.Context, dynamicClient dynamic.Interface,
	mapper apimeta.RESTMapper, objs []*unstructured.Unstructured) ([]trainerResourceRef, error) {

	created, err := applyTrainerResources(ctx, dynamicClient, mapper, objs)
	if err == nil {
		// No discovered name here: these objects were just applied from the overlay,
		// so the controller carries its fixed self-install name.
		err = waitForTrainerReady(ctx, dynamicClient, trainerNamespace, "")
	}
	if err != nil {
		// contextcheck: rollback deliberately runs on a fresh context. ctx is
		// usually the reason we are here (deadline exceeded), and cleanup of
		// cluster-scoped resources must still complete.
		return nil, rollbackTrainer(dynamicClient, created, err) //nolint:contextcheck
	}

	return created, nil
}

// waitForTrainerReady blocks until a freshly applied Trainer is usable: the CRDs
// the NCCL test needs are Established, and the controller-manager has a ready
// replica so its admission webhooks have endpoints.
//
// deployment is the controller-manager's live name. The probe discovers it by
// label because the Helm chart derives the name from the release, so a caller
// that already located the installation must pass what it found rather than let
// this fall back to the fixed self-install name. Empty means "not discovered"
// (the post-install path, which applied the overlay's own fixed name).
func waitForTrainerReady(ctx context.Context, dynamicClient dynamic.Interface, namespace, deployment string) error {
	if err := waitForTrainerCRDsEstablished(ctx, dynamicClient); err != nil {
		return aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeTimeout, "Trainer CRDs not ready after install")
	}
	if err := waitForTrainerControllerReady(ctx, dynamicClient, namespace, deployment); err != nil {
		return aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeTimeout, "Trainer controller not ready after install")
	}
	// TrainJobs run as JobSets. A stale install still carrying the garbage-collected
	// staging image (issue #1430) satisfies every other signal, so without this the
	// failure only surfaces later as a TrainJob whose JobSet is never created.
	if err := waitForJobSetControllerReady(ctx, dynamicClient, namespace); err != nil {
		return aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeTimeout, "JobSet controller not ready")
	}
	return nil
}

// waitForJobSetControllerReady waits for the JobSet controller the Trainer manager
// overlay bundles. The overlay allows that resource to be omitted when JobSet is
// managed separately, so an absent controller is not a failure — only a present
// one that never becomes ready.
func waitForJobSetControllerReady(ctx context.Context, dynamicClient dynamic.Interface, namespace string) error {
	name, found, err := findJobSetController(ctx, dynamicClient, namespace)
	if err != nil {
		return err
	}
	if !found {
		slog.Debug("No JobSet controller alongside Trainer; assuming JobSet is managed elsewhere",
			"namespace", namespace)
		return nil
	}
	return waitForDeploymentReady(ctx, dynamicClient, namespace,
		name, defaults.TrainerControllerReadyTimeout)
}

// findJobSetController locates the JobSet controller Deployment beside Trainer by
// label, since its name differs between deployment paths and the Helm one is
// release-derived.
func findJobSetController(ctx context.Context, dynamicClient dynamic.Interface,
	namespace string) (string, bool, error) {

	return findDeploymentByLabels(ctx, dynamicClient, namespace,
		map[string]string{jobSetNameLabel: jobSetLabelValue}, "JobSet controller")
}

// findTrainerController locates the Trainer controller-manager Deployment by label
// rather than by name, which is release-derived on the Helm path.
func findTrainerController(ctx context.Context, dynamicClient dynamic.Interface,
	namespace string) (string, bool, error) {

	return findDeploymentByLabels(ctx, dynamicClient, namespace, map[string]string{
		trainerComponentLabel: trainerComponentValue,
		trainerPartOfLabel:    trainerPartOfValue,
	}, "Trainer controller")
}

// findDeploymentByLabels returns the name of a live Deployment matching labels. The
// selector is sent to the apiserver and re-checked on the result, because a specific
// object is then selected from the response.
func findDeploymentByLabels(ctx context.Context, dynamicClient dynamic.Interface,
	namespace string, labels map[string]string, what string) (string, bool, error) {

	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	selector := make([]string, 0, len(labels))
	for k, v := range labels {
		selector = append(selector, k+"="+v)
	}
	slices.Sort(selector)

	list, err := dynamicClient.Resource(trainerDeploymentGVR).Namespace(namespace).
		List(listCtx, metav1.ListOptions{LabelSelector: strings.Join(selector, ",")})
	if err != nil {
		return "", false, aicrErrors.Wrap(trainerAPIErrorCode(err),
			fmt.Sprintf("failed to list Deployments in %q while locating the %s", namespace, what), err)
	}

	for i := range list.Items {
		item := &list.Items[i]
		if item.GetDeletionTimestamp() != nil {
			continue
		}
		got := item.GetLabels()
		matched := true
		for k, v := range labels {
			if got[k] != v {
				matched = false
				break
			}
		}
		if matched {
			return item.GetName(), true, nil
		}
	}
	return "", false, nil
}

// applyTrainerResources creates each object, updating any that already exist.
//
// The returned list holds only the resources this call actually created. Objects
// that were already present are updated but deliberately excluded, so a rollback
// never deletes a Trainer another owner installed.
func applyTrainerResources(ctx context.Context, dynamicClient dynamic.Interface,
	mapper apimeta.RESTMapper, objs []*unstructured.Unstructured) ([]trainerResourceRef, error) {

	attemptID, err := newInstallAttemptID()
	if err != nil {
		return nil, err
	}

	created := make([]trainerResourceRef, 0, len(objs))
	for _, obj := range objs {
		// Abort the moment the caller gives up rather than issuing a Create per
		// remaining object and relying on each API call to fail on its own. The
		// error routes through the normal path, so rollback still runs.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return created, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
				"Trainer installation canceled before all resources were applied", ctxErr)
		}

		gvk := obj.GroupVersionKind()

		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return created, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("failed to resolve REST mapping for %s", gvk), err)
		}

		ref := trainerResourceRef{GVR: mapping.Resource, Name: obj.GetName()}
		if mapping.Scope.Name() == apimeta.RESTScopeNameNamespace {
			ref.Namespace = obj.GetNamespace()
		}
		client := trainerResourceClient(dynamicClient, mapping.Resource, ref.Namespace)

		// Stamp the attempt marker only on the create, so an ambiguous failure can
		// prove the object is ours. The update path below deliberately applies the
		// unstamped object: we do not mark resources we did not create.
		stamped := obj.DeepCopy()
		annotations := stamped.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[installAttemptAnnotation] = attemptID
		stamped.SetAnnotations(annotations)

		applyCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
		createdObj, err := client.Create(applyCtx, stamped, metav1.CreateOptions{})
		cancel()

		switch {
		case err == nil:
			ref.UID = createdObj.GetUID()
			created = append(created, ref)
			slog.Info("Applied Trainer resource", "kind", gvk.Kind, "name", ref.Name, "namespace", ref.Namespace)
		case k8serrors.IsAlreadyExists(err):
			// Enforce current resource state even when left from a prior partial
			// install. A failure here aborts: continuing would drive the benchmark
			// at a Trainer whose configuration we could not confirm.
			if updateErr := updateExistingTrainerResource(ctx, client, obj); updateErr != nil {
				return created, aicrErrors.PropagateOrWrap(updateErr, aicrErrors.ErrCodeInternal,
					fmt.Sprintf("failed to update existing %s %q", gvk.Kind, ref.Name))
			}
			slog.Info("Updated existing Trainer resource", "kind", gvk.Kind, "name", ref.Name, "namespace", ref.Namespace)
		default:
			// An ambiguous Create (timeout, dropped connection) may still have
			// persisted the object. Claim it before failing, otherwise rollback
			// cannot remove it and we leak exactly what this change exists to stop.
			if claimed, ok := claimAmbiguousCreate(ctx, client, ref, attemptID, err); ok {
				created = append(created, claimed)
			}
			return created, aicrErrors.Wrap(trainerAPIErrorCode(err),
				fmt.Sprintf("failed to create %s %q", gvk.Kind, ref.Name), err)
		}
	}
	return created, nil
}

// updateExistingTrainerResource overwrites a resource left behind by a prior or
// concurrent install. It reads the live object for its resourceVersion, and for
// Services carries over the server-assigned cluster IPs: those are immutable and
// absent from the rendered manifest, so an update without them is rejected.
//
// A lost optimistic-concurrency race is retried with a fresh read, since an update
// failure aborts the whole installation and a concurrent writer touching the same
// leftover object should not be able to sink the benchmark. Every other error
// surfaces on the first attempt.
func updateExistingTrainerResource(ctx context.Context, client dynamic.ResourceInterface,
	obj *unstructured.Unstructured) error {

	updateCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	// Both are reset per attempt, so after the loop each is non-nil only when the
	// final attempt ended that way rather than at the write.
	var readErr, ownershipErr error
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		readErr, ownershipErr = nil, nil

		// Re-read on every attempt: a conflict means the resourceVersion just
		// used is stale, so replaying it would conflict forever.
		existing, getErr := client.Get(updateCtx, obj.GetName(), metav1.GetOptions{})
		if getErr != nil {
			readErr = getErr
			return getErr
		}

		// Upstream names the admission configurations generically, so an existing
		// one may belong to a different operator. A full Update would replace its
		// webhooks (and any injected caBundle), and because updates are excluded
		// from the rollback set it would never be restored. Fail closed instead.
		if isAdmissionConfigKind(existing.GetKind()) {
			if !hasTrainerWebhookEntry(existing) {
				ownershipErr = aicrErrors.New(aicrErrors.ErrCodeConflict,
					fmt.Sprintf("%s %q exists but carries no %s webhook; refusing to overwrite another operator's configuration",
						existing.GetKind(), existing.GetName(), trainerWebhookSuffix))
				return ownershipErr
			}
			// A Trainer-owned configuration pointing at a different namespace is a
			// live installation deployed another way (the Helm chart uses kubeflow,
			// this installer uses kubeflow-system). Repointing it here would break
			// that installation's admission path, and since updates are excluded
			// from the rollback set, nothing would put it back.
			if liveNS := webhookServiceNamespace(existing); liveNS != "" && liveNS != webhookServiceNamespace(obj) {
				ownershipErr = aicrErrors.New(aicrErrors.ErrCodeConflict,
					fmt.Sprintf("%s %q belongs to a Trainer installation in namespace %q; refusing to repoint it",
						existing.GetKind(), existing.GetName(), liveNS))
				return ownershipErr
			}
		}

		updated := obj.DeepCopy()
		updated.SetResourceVersion(existing.GetResourceVersion())
		if updated.GetKind() == "Service" {
			preserveServiceClusterIPs(existing, updated)
		}

		_, updateErr := client.Update(updateCtx, updated, metav1.UpdateOptions{})
		return updateErr
	})

	switch {
	case err == nil:
		return nil
	case ownershipErr != nil:
		return ownershipErr
	case readErr != nil:
		return aicrErrors.Wrap(trainerAPIErrorCode(readErr), "failed to read existing resource for update", readErr)
	default:
		return aicrErrors.Wrap(trainerAPIErrorCode(err), "failed to update existing resource", err)
	}
}

// claimAmbiguousCreate re-reads a resource whose Create failed inconclusively. A
// timeout or dropped connection can be returned after the apiserver has already
// persisted the object; without this the resource is absent from the rollback set
// and leaks. Returns the ref with its UID when the object is there to claim.
func claimAmbiguousCreate(ctx context.Context, client dynamic.ResourceInterface,
	ref trainerResourceRef, attemptID string, createErr error) (trainerResourceRef, bool) {

	// Only genuinely ambiguous failures can have persisted. A rejection (Forbidden,
	// Invalid, TooManyRequests) definitively did not create anything, and probing
	// after one risks claiming an object that was already there.
	if !isAmbiguousCreateError(createErr) {
		return ref, false
	}

	// contextcheck: deliberately not derived from ctx, which may itself be the
	// reason Create failed; the object still has to be claimed for rollback.
	getCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
	defer cancel()
	_ = ctx

	obj, err := client.Get(getCtx, ref.Name, metav1.GetOptions{}) //nolint:contextcheck
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			slog.Warn("Could not determine whether an ambiguous create persisted",
				"resource", ref.String(), "error", err)
		}
		return ref, false
	}

	// Presence is not ownership. Without a matching marker this object predates the
	// attempt or belongs to someone else, and claiming it would let rollback delete
	// a resource we never created.
	if obj.GetAnnotations()[installAttemptAnnotation] != attemptID {
		slog.Warn("Create failed and a same-named resource exists that this attempt did not create; leaving it alone",
			"resource", ref.String())
		return ref, false
	}

	ref.UID = obj.GetUID()
	slog.Warn("Create failed but the resource exists and is ours; claiming it for rollback",
		"resource", ref.String())
	return ref, true
}

// isAmbiguousCreateError reports whether a failed Create may still have persisted
// the object. Deterministic rejections are excluded.
func isAmbiguousCreateError(err error) bool {
	return k8serrors.IsTimeout(err) ||
		k8serrors.IsServerTimeout(err) ||
		k8serrors.IsServiceUnavailable(err) ||
		k8serrors.IsUnexpectedServerError(err) ||
		aicrErrors.IsNetworkError(err) ||
		aicrErrors.IsTransient(err)
}

// newInstallAttemptID returns a random marker identifying one installation attempt.
func newInstallAttemptID() (string, error) {
	buf := make([]byte, 16)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to generate an install attempt id", err)
	}
	return hex.EncodeToString(buf), nil
}

// webhookServiceNamespace returns the namespace of the controller Service an
// admission configuration points at, or "" when it names none.
func webhookServiceNamespace(obj *unstructured.Unstructured) string {
	entries, _, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		entry, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if ns, _, _ := unstructured.NestedString(entry, "clientConfig", "service", "namespace"); ns != "" {
			return ns
		}
	}
	return ""
}

// isAdmissionConfigKind reports whether kind is one of the generically-named
// admission configurations another operator on the cluster could own.
func isAdmissionConfigKind(kind string) bool {
	return kind == "ValidatingWebhookConfiguration" || kind == "MutatingWebhookConfiguration"
}

// hasTrainerWebhookEntry reports whether an admission configuration carries at
// least one Kubeflow Trainer webhook, which is what marks it as ours to replace.
func hasTrainerWebhookEntry(obj *unstructured.Unstructured) bool {
	entries, _, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		return false
	}
	for _, e := range entries {
		entry, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := entry[keyName].(string); ok && strings.HasSuffix(name, trainerWebhookSuffix) {
			return true
		}
	}
	return false
}

// preserveServiceClusterIPs copies the apiserver-assigned cluster IPs from the
// live Service onto the replacement. They cannot be unset once assigned.
func preserveServiceClusterIPs(existing, updated *unstructured.Unstructured) {
	if ip, found, err := unstructured.NestedString(existing.Object, "spec", "clusterIP"); err == nil && found {
		if setErr := unstructured.SetNestedField(updated.Object, ip, "spec", "clusterIP"); setErr != nil {
			slog.Warn("Failed to preserve Service clusterIP", "name", updated.GetName(), "error", setErr)
		}
	}
	if ips, found, err := unstructured.NestedStringSlice(existing.Object, "spec", "clusterIPs"); err == nil && found {
		if setErr := unstructured.SetNestedStringSlice(updated.Object, ips, "spec", "clusterIPs"); setErr != nil {
			slog.Warn("Failed to preserve Service clusterIPs", "name", updated.GetName(), "error", setErr)
		}
	}
}

// rollbackTrainer removes everything a failed installation created and returns
// cause. A cleanup failure is folded into the returned error rather than logged
// and dropped, so leaked cluster-scoped resources are never mistaken for a clean
// failure.
func rollbackTrainer(dynamicClient dynamic.Interface, created []trainerResourceRef, cause error) error {
	if len(created) == 0 {
		return cause
	}

	slog.Warn("Rolling back partial Kubeflow Trainer installation",
		"resources", len(created), "cause", cause)

	cleanupErr := deleteTrainer(dynamicClient, created)
	if cleanupErr == nil {
		return cause
	}
	return aicrErrors.WrapWithContext(aicrErrors.ErrCodeInternal,
		fmt.Sprintf("Trainer installation failed and rollback left resources behind: %v", cleanupErr),
		cause, map[string]interface{}{"rollbackError": cleanupErr.Error()})
}

// rewriteJobSetStagingImage rewrites any reference to the garbage-collected JobSet
// staging-registry image repository onto the promoted production registry, preserving
// the tag/digest. The Kubeflow Trainer v2.2.0 manifests pin the JobSet controller image
// to the Kubernetes staging registry, whose tags have been garbage-collected; left as-is
// the jobset-controller-manager enters ImagePullBackOff and its admission webhook has no
// endpoints, so the NCCL TrainJob cannot create pods (issue #1430). The replacement is a
// repo-prefix swap only, so it is tag-agnostic and a no-op when the staging repo is absent.
func rewriteJobSetStagingImage(yamlBytes []byte) []byte {
	if !bytes.Contains(yamlBytes, []byte(jobSetStagingImageRepo)) {
		return yamlBytes
	}
	rewritten := bytes.ReplaceAll(yamlBytes, []byte(jobSetStagingImageRepo), []byte(jobSetPromotedImageRepo))
	slog.Info("Rewrote JobSet image off staging registry",
		"from", jobSetStagingImageRepo, "to", jobSetPromotedImageRepo)
	return rewritten
}

// deleteTrainer removes every resource that was created by installTrainer, in reverse
// application order so dependents are deleted before their owners.
// Uses context.Background() because the parent context may already be canceled at
// defer time; cleanup must still complete.
//
// Every resource is attempted even after a failure, and all failures are returned
// together, each naming the resource that leaked. An already-deleted resource
// counts as success.
func deleteTrainer(dynamicClient dynamic.Interface, resources []trainerResourceRef) error {
	slog.Info("Deleting installed Kubeflow Trainer resources", "count", len(resources))

	var failures []string
	// A teardown failure fails an otherwise-passing benchmark, so the code has to
	// distinguish a cluster blip from a real fault. Transient unless proven
	// otherwise; one deterministic failure makes the whole cleanup Internal.
	code := aicrErrors.ErrCodeUnavailable
	for _, ref := range slices.Backward(resources) {
		err := deleteTrainerResource(dynamicClient, ref)
		if err == nil {
			slog.Info("Deleted Trainer resource", "resource", ref.String())
			continue
		}

		slog.Error("Failed to delete Trainer resource", "resource", ref.String(), "error", err)
		failures = append(failures, fmt.Sprintf("%s: %v", ref, err))
		if trainerAPIErrorCode(err) == aicrErrors.ErrCodeInternal {
			code = aicrErrors.ErrCodeInternal
		}
	}

	if len(failures) > 0 {
		return aicrErrors.New(code,
			fmt.Sprintf("failed to delete %d Trainer resource(s):\n  - %s",
				len(failures), strings.Join(failures, "\n  - ")))
	}
	return nil
}

// deleteTrainerResource deletes one resource, retrying transient API failures.
// Because a cleanup failure fails an otherwise-good benchmark, a momentary
// control-plane blip must not sink the run; deterministic failures (Forbidden,
// admission rejections) return on the first attempt rather than burning backoff.
// An already-deleted resource counts as success.
func deleteTrainerResource(dynamicClient dynamic.Interface, ref trainerResourceRef) error {
	return retry.OnError(retry.DefaultBackoff,
		func(err error) bool { return trainerAPIErrorCode(err) == aicrErrors.ErrCodeUnavailable },
		func() error {
			deleteCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
			defer cancel()

			opts := metav1.DeleteOptions{}
			if ref.UID != "" {
				opts.Preconditions = &metav1.Preconditions{UID: &ref.UID}
			}

			err := trainerResourceClient(dynamicClient, ref.GVR, ref.Namespace).
				Delete(deleteCtx, ref.Name, opts)
			switch {
			case err == nil, k8serrors.IsNotFound(err):
				return nil
			case k8serrors.IsConflict(err):
				// The UID moved on: what is there now was recreated by someone
				// else, so the object we created is already gone. Not ours to delete.
				slog.Info("Trainer resource was replaced by another owner; leaving it alone",
					"resource", ref.String())
				return nil
			default:
				return err
			}
		})
}

// waitForTrainerCRDsEstablished waits for the two CRDs that the NCCL test requires
// to reach the Established condition after Trainer installation.
func waitForTrainerCRDsEstablished(ctx context.Context, dynamicClient dynamic.Interface) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.TrainerCRDEstablishedTimeout)
	defer cancel()

	for _, crd := range requiredTrainerCRDs {
		slog.Info("Waiting for Trainer CRD to be established", "crd", crd)
		if err := waitForCRDEstablished(waitCtx, dynamicClient, crd); err != nil {
			// Preserve the structured code from the re-check path (Unavailable /
			// Internal) instead of collapsing every failure to Timeout.
			return aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeTimeout, fmt.Sprintf("CRD %s not established", crd))
		}
	}
	return nil
}

// waitForTrainerControllerReady polls the controller-manager Deployment until at
// least one replica is ready, ensuring the ValidatingWebhookConfiguration can
// serve admission requests before the caller creates Trainer custom resources.
//
// An empty deployment falls back to the self-install overlay's fixed name, which
// is correct only for an installation this validator just applied. Waiting on that
// name against a chart installation under a non-default release name would poll a
// Deployment that does not exist: waitForDeploymentReady treats NotFound as
// not-ready-yet, so it would burn the full timeout and report a healthy controller
// as never ready.
func waitForTrainerControllerReady(ctx context.Context, dynamicClient dynamic.Interface,
	namespace, deployment string) error {

	if deployment == "" {
		deployment = trainerControllerDeployment
	}
	return waitForDeploymentReady(ctx, dynamicClient, namespace,
		deployment, defaults.TrainerControllerReadyTimeout)
}

// waitForDeploymentReady polls a Deployment until at least one replica is ready.
//
// Terminal authorization failures return immediately: looping the full timeout on
// a persistent Forbidden would report a generic readiness timeout and hide the
// real cause. Every poll logs what it observed, so a stuck wait leaves a trail
// rather than a silent gap followed by a bare timeout.
func waitForDeploymentReady(ctx context.Context, dynamicClient dynamic.Interface,
	namespace, name string, timeout time.Duration) error {

	slog.Info("Waiting for Deployment to become ready", "deployment", name, "namespace", namespace)

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		deploy, err := dynamicClient.Resource(trainerDeploymentGVR).Namespace(namespace).
			Get(waitCtx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			readyReplicas, _, _ := unstructured.NestedInt64(deploy.Object, "status", "readyReplicas")
			if readyReplicas >= 1 {
				slog.Info("Deployment is ready", "deployment", name, "readyReplicas", readyReplicas)
				return nil
			}
			slog.Debug("Deployment not ready yet", "deployment", name, "readyReplicas", readyReplicas)
		case k8serrors.IsForbidden(err), k8serrors.IsUnauthorized(err):
			return aicrErrors.Wrap(aicrErrors.ErrCodeUnauthorized,
				fmt.Sprintf("not permitted to read Deployment %s/%s", namespace, name), err)
		default:
			slog.Debug("Failed to read Deployment while waiting for readiness",
				"deployment", name, "namespace", namespace, "error", err)
		}

		select {
		case <-waitCtx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
				fmt.Sprintf("timed out waiting for Deployment %s/%s to become ready", namespace, name),
				waitCtx.Err())
		case <-time.After(defaults.TrainerControllerPollInterval):
		}
	}
}

// waitForCRDEstablished watches a CRD until its Established condition is True.
// It checks the current state first so the fast path (already established) returns
// immediately without starting a watch.
func waitForCRDEstablished(ctx context.Context, dynamicClient dynamic.Interface, crdName string) error {
	existing, err := dynamicClient.Resource(trainerCRDGVR).Get(ctx, crdName, metav1.GetOptions{})
	if err == nil && isCRDEstablished(existing) {
		return nil
	}

	watcher, err := dynamicClient.Resource(trainerCRDGVR).Watch(ctx, metav1.ListOptions{
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
				if ctxErr := ctx.Err(); ctxErr != nil {
					return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for CRD to be established", ctxErr)
				}
				// Watch closed without cancellation — re-Get before failing, in
				// case the CRD was established during the closure window.
				recheck, getErr := dynamicClient.Resource(trainerCRDGVR).Get(ctx, crdName, metav1.GetOptions{})
				switch {
				case getErr == nil:
					if isCRDEstablished(recheck) {
						slog.Info("CRD established", "crd", crdName)
						return nil
					}
					return aicrErrors.New(aicrErrors.ErrCodeUnavailable, "CRD watch channel closed before it was established")
				case k8serrors.IsNotFound(getErr):
					return aicrErrors.New(aicrErrors.ErrCodeUnavailable, "CRD watch channel closed before it was established")
				case aicrErrors.IsTransient(getErr):
					return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "CRD watch closed and re-check timed out", getErr)
				default:
					return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "CRD watch closed and re-check failed", getErr)
				}
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
