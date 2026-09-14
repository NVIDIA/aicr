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
	"encoding/json"
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

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
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

	// jobSetControllerDeployment is the JobSet controller-manager Deployment name
	// emitted by this package's kustomize overlay (see jobSetNameLabel for why
	// the Helm chart's release-derived name doesn't apply here).
	jobSetControllerDeployment = "jobset-controller-manager"

	// trainerControllerService is the Service fronting the controller-manager's
	// webhook port. Without it the admission webhooks have no endpoints and every
	// TrainJob create is rejected.
	trainerControllerService = "kubeflow-trainer-controller-manager"

	// trainerNamespace is the namespace this validator's self-install uses. It is
	// NOT the only layout: the kubeflow-trainer Helm chart the recipes deploy pins
	// defaultNamespace: kubeflow (recipes/registry.yaml). The probe therefore
	// discovers the live namespace rather than assuming this one.
	//
	// Differing from the chart's namespace is load-bearing, not accidental drift to
	// be tidied away later (issue #2223). Two things depend on it:
	//
	//   - The CNCF evidence collector distinguishes "the bundle deployed Kubeflow
	//     Trainer" from "the validator self-installed one to run the NCCL
	//     benchmark" solely by namespace. Sharing one namespace would let this
	//     temporary install be collected as recipe-deployed, so signed conformance
	//     evidence would claim an operator the bundle never shipped.
	//   - The conflict guard in ensureTrainerInstalled compares the discovered
	//     installation's namespace against this one to refuse installing over a
	//     Trainer this validator does not own. Sharing a namespace makes that
	//     comparison always false, and applyTrainerResources would then overwrite
	//     the recipe's Helm-managed objects in place via
	//     updateExistingTrainerResource, with nothing restoring them afterwards.
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

	// kubeflowTrainerComponent is the registry name of the Kubeflow Trainer
	// component. A recipe declaring it is promising a Trainer installation, which
	// is what ensureTrainerInstalled keys its behavior on.
	kubeflowTrainerComponent = "kubeflow-trainer"

	// jobSetCRDName identifies the JobSet dependency. TrainJobs run as JobSets, so
	// a Trainer whose JobSet controller never becomes ready fails opaquely later.
	jobSetCRDName = "jobsets.jobset.x-k8s.io"

	// The trainer component and part-of labels locate the Trainer controller
	// Deployment across both deployment paths. The in-tree Helm values pin
	// fullnameOverride, but a pre-existing chart installation can use
	// release-derived naming or another override; the kustomize overlay also uses
	// app.kubernetes.io/name=trainer instead of kubeflow-trainer. These two labels
	// are stable across both layouts.
	trainerComponentLabel = "app.kubernetes.io/component"
	trainerComponentValue = "manager"
	trainerPartOfLabel    = "app.kubernetes.io/part-of"
	trainerPartOfValue    = "kubeflow"

	// installAttemptAnnotation marks objects created by one installation attempt, so
	// an ambiguous Create can only claim an object this attempt actually created.
	installAttemptAnnotation = "aicr.nvidia.com/install-attempt"

	// jobSetNameLabel/jobSetLabelValue locate the JobSet controller Deployment.
	// The kustomize overlay emits jobset-controller-manager. The upstream chart
	// defaults jobset.fullnameOverride to jobset (AICR does not override it), so
	// the bundled Helm path renders jobset-controller regardless of release name;
	// a separately managed chart can choose another override. Every layout sets
	// this label, so it is the stable handle.
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

	// trainerInstallManifestName is a ConfigMap in trainerNamespace recording a
	// self-install's resource list, durably, so reapOrphanedTrainerInstall can
	// still find and remove it if the installer never reaches its own cleanup.
	trainerInstallManifestName = "aicr-trainer-self-install-manifest"

	// trainerInstallManifestKey holds the JSON-encoded []trainerResourceRef.
	trainerInstallManifestKey = "resources"
)

// controllerTolerateAll lets a Trainer/JobSet controller-manager Deployment
// schedule on any node pool, regardless of taints. Built through
// component.TolerationsToPodSpec, the same converter used for Helm-values
// toleration overrides, so there is one canonical place that knows the
// toleration-to-map shape.
var controllerTolerateAll = tolerationsToAnySlice(
	component.TolerationsToPodSpec([]corev1.Toleration{{Operator: corev1.TolerationOpExists}}),
)

// tolerationsToAnySlice widens []map[string]any to []any: unstructured pod
// specs (podSpec["tolerations"]) must hold []any, not []map[string]any, to
// match how NestedSlice reads and how JSON round-tripping serializes it.
func tolerationsToAnySlice(tolerations []map[string]any) []any {
	result := make([]any, len(tolerations))
	for i, t := range tolerations {
		result[i] = t
	}
	return result
}

// cloneControllerTolerateAll returns a fresh copy of controllerTolerateAll.
// It is stamped onto both the Trainer and JobSet Deployments, so a per-call
// copy keeps a future in-place edit of one controller's tolerations from
// silently corrupting the other Deployment's live object, or the shared
// process-wide global itself.
func cloneControllerTolerateAll() []any {
	cloned := make([]any, len(controllerTolerateAll))
	for i, t := range controllerTolerateAll {
		m, _ := t.(map[string]any)
		clonedMap := make(map[string]any, len(m))
		for k, v := range m {
			clonedMap[k] = v
		}
		cloned[i] = clonedMap
	}
	return cloned
}

// applyControllerTolerations stamps controllerTolerateAll onto the Trainer and
// JobSet controller-manager Deployments' pod template, unless one already
// declares tolerations. Scoped to those two names so an unrelated Deployment
// in the manifest set never gets a blanket toleration it didn't ask for.
func applyControllerTolerations(obj *unstructured.Unstructured) error {
	if gvk := obj.GroupVersionKind(); gvk.Kind != "Deployment" || gvk.Group != apiGroupApps {
		return nil
	}
	switch obj.GetName() {
	case trainerControllerDeployment, jobSetControllerDeployment:
	default:
		return nil
	}

	if existing, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "tolerations"); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to read tolerations from Deployment %q", obj.GetName()), err)
	} else if found && len(existing) > 0 {
		slog.Debug("Controller Deployment already declares tolerations; leaving untouched", "name", obj.GetName())
		return nil
	}

	podSpec, found := nestedMap(obj.Object, "spec", "template", "spec")
	if !found {
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("pod spec not found in Deployment %q", obj.GetName()))
	}
	podSpec["tolerations"] = cloneControllerTolerateAll()
	slog.Info("Applying blanket toleration to controller Deployment", "name", obj.GetName(), "namespace", obj.GetNamespace())
	return nil
}

// GVRs for the objects the Trainer lifecycle probes and waits on.
var (
	trainerCRDGVR = schema.GroupVersionResource{
		Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs,
	}
	trainerDeploymentGVR = schema.GroupVersionResource{
		Group: apiGroupApps, Version: "v1", Resource: "deployments",
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
	trainerClusterRoleGVR = schema.GroupVersionResource{
		Group: apiGroupRBAC, Version: "v1", Resource: "clusterroles",
	}
	trainerClusterRoleBindingGVR = schema.GroupVersionResource{
		Group: apiGroupRBAC, Version: "v1", Resource: "clusterrolebindings",
	}
	trainerNamespaceGVR = schema.GroupVersionResource{
		Version: "v1", Resource: "namespaces",
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

	// Incomplete records which specific object was missing when the probe
	// reported the installation as incomplete. The probe already determines this
	// to log it; carrying it out makes the resulting failure self-diagnosing
	// rather than sending an operator to the logs to find out what to fix.
	Incomplete string
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
			return trainerInstall{Incomplete: fmt.Sprintf("CRD %s is missing", crd)}, false, nil
		}
		if !isCRDEstablished(obj) {
			slog.Info("Kubeflow Trainer incomplete: CRD not established", "crd", crd)
			return trainerInstall{Incomplete: fmt.Sprintf("CRD %s is not established", crd)}, false, nil
		}
	}

	// The validating configuration is the authority on where Trainer lives: its
	// webhook entries name the controller Service and its namespace, which is what
	// makes this work for both the self-install overlay and the Helm chart.
	install, found, err := discoverTrainerInstall(ctx, dynamicClient,
		trainerValidatingWebhookGVR, trainerValidatingWebhookConfig, trainerValidatingWebhookName)
	if err != nil {
		return trainerInstall{}, false, err
	}
	if !found {
		// Carry discovery's own reason out rather than flattening it to a bare
		// false: it is the difference between "no admission configuration at all"
		// and "the configuration is there but names no Service", and the operator
		// needs to know which to fix.
		return install, false, nil
	}

	reason, ok, err := hasTrainerWebhook(ctx, dynamicClient,
		trainerMutatingWebhookGVR, trainerMutatingWebhookConfig, trainerMutatingWebhookName)
	if err != nil {
		return trainerInstall{}, false, err
	}
	if !ok {
		// Carry the specific reason out rather than reporting every failure as a
		// missing configuration: "create it" and "something else owns this name"
		// are different jobs for whoever reads the verdict.
		slog.Info("Kubeflow Trainer incomplete: mutating admission webhook unusable",
			"configuration", trainerMutatingWebhookConfig, "webhook", trainerMutatingWebhookName,
			"reason", reason)
		return trainerInstall{Incomplete: reason}, false, nil
	}

	// The controller Deployment is found by label because an externally managed
	// chart installation may use a custom name; a hardcoded in-tree name would
	// misreport that installation as incomplete.
	controller, found, err := findTrainerController(ctx, dynamicClient, install.Namespace)
	if err != nil {
		return trainerInstall{}, false, err
	}
	if !found {
		slog.Info("Kubeflow Trainer incomplete: controller Deployment missing",
			"namespace", install.Namespace)
		return trainerInstall{Incomplete: "the controller Deployment was not found"}, false, nil
	}
	install.Deployment = controller

	if _, found, err := getTrainerObject(ctx, dynamicClient, trainerServiceGVR,
		install.Namespace, install.Service); err != nil {
		return trainerInstall{}, false, err
	} else if !found {
		slog.Info("Kubeflow Trainer incomplete: controller Service missing",
			"namespace", install.Namespace, "name", install.Service)
		return trainerInstall{Incomplete: "the controller Service was not found"}, false, nil
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
	if err != nil {
		// A failed read is not evidence of absence. Propagate it so the caller
		// classifies it as transport or timeout rather than as a deployment that
		// never happened.
		return trainerInstall{}, false, err
	}
	if !found {
		slog.Info("Kubeflow Trainer incomplete: admission configuration missing",
			"configuration", configName)
		return trainerInstall{Incomplete: fmt.Sprintf(
			"admission configuration %q is missing", configName)}, false, nil
	}

	entries, _, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		return trainerInstall{}, false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to read webhooks from %s %q", gvr.Resource, configName), err)
	}

	for _, e := range entries {
		entry, ok := e.(map[string]any)
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
			return trainerInstall{Incomplete: "the admission webhook has no Service reference, so the installation namespace cannot be located"}, false, nil
		}
		return trainerInstall{Namespace: namespace, Service: service}, true, nil
	}

	slog.Info("Kubeflow Trainer incomplete: admission webhook missing",
		"configuration", configName, "webhook", webhookName)
	return trainerInstall{Incomplete: fmt.Sprintf(
		"admission configuration %q exists but does not contain the %q webhook",
		configName, webhookName)}, false, nil
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
//
// When the answer is no it returns the reason, because the two ways to get there
// need different remedies: a configuration that is absent has to be created, while
// one that exists but serves someone else's webhook has to be reconciled with
// whatever already owns that name. Reporting both as "missing" — which is what
// flattening this to a bare false did — sends an operator to create an object that
// is already on the cluster. The validating-webhook path in discoverTrainerInstall
// draws the same distinction.
func hasTrainerWebhook(ctx context.Context, dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource, configName, webhookName string) (string, bool, error) {

	obj, found, err := getTrainerObject(ctx, dynamicClient, gvr, "", configName)
	if err != nil {
		return "", false, err
	}
	if !found {
		return fmt.Sprintf("admission configuration %q is missing", configName), false, nil
	}

	entries, _, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to read webhooks from %s %q", gvr.Resource, configName), err)
	}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if entry[keyName] == webhookName {
			return "", true, nil
		}
	}
	return fmt.Sprintf("admission configuration %q exists but does not contain the %q webhook",
		configName, webhookName), false, nil
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
//
// recipeDeclaresTrainer says whether the recipe under validation ships Kubeflow
// Trainer as a delivered component, and it decides what an incomplete installation
// means:
//
//   - declared, and present: use the delivered installation.
//   - declared, but missing or incomplete: fail. Self-installing here would mask a
//     broken deployment of a component the recipe promised, and the benchmark would
//     then report a passing result for a cluster that cannot run TrainJobs at all.
//   - not declared, and present: reuse it, unchanged. The recipe never claimed a
//     Trainer, so a pre-existing one is not evidence of anything to report.
//   - not declared, and absent: install an ephemeral fixture and tear it down, as
//     before. There is nothing to mask, because nothing was promised.
//
// This is deliberately keyed on the recipe rather than on live cluster state: what a
// missing installation *means* is now a property of the recipe, not of whatever
// happens to be on the cluster. Execution still differs across the undeclared rows —
// a pre-existing Trainer is reused, an absent one is installed — which is why the
// earlier "behaves identically regardless of live state" wording was withdrawn.
func ensureTrainerInstalled(ctx context.Context, dynamicClient dynamic.Interface, clientset kubernetes.Interface,
	discoveryClient discovery.DiscoveryInterface, recipeDeclaresTrainer bool) ([]trainerResourceRef, error) {

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
		// The recipe ships Kubeflow Trainer, so a missing or incomplete installation
		// is a deployment failure, not something to paper over. Installing our own
		// here would produce a passing benchmark for a cluster whose delivered
		// Trainer is broken.
		//
		// But isTrainerInstalled is an existence probe, not a readiness probe: it
		// reports incomplete for a CRD that is present but not yet Established, or a
		// controller Deployment that has not appeared yet. Both are ordinary rollout
		// states on the bundle-deploy-validate path. Poll before declaring failure,
		// so a validate that starts while the chart is still landing waits it out
		// rather than reporting a deployment that has not failed.
		if recipeDeclaresTrainer {
			declared, waitErr := waitForDeclaredTrainer(ctx, dynamicClient)
			if waitErr != nil {
				return nil, waitErr
			}
			// The delivered installation finished rolling out. Fall through to the
			// same readiness wait a pre-existing installation gets, and claim no
			// resources: the recipe owns this Trainer, not the benchmark.
			install = declared
			return nil, awaitTrainerController(ctx, dynamicClient, install)
		}

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
		created, installErr := installTrainer(ctx, dynamicClient, clientset, discoveryClient)
		if installErr != nil {
			return nil, aicrErrors.PropagateOrWrap(installErr, aicrErrors.ErrCodeInternal,
				"failed to install Kubeflow Trainer")
		}
		slog.Info("Kubeflow Trainer installed", "resources", len(created))
		return created, nil
	}

	return nil, awaitTrainerController(ctx, dynamicClient, install)
}

// classifyPollExpiry turns an expired poll context into the right verdict, or nil
// when the poll context is still live and the caller should handle its own error.
//
// The two expiries mean opposite things. A canceled parent means the run was
// aborted — catalog timeout, canceled phase, killed Job — which is not a customer
// deployment defect. A local deadline means the delivered Trainer never became
// complete, which is.
//
// Both paths route through here so the verdict does not depend on whether the clock
// ran out during a probe or during a sleep: the same cluster state must not produce
// two different codes.
//
// observed carries a concrete incomplete observation — a probe that succeeded and
// named the object that is not there — and it is what separates the two failing
// codes. NotFound may only be synthesized from such an observation. Deriving it from
// an empty one would report a degraded or slow apiserver, where every probe attempt
// was a read timeout and nothing was ever read, as a customer deployment defect; and
// it would report a probe that found the installation complete as one that found
// nothing. With no observation to stand on, the honest verdict is the timeout that
// actually happened. The allowance is enforced either way — only the classification
// differs.
//
// unobserved is the reason to report in that case.
func classifyPollExpiry(ctx, pollCtx context.Context, observed trainerInstall, unobserved string) error {
	if ctx.Err() != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
			"canceled while waiting for the recipe-declared Kubeflow Trainer", ctx.Err())
	}
	if pollCtx.Err() == nil {
		return nil
	}

	verdict := fmt.Sprintf(
		"the recipe declares the %s component but its Kubeflow Trainer installation "+
			"did not become complete within %s: %%s. The benchmark will not self-install "+
			"over a delivered component that failed to deploy",
		kubeflowTrainerComponent, trainerInstallWaitTimeout)

	if observed.Incomplete == "" {
		if unobserved == "" {
			unobserved = "the allowance expired before any probe could tell"
		}
		return aicrErrors.New(aicrErrors.ErrCodeTimeout, fmt.Sprintf(verdict, unobserved))
	}

	// ErrCodeNotFound, not Unavailable: the read succeeded and the answer was "not
	// deployed". Unavailable is this package's code for a transport failure — see
	// the decision table on validators.Require — and using it here would file a
	// product defect alongside apiserver hiccups, telling whoever triages it to
	// re-run rather than to fix their deployment.
	return aicrErrors.New(aicrErrors.ErrCodeNotFound, fmt.Sprintf(verdict, observed.Incomplete))
}

// awaitTrainerController waits for a Trainer the benchmark does not own to finish
// starting. The probe confirms every object exists; the controller may still be
// rolling, and waiting is the alternative to reinstalling over a healthy Trainer.
func awaitTrainerController(ctx context.Context, dynamicClient dynamic.Interface, install trainerInstall) error {
	slog.Info("Kubeflow Trainer already installed, waiting for controller readiness",
		"namespace", install.Namespace, "deployment", install.Deployment)
	if readyErr := waitForTrainerReady(ctx, dynamicClient, install.Namespace, install.Deployment); readyErr != nil {
		return aicrErrors.PropagateOrWrap(readyErr, aicrErrors.ErrCodeTimeout,
			"pre-existing Kubeflow Trainer controller is not ready")
	}
	return nil
}

// trainerInstallWaitTimeout and trainerInstallPollInterval bound the wait for a
// recipe-declared Trainer to finish rolling out. They are variables rather than
// constants only so tests can exercise the timeout path without waiting for it.
var (
	trainerInstallWaitTimeout  = defaults.TrainerControllerReadyTimeout
	trainerInstallPollInterval = defaults.TrainerInstallPollInterval
)

// waitForDeclaredTrainer polls until a recipe-declared Kubeflow Trainer reports a
// complete installation, or fails with the specific object that never appeared.
//
// The distinction it draws is the point of the recipe-driven lifecycle: an
// installation that is still rolling out is not a failed one, but an installation
// that never completes is — and the benchmark must not install its own Trainer over
// a delivered component, because that would report a passing result for a cluster
// whose Trainer is broken.
func waitForDeclaredTrainer(ctx context.Context, dynamicClient dynamic.Interface) (trainerInstall, error) {
	var last trainerInstall

	pollCtx, cancel := context.WithTimeout(ctx, trainerInstallWaitTimeout)
	defer cancel()

	for {
		// Probe under pollCtx so the rollout allowance actually bounds it. Each probe
		// makes several sequential reads, each with its own DiagnosticTimeout, so a
		// probe running on the parent context could cross the deadline and still
		// report success — the allowance would bound only the sleeps.
		//
		// The cost is that getTrainerObject checks ctx.Err() at the top of every read
		// and returns Timeout, so an expiring deadline surfaces here as a probe error
		// rather than through the select. Classify it below instead of propagating it
		// blind, so the verdict does not depend on where in the loop the clock ran out.
		install, ok, err := isTrainerInstalled(pollCtx, dynamicClient)
		if err != nil {
			// Let expiry win only for a context-derived error. A read that starts
			// before the deadline and fails for a real reason after it — an apiserver
			// 503, say — must keep its transport classification: reporting that as
			// "the installation did not become complete" files a control-plane outage
			// as a customer deployment defect, which is the same misclassification
			// the NotFound/Unavailable distinction exists to prevent. It is also
			// exactly when the apiserver is degraded that the transport signal is
			// worth the most.
			if errors.Is(err, aicrErrors.New(aicrErrors.ErrCodeTimeout, "")) {
				// last is the newest concrete observation, and it is the zero value
				// until some probe completes. When every attempt was cut short by the
				// deadline there is nothing on the cluster to point at, so this stays
				// a timeout rather than becoming a deployment defect.
				if verdict := classifyPollExpiry(ctx, pollCtx, last,
					"no probe completed before the allowance expired"); verdict != nil {
					return trainerInstall{}, verdict
				}
			}
			return trainerInstall{}, aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeInternal,
				"failed to check Kubeflow Trainer installation")
		}
		// Recheck expiry before accepting success. The probe's last read can return
		// just after the deadline, and taking ok on that read would let the wait
		// outrun its own allowance — the bound has to cover the answer, not only
		// the question.
		//
		// Report why from *this* probe, not the previous one. When ok is true the
		// installation is complete, so reusing last would blame a missing object the
		// probe just found — sending an operator to look for something that is there,
		// when the real finding is a rollout slower than its budget. When ok is false
		// the current probe's own finding is the specific object still missing, which
		// the previous iteration's result would replace with a staler one — or, on the
		// first probe, with the zero value's generic fallback.
		//
		// The complete case claims only what was observed. The wait never measures when
		// the installation became complete, only when it saw that it was, so wording it
		// as a transition would assert a time nobody read.
		//
		// A complete probe is not an incomplete observation, so it is reported as the
		// timeout it is: the installation is there, and the only finding is a rollout
		// or a read slower than the budget.
		observed, unobserved := install, ""
		if ok {
			observed = trainerInstall{}
			unobserved = "the installation was observed complete only after the allowance expired"
		}
		if verdict := classifyPollExpiry(ctx, pollCtx, observed, unobserved); verdict != nil {
			return trainerInstall{}, verdict
		}
		if ok {
			return install, nil
		}
		last = install

		select {
		case <-pollCtx.Done():
			if verdict := classifyPollExpiry(ctx, pollCtx, last,
				"no probe completed before the allowance expired"); verdict != nil {
				return trainerInstall{}, verdict
			}
			// unreachable: this case fires only once pollCtx is done, and
			// classifyPollExpiry always returns non-nil then. Kept as a fail-loud
			// invariant rather than a silent fallthrough.
			return trainerInstall{}, aicrErrors.New(aicrErrors.ErrCodeInternal,
				"Kubeflow Trainer wait ended without a verdict")
		case <-time.After(trainerInstallPollInterval):
		}
	}
}

// foldCleanupError decides the check's verdict when teardown fails. A
// cleanup failure leaks cluster-scoped CRDs, RBAC, and webhook
// configurations (or, for the NCCL resource cleanup caller, poisons the
// next run's fixed-named resources), so it fails an otherwise-passing
// check without masking a real benchmark failure, the more useful signal.
// fallbackMsg is used only when cleanupErr isn't already a
// *StructuredError (PropagateOrWrap preserves an existing one's own
// message/code as-is).
func foldCleanupError(benchErr, cleanupErr error, fallbackMsg string) error {
	if cleanupErr == nil || benchErr != nil {
		return benchErr
	}
	return aicrErrors.PropagateOrWrap(cleanupErr, aicrErrors.ErrCodeInternal, fallbackMsg)
}

// installTrainer downloads the Kubeflow Trainer v2.2.0 archive from GitHub, builds the
// kustomize manager overlay entirely in Go (no CLI), and applies every resource to the
// cluster via the dynamic client.
//
// Installation is transactional: on success it returns the resources it created so
// the caller can defer deleteTrainer for cleanup; on any failure it rolls those
// resources back itself and returns none.
func installTrainer(ctx context.Context, dynamicClient dynamic.Interface, clientset kubernetes.Interface,
	discoveryClient discovery.DiscoveryInterface) ([]trainerResourceRef, error) {

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

	return installTrainerResources(ctx, dynamicClient, clientset, mapper, objs)
}

// decodeTrainerObjects converts kustomize build output into unstructured objects,
// repointing the JobSet controller image off the garbage-collected staging
// registry (issue #1430). Resources without a Kind are skipped. Decoding happens
// before the first apply so a malformed manifest cannot leave a partial install.
func decodeTrainerObjects(resources []*resource.Resource) ([]*unstructured.Unstructured, error) {
	objs := make([]*unstructured.Unstructured, 0, len(resources))
	seenControllers := make(map[string]bool, 2)
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
		if tolErr := applyControllerTolerations(obj); tolErr != nil {
			return nil, tolErr
		}
		// Gated the same way as applyControllerTolerations above: a
		// Deployment-kind resource in a non-apps group must not mark a
		// controller name "seen" without actually receiving the toleration,
		// or it would suppress the warning for the exact silent-miss below
		// that the warning exists to catch.
		if gvk := obj.GroupVersionKind(); gvk.Kind == "Deployment" && gvk.Group == apiGroupApps {
			seenControllers[obj.GetName()] = true
		}
		objs = append(objs, obj)
	}

	// A future Trainer archive bump (or a JobSet overlay variant) that renames
	// either controller Deployment makes applyControllerTolerations's name
	// switch miss it silently, reverting that controller to unschedulable on
	// an all-tainted cluster. Surface the mismatch here instead of letting it
	// resurface only as a bare readiness timeout downstream.
	for _, name := range []string{trainerControllerDeployment, jobSetControllerDeployment} {
		if !seenControllers[name] {
			slog.Warn("Expected controller Deployment not found in Trainer manifest set; "+
				"it will not receive the blanket toleration and may be unschedulable on all-tainted clusters",
				"deployment", name)
		}
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
func installTrainerResources(ctx context.Context, dynamicClient dynamic.Interface, clientset kubernetes.Interface,
	mapper apimeta.RESTMapper, objs []*unstructured.Unstructured) ([]trainerResourceRef, error) {

	created, err := applyTrainerResources(ctx, dynamicClient, clientset, mapper, objs)
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

// trainerResourceKey identifies one tracked resource for manifest merge and
// removal, regardless of kind.
type trainerResourceKey struct {
	gvr             schema.GroupVersionResource
	namespace, name string
}

func trainerResourceKeyOf(ref trainerResourceRef) trainerResourceKey {
	return trainerResourceKey{ref.GVR, ref.Namespace, ref.Name}
}

// newTrainerInstallManifestConfigMap encodes resources into a fresh manifest object.
func newTrainerInstallManifestConfigMap(resources []trainerResourceRef) (*corev1.ConfigMap, error) {
	data, err := json.Marshal(resources)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      trainerInstallManifestName,
			Namespace: trainerNamespace,
			Labels:    map[string]string{labels.ManagedBy: labels.ValueValidator, labels.Component: labels.ValueNCCLPerf},
		},
		Data: map[string]string{trainerInstallManifestKey: string(data)},
	}, nil
}

// persistTrainerInstallManifest durably records a fresh self-install's
// resources, so reapOrphanedTrainerInstall can find and remove them if the
// installing caller never reaches its own cleanup. Failures are only
// logged, since the caller's own cleanup path still works either way.
//
// A concurrent installer's Create can lose to this one with AlreadyExists,
// so the two lists are merged instead of one overwriting the other,
// retrying on conflict.
func persistTrainerInstallManifest(ctx context.Context, clientset kubernetes.Interface, resources []trainerResourceRef) {
	cm, buildErr := newTrainerInstallManifestConfigMap(resources)
	if buildErr != nil {
		slog.Warn("Failed to encode Trainer install manifest", "error", buildErr)
		return
	}

	putCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	if _, err := clientset.CoreV1().ConfigMaps(trainerNamespace).Create(putCtx, cm, metav1.CreateOptions{}); err == nil {
		return
	} else if !k8serrors.IsAlreadyExists(err) {
		slog.Warn("Failed to persist Trainer install manifest", "error", err)
		return
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existingCM, existing, loadErr := loadTrainerInstallManifest(ctx, clientset)
		if loadErr != nil {
			return loadErr
		}
		if existingCM == nil {
			// Deleted between the failed Create above and this read. Recreate it
			// with just this installer's own resources.
			_, createErr := clientset.CoreV1().ConfigMaps(trainerNamespace).Create(putCtx, cm, metav1.CreateOptions{})
			return createErr
		}
		merged, mergeErr := json.Marshal(mergeTrainerResourceRefs(existing, resources))
		if mergeErr != nil {
			return mergeErr
		}
		existingCM.Data = map[string]string{trainerInstallManifestKey: string(merged)}
		_, updateErr := clientset.CoreV1().ConfigMaps(trainerNamespace).Update(putCtx, existingCM, metav1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		slog.Warn("Failed to merge Trainer install manifest with a concurrent installer's", "error", err)
	}
}

// mergeTrainerResourceRefs unions two resource lists by GVR/namespace/name,
// keeping b's entry on a duplicate. Preserves a's, then b's, first-seen
// order, since deleteTrainer tears down in reverse list order and a random
// map order could delete a CRD or webhook before its controller.
func mergeTrainerResourceRefs(a, b []trainerResourceRef) []trainerResourceRef {
	index := make(map[trainerResourceKey]int, len(a)+len(b))
	merged := make([]trainerResourceRef, 0, len(a)+len(b))
	for _, ref := range append(append([]trainerResourceRef{}, a...), b...) {
		key := trainerResourceKeyOf(ref)
		if i, seen := index[key]; seen {
			merged[i] = ref
			continue
		}
		index[key] = len(merged)
		merged = append(merged, ref)
	}
	return merged
}

// loadTrainerInstallManifest reads back a persisted self-install manifest. A
// missing ConfigMap means nothing is tracked, and both return values are nil.
func loadTrainerInstallManifest(ctx context.Context, clientset kubernetes.Interface) (*corev1.ConfigMap, []trainerResourceRef, error) {
	getCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	cm, err := clientset.CoreV1().ConfigMaps(trainerNamespace).Get(getCtx, trainerInstallManifestName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read Trainer install manifest", err)
	}
	raw, ok := cm.Data[trainerInstallManifestKey]
	if !ok || raw == "" {
		return cm, nil, nil
	}
	var resources []trainerResourceRef
	if err := json.Unmarshal([]byte(raw), &resources); err != nil {
		return cm, nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to decode Trainer install manifest", err)
	}
	return cm, resources, nil
}

// deleteTrainerInstallManifest best-effort removes the manifest once its
// resources are gone, so a future run's prune sweep no longer finds it.
func deleteTrainerInstallManifest(ctx context.Context, clientset kubernetes.Interface) {
	delCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	err := clientset.CoreV1().ConfigMaps(trainerNamespace).Delete(delCtx, trainerInstallManifestName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		slog.Warn("Failed to delete Trainer install manifest", "error", err)
	}
}

// removeTrainerInstallManifestEntries removes this run's own resources from
// the persisted manifest, deleting it only once nothing remains. A
// concurrent installer's resources may still be listed, so a full delete
// here would drop them from tracking even though none were removed. A
// missing manifest is a no-op, and conflicts are retried.
func removeTrainerInstallManifestEntries(ctx context.Context, clientset kubernetes.Interface, resources []trainerResourceRef) {
	toRemove := make(map[trainerResourceKey]bool, len(resources))
	for _, ref := range resources {
		toRemove[trainerResourceKeyOf(ref)] = true
	}

	opCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, existing, loadErr := loadTrainerInstallManifest(ctx, clientset)
		if loadErr != nil {
			return loadErr
		}
		if cm == nil {
			return nil
		}

		remaining := make([]trainerResourceRef, 0, len(existing))
		for _, ref := range existing {
			if !toRemove[trainerResourceKeyOf(ref)] {
				remaining = append(remaining, ref)
			}
		}
		if len(remaining) == 0 {
			delErr := clientset.CoreV1().ConfigMaps(trainerNamespace).Delete(opCtx, trainerInstallManifestName,
				metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &cm.ResourceVersion}})
			if k8serrors.IsNotFound(delErr) {
				return nil
			}
			return delErr
		}

		data, marshalErr := json.Marshal(remaining)
		if marshalErr != nil {
			return marshalErr
		}
		cm.Data = map[string]string{trainerInstallManifestKey: string(data)}
		_, updateErr := clientset.CoreV1().ConfigMaps(trainerNamespace).Update(opCtx, cm, metav1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		slog.Warn("Failed to remove this run's entries from the Trainer install manifest", "error", err)
	}
}

// reapOrphanedTrainerInstall best-effort deletes a self-installed Trainer
// abandoned when its installer lost the execution lock before reaching its
// own cleanup. otherNamespacesRemain means another execution might still
// depend on Trainer, so reaping is skipped. A manifest younger than
// NCCLExecutionLockStaleAge might still be mid-install, so that's skipped too.
func reapOrphanedTrainerInstall(ctx context.Context, clientset kubernetes.Interface, dynamicClient dynamic.Interface, otherNamespacesRemain bool) {
	if otherNamespacesRemain {
		return
	}
	cm, resources, err := loadTrainerInstallManifest(ctx, clientset)
	if err != nil {
		slog.Warn("Failed to check for an orphaned Trainer install", "error", err)
		return
	}
	if cm == nil {
		return
	}
	if time.Since(cm.CreationTimestamp.Time) < defaults.NCCLExecutionLockStaleAge {
		return
	}

	resources = trustworthyTrainerResourceRefs(resources)
	if len(resources) == 0 {
		return
	}

	// deleteTrainer's own retries use a fresh background context per call,
	// same as its other callers.
	if err := deleteTrainer(dynamicClient, resources); err != nil { //nolint:contextcheck
		slog.Warn("Failed to reap an orphaned Trainer install", "error", err)
		return
	}
	slog.Info("Reaped a Trainer install orphaned by an abandoned execution")
	deleteTrainerInstallManifest(ctx, clientset)
}

// trustedClusterScopedTrainerGVRs is every cluster-scoped GVR the Trainer
// kustomize overlay can legitimately produce (see decodeTrainerObjects and
// the manifest set built from trainerKustomizePath). Confines
// trustworthyTrainerResourceRefs's cluster-scoped path to exactly these
// kinds, so a manifest entry cannot name some unrelated cluster-scoped
// resource, such as a Namespace this package never installed.
var trustedClusterScopedTrainerGVRs = map[schema.GroupVersionResource]bool{
	trainerCRDGVR:                true,
	trainerValidatingWebhookGVR:  true,
	trainerMutatingWebhookGVR:    true,
	trainerClusterRoleGVR:        true,
	trainerClusterRoleBindingGVR: true,
	trainerNamespaceGVR:          true,
}

// trustworthyTrainerResourceRefs drops manifest entries reapOrphanedTrainerInstall
// must not delete unconditionally. The manifest is a ConfigMap anyone with
// write access to trainerNamespace can edit, and deleteTrainerResource skips
// its UID delete precondition when UID is empty, so an entry with no UID, a
// namespace outside this install's own, or a cluster-scoped kind this
// package never installs could point a delete at any resource on the
// cluster. Every entry this package itself persists always carries the real
// UID it got back from Create and stays within trainerNamespace or one of
// trustedClusterScopedTrainerGVRs, so a rejected entry here is never a
// legitimate one.
func trustworthyTrainerResourceRefs(resources []trainerResourceRef) []trainerResourceRef {
	trusted := make([]trainerResourceRef, 0, len(resources))
	for _, ref := range resources {
		if ref.UID == "" {
			slog.Warn("Refusing to reap a Trainer install manifest entry with no recorded UID",
				"resource", ref.String())
			continue
		}
		if ref.Namespace != "" {
			if ref.Namespace != trainerNamespace {
				slog.Warn("Refusing to reap a Trainer install manifest entry outside the expected namespace",
					"resource", ref.String())
				continue
			}
		} else if !trustedClusterScopedTrainerGVRs[ref.GVR] {
			slog.Warn("Refusing to reap a Trainer install manifest entry with an unexpected cluster-scoped resource type",
				"resource", ref.String())
			continue
		}
		trusted = append(trusted, ref)
	}
	return trusted
}

// waitForTrainerReady blocks until a freshly applied Trainer is usable: the CRDs
// the NCCL test needs are Established, and the controller-manager has a ready
// replica so its admission webhooks have endpoints.
//
// deployment is the controller-manager's live name. The probe discovers it by
// label because an existing chart installation may customize the name, so a
// caller that already located the installation must pass what it found rather
// than let this fall back to the fixed self-install name. Empty means "not
// discovered" (the post-install path, which applied the overlay's own fixed name).
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
// label, since the supported layouts use different names and chart installations
// may customize them.
func findJobSetController(ctx context.Context, dynamicClient dynamic.Interface,
	namespace string) (string, bool, error) {

	return findDeploymentByLabels(ctx, dynamicClient, namespace,
		map[string]string{jobSetNameLabel: jobSetLabelValue}, "JobSet controller")
}

// findTrainerController locates the Trainer controller-manager Deployment by label
// rather than assuming the in-tree Helm or self-install name.
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
func applyTrainerResources(ctx context.Context, dynamicClient dynamic.Interface, clientset kubernetes.Interface,
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
			// Persisted as each resource is created, not once the whole
			// install returns. A kill in between would leave this on the
			// cluster with no record for reapOrphanedTrainerInstall to
			// find, leaking it indefinitely.
			persistTrainerInstallManifest(ctx, clientset, []trainerResourceRef{ref})
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
				// ctx may itself be the reason Create was ambiguous, so
				// persisting under it here risks silently skipping the
				// write. A fresh context keeps this claimed resource's
				// record from leaking untracked.
				persistTrainerInstallManifest(context.Background(), clientset, []trainerResourceRef{claimed}) //nolint:contextcheck
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
		entry, ok := e.(map[string]any)
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
		entry, ok := e.(map[string]any)
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
		cause, map[string]any{"rollbackError": cleanupErr.Error()})
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
// name against an externally managed chart installation with a custom name would
// poll a Deployment that does not exist: waitForDeploymentReady treats NotFound
// as not-ready-yet, so it would burn the full timeout and report a healthy
// controller as never ready.
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
		condition, ok := c.(map[string]any)
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
