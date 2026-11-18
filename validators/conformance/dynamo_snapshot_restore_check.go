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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/validator/catalog"
	"aicr.run/pkg/validator/labels"
	validatorv1 "aicr.run/pkg/validator/v1"
	"aicr.run/validators"
	"aicr.run/validators/internal/allocmode"
	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

const (
	dynamoPlatformComponentName = "dynamo-platform"
	snapshotComponentName       = "snapshot"
	// snapshotDefaultNamespace is where the snapshot component installs when the
	// recipe's componentRef names no namespace.
	snapshotDefaultNamespace = "snapshot"
	snapshotOperatorName     = "snapshot-operator"
	snapshotAgentName        = "snapshot-agent"
	// dynamoOperatorName is the operator Deployment the dynamo-platform release
	// creates (the release is named after the component).
	dynamoOperatorName = "dynamo-platform-dynamo-operator-controller-manager"
	dgdCRDName         = "dynamographdeployments.nvidia.com"

	// The DGD. The name is fixed because the namespace is per run.
	dgdName            = "aicr-snapshot"
	dgdFrontend        = "Frontend"
	dgdWorker          = "VllmDecodeWorker"
	dgdMainContainer   = "main"
	dgdModel           = "Qwen/Qwen3-0.6B"
	dgdPrompt          = "What is the capital of France? Answer with the city name only."
	dgdExpectedAnswer  = "paris"
	dgdFrontendPort    = 8000
	dgdRuntimeImage    = "vllm-runtime"
	dgdSharedMemory    = "2Gi"
	dgdGPUMemoryShare  = "0.3"
	dgdMaxModelLen     = "2048"
	dgdGroveAnnotation = "nvidia.com/enable-grove"
	// keyType is the key of a DGD component's type.
	keyType = "type"

	// Labels Dynamo puts on the pods and Snapshot objects of a DGD.
	dgdNameLabel      = "nvidia.com/dynamo-graph-deployment-name"
	dgdComponentLabel = "nvidia.com/dynamo-component"
	snapshotJobLabel  = "nvidia.com/snapshot-job"

	snapshotRestoreFromAnnotation = "nvidia.com/restore-from"
	snapshotRestoredCondition     = "nvidia.com/Restored"

	// Reasons of the nvidia.com/Restored pod condition that end a restore without
	// success, from Snapshot's api/podcontract (v0.3.0).
	snapshotRestoreFailedReason   = "RestoreFailed"
	snapshotRestoreIncompatReason = "RestoreIncompatible"
	snapshotRestorePartialReason  = "RestorePartiallySucceeded"

	// checkpointDisabledMessage is what Dynamo's admission webhook says when a
	// DGD asks for a checkpoint and the operator has the feature off.
	checkpointDisabledMessage = "checkpoint functionality is disabled in the operator configuration"

	// Condition types and states of the objects the check reads.
	snapshotConditionTrue = "True"
	snapshotJobFailed     = "Failed"
	dgdReadyCondition     = "Ready"
	dgdStateFailed        = "failed"

	// Waits. A capture includes pulling the runtime image on a node that lacks it
	// (about 10 GB), downloading the model and initializing the engine. They fit
	// inside dgdWorkBudget with the teardown and the namespace cleanup.
	dgdCaptureTimeoutDefault  = 15 * time.Minute
	dgdRestoreTimeoutDefault  = 10 * time.Minute
	dgdAnswerTimeoutDefault   = 3 * time.Minute
	dgdTeardownTimeoutDefault = 3 * time.Minute

	// dgdMinWorkBudget is the least a check deadline may leave for the capture and
	// the restore after the teardown reserve: less cannot finish a cold run.
	dgdMinWorkBudget = 10 * time.Minute

	// dgdWorkBudget is the fallback work budget for a 35m check (see
	// driverRuntimeWorkBudget); a live check derives it from its deadline. It is
	// what is left of 35m after the cleanup reserve (90s) and the DGD teardown (3m).
	dgdWorkBudget = 30*time.Minute + 30*time.Second

	snapshotLogTailLines = int64(20)

	// noContainerState is podContainerStateSummary's answer for a pod none of
	// whose containers has terminated.
	noContainerState = "no container state reported"

	// truncateLimit caps the response text quoted in an error.
	truncateLimit = 200

	// unschedulableReason is the prefix podStuckReason gives a pod the scheduler
	// has not placed yet.
	unschedulableReason = "Unschedulable"
)

var (
	snapshotJobGVR = schema.GroupVersionResource{Group: apiGroupNVIDIA, Version: versionV1alpha1, Resource: "snapshotjobs"}

	// The waits and the poll interval are variables so tests can shorten them.
	dgdCaptureTimeout  = dgdCaptureTimeoutDefault
	dgdRestoreTimeout  = dgdRestoreTimeoutDefault
	dgdAnswerTimeout   = dgdAnswerTimeoutDefault
	dgdTeardownTimeout = dgdTeardownTimeoutDefault
	// dgdTeardownOpTimeout bounds the teardown's ownership read and delete, which
	// neither the check's deadline nor SIGTERM may cancel.
	dgdTeardownOpTimeout = 30 * time.Second
	dgdPollInterval      = defaults.PodPollInterval
	// dgdFailedGrace is how long a DGD may report state failed before the check
	// believes it. Dynamo's operator sets failed on any reconcile error, for
	// example a webhook that is not ready yet or a conflict, and clears it on the
	// next good reconcile, so a single read of failed proves little.
	dgdFailedGrace = 30 * time.Second

	// askFrontend asks the frontend pod for a chat completion and returns the raw
	// response body; tests replace it, since a fake clientset has no exec endpoint.
	askFrontend = execFrontendCompletion
)

// frontendScript posts one chat completion to the frontend from inside its pod
// and prints the response body. python3 is in the Dynamo runtime image.
const frontendScript = `import json, sys, urllib.error, urllib.request
model, prompt, port = sys.argv[1], sys.argv[2], sys.argv[3]
body = json.dumps({"model": model, "messages": [{"role": "user", "content": prompt}],
                   "max_tokens": 64, "temperature": 0, "stream": False,
                   "chat_template_kwargs": {"enable_thinking": False}}).encode()
req = urllib.request.Request("http://127.0.0.1:%s/v1/chat/completions" % port, data=body,
                             headers={"Content-Type": "application/json"})
# The pod may set HTTP_PROXY without a loopback bypass; talk to the listener directly.
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
try:
    print(opener.open(req, timeout=60).read().decode())
except urllib.error.HTTPError as exc:
    print("HTTP %d: %s" % (exc.code, exc.read().decode()[:300]))
    sys.exit(1)
`

// dgdRun carries the names of one run.
type dgdRun struct {
	run   *gpuTestRun
	image string
	// runtimeClass is "" when the cluster has no nvidia RuntimeClass.
	runtimeClass string
	// checkpointName is what the DGD reported for the worker.
	checkpointName string
	// dgdSeen is set once the DGD has been read: a DGD that is then gone failed.
	dgdSeen bool
	// createAttempted is set just before the DGD Create is sent, so a Create whose
	// response was lost still gets its DGD cleaned up, and a run that never sent
	// one deletes nothing.
	createAttempted bool
	// failedSince is when the DGD's state was first seen as failed in the current
	// run of polls, or the zero time.
	failedSince time.Time
}

// CheckDynamoSnapshotRestore proves that Snapshot (ai-dynamo/snapshot, GPU
// checkpoint and restore) works through Dynamo, the way a Dynamo recipe uses it.
// It does not drive Snapshot's own API. It deploys a DynamoGraphDeployment
// whose worker has checkpointing on, and checks the outcome through Dynamo:
//
//  1. The Dynamo operator takes the checkpoint: the DGD reports
//     status.checkpoints.<worker>.ready once its SnapshotJob has produced a
//     PodSnapshot. startupPolicy WaitForCheckpoint keeps the serving worker from
//     starting until then, so the worker that serves is a restored one.
//  2. The worker restores from it: the worker pod (the one that is not the
//     capture pod) is Ready, carries nvidia.com/Restored=True, and its
//     nvidia.com/restore-from annotation names the checkpoint the DGD reported.
//  3. The graph serves: the DGD is Ready, and the frontend answers a chat
//     completion with the capital of France.
//
// The model is Qwen/Qwen3-0.6B (public, ungated, about 1.2 GB) on the vLLM
// runtime image of the recipe's dynamo-platform version, the shape AICR's own
// Dynamo smoke test and inference-perf already use. The worker runs Dynamo's own
// entrypoint (python3 -m dynamo.vllm) unwrapped: the operator's restore
// admission accepts a worker container only when it runs python -m <module>
// directly. Grove is switched off for this DGD (nvidia.com/enable-grove), so the
// check does not depend on how the cluster's Dynamo orchestrates workers.
//
// Placement. The worker, and so the capture pod the operator builds from it, is
// allocated through the device plugin (nvidia.com/gpu) on a device-plugin node.
//
// Asking the frontend. The request is made from inside the frontend pod, to
// 127.0.0.1:8000, through exec. That needs neither a route from the validator to
// the pod network nor an extra image; the frontend image ships python3.
//
// Cleanup. Every object lives in a per-run namespace whose cleanup is registered
// before the first Create. The DGD is deleted first and waited on, because its
// finalizer makes Dynamo and Snapshot tear down the pods, the SnapshotJob, the
// PodSnapshot and the stored checkpoint (deletionPolicy Delete, the default),
// which takes minutes and would otherwise hold the GPU after the check returns.
//
// It skips unless the recipe declares both snapshot and dynamo-platform, and once
// they are declared a prerequisite that is missing fails the check.
func CheckDynamoSnapshotRestore(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	for _, component := range []string{snapshotComponentName, dynamoPlatformComponentName} {
		if !validators.RecipeDeclares(ctx, component) {
			return validators.SkipWithReason(validators.SkipReasonComponentNotInRecipe, fmt.Sprintf(
				"%s is not declared in the recipe: this check tests Snapshot through Dynamo, so it needs both snapshot and dynamo-platform", component))
		}
	}

	budget, err := dgdWorkBudgetFor(ctx.Ctx)
	if err != nil {
		return err
	}
	workCtx, cancelWork := ctx.Timeout(budget)
	defer cancelWork()
	work := *ctx
	work.Ctx = workCtx
	ctx = &work

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	if err = verifyDynamoSnapshotInstalled(ctx, dynClient); err != nil {
		return err
	}

	mode, err := detectGPUAllocationMode(ctx.Ctx, ctx.Clientset, dynClient)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "GPU allocation mode",
		"kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable.nvidia\\.com/gpu",
		mode.Summary())
	policy := ctx.ValidationInput.GetGPUAllocationPolicy()
	validators.EmitExtraOrWarn(validators.GPUAllocationPolicyExtra(policy))
	if verifyErr := verifyGPUAllocationPolicy(policy, mode); verifyErr != nil {
		return verifyErr
	}
	if err = requireDevicePluginWorker(policy, mode); err != nil {
		return err
	}
	validators.EmitExtraOrWarn(validators.GPUAllocationResolvedExtra(false, false))

	tag, err := resolveDynamoRuntimeTag(ctx)
	if err != nil {
		return err
	}
	image := catalog.ApplyRegistryOverride(defaults.DynamoImageRepository + "/" + dgdRuntimeImage + ":" + tag)
	recordRawTextArtifact(ctx, "Dynamo runtime image", "",
		fmt.Sprintf("image: %s (the recipe's dynamo-platform version; AICR_VALIDATOR_IMAGE_REGISTRY applies)", image))
	return runDynamoSnapshotTest(ctx, dynClient, mode.DevicePluginNodes, image)
}

// dgdWorkBudgetFor derives the work budget from the check's deadline: what is
// left after the DGD teardown and the namespace cleanup reserve. A deadline that
// leaves less than dgdMinWorkBudget fails before anything is created.
func dgdWorkBudgetFor(parent context.Context) (time.Duration, error) {
	budget, err := gpuCheckWorkBudget(parent, dgdWorkBudget)
	if err != nil {
		return 0, err
	}
	if _, ok := parent.Deadline(); !ok {
		return budget, nil
	}
	budget -= dgdTeardownTimeout
	if budget < dgdMinWorkBudget {
		return 0, errors.New(errors.ErrCodeTimeout, fmt.Sprintf(
			"check timeout too short for a cold Dynamo checkpoint and restore: %s of work time remain after the cleanup and teardown reserve but at least %s is needed — no test resources were created; raise the check's catalog timeout",
			budget.Round(time.Second), dgdMinWorkBudget))
	}
	return budget, nil
}

// requireDevicePluginWorker fails when the worker cannot be allocated through
// the device plugin: a recipe that pins DRA allocation, or a cluster without
// device-plugin GPUs.
func requireDevicePluginWorker(policy string, mode *allocmode.Mode) error {
	if policy == validatorv1.GPUAllocationPolicyDRAResourceClaim {
		return errors.New(errors.ErrCodeUnavailable,
			"the Dynamo worker of this check is allocated through the device plugin (nvidia.com/gpu), but the recipe's GPU allocation policy is "+policy)
	}
	if !mode.DevicePluginUsable || len(mode.DevicePluginNodes) == 0 {
		return errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
			"no device-plugin GPU node for the Dynamo worker: %s", mode.DevicePluginDetail))
	}
	return nil
}

// componentNamespace returns the namespace the recipe installs a component
// into, or def.
func componentNamespace(ctx *validators.Context, component, def string) string {
	if ctx.ValidationInput != nil {
		for _, ref := range ctx.ValidationInput.ComponentRefs {
			if ref.Name == component && ref.Namespace != "" {
				return ref.Namespace
			}
		}
	}
	return def
}

// verifyDynamoSnapshotInstalled requires Dynamo's operator, the DGD API and
// Snapshot's operator and node agent to be up before any GPU is requested, so a
// missing install reads as one.
func verifyDynamoSnapshotInstalled(ctx *validators.Context, dynClient dynamic.Interface) error {
	dynamoNS := componentNamespace(ctx, dynamoPlatformComponentName, namespaceDynamoSystem)
	if err := verifyDeploymentAvailable(ctx, dynamoNS, dynamoOperatorName); err != nil {
		return wrapWithCode(err, errors.ErrCodeUnavailable,
			fmt.Sprintf("Dynamo operator %s/%s is not available", dynamoNS, dynamoOperatorName))
	}
	if _, err := requireCRDEstablished(ctx.Ctx, dynClient, dgdCRDName); err != nil {
		return wrapWithCode(err, errors.ErrCodeUnavailable,
			"the DynamoGraphDeployment API is not served: is dynamo-platform installed?")
	}
	snapshotNS := componentNamespace(ctx, snapshotComponentName, snapshotDefaultNamespace)
	if err := verifyDeploymentAvailable(ctx, snapshotNS, snapshotOperatorName); err != nil {
		return wrapWithCode(err, errors.ErrCodeUnavailable,
			fmt.Sprintf("snapshot operator %s/%s is not available", snapshotNS, snapshotOperatorName))
	}
	if err := verifyDaemonSetReady(ctx, snapshotNS, snapshotAgentName); err != nil {
		return wrapWithCode(err, errors.ErrCodeUnavailable,
			fmt.Sprintf("snapshot node agent %s/%s is not ready", snapshotNS, snapshotAgentName))
	}
	return nil
}

// resolveDynamoRuntimeTag returns the tag of the Dynamo runtime image: the
// recipe's dynamo-platform version, so the runtime matches the deployed operator,
// or defaults.DynamoRuntimeImageTag when the ref carries none. This mirrors
// validators/performance (dynamo_runtime_image.go), which inference-perf uses;
// keep the two in sync.
func resolveDynamoRuntimeTag(ctx *validators.Context) (string, error) {
	raw, source := "", "defaults.DynamoRuntimeImageTag"
	if ctx.ValidationInput != nil {
		for _, ref := range ctx.ValidationInput.ComponentRefs {
			if ref.Name == dynamoPlatformComponentName && strings.TrimSpace(ref.Version) != "" {
				raw, source = strings.TrimSpace(ref.Version), "recipe component "+dynamoPlatformComponentName
			}
		}
	}
	if raw == "" {
		raw = defaults.DynamoRuntimeImageTag
	}
	// The strict parse also keeps arbitrary input out of the image reference.
	// Build metadata is rejected because "+" is not valid in an image tag.
	v, err := semver.StrictNewVersion(strings.TrimPrefix(raw, "v"))
	if err == nil && v.Metadata() != "" {
		err = errors.New(errors.ErrCodeInvalidRequest, "build metadata is not valid in an image tag")
	}
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"invalid %s version %q from %s: not usable as a Dynamo runtime image tag", dynamoPlatformComponentName, raw, source), err)
	}
	slog.Info("Resolved Dynamo runtime image tag", "tag", v.String(), "source", source)
	return v.String(), nil
}

// runDynamoSnapshotTest creates the per-run namespace and the DGD, follows the
// checkpoint and the restore through Dynamo, and asks the frontend. The return is
// NAMED so the deferred cleanup can fail an otherwise-passing check when cleanup
// terminally fails; a primary error is never overwritten.
func runDynamoSnapshotTest(ctx *validators.Context, dynClient dynamic.Interface, nodes []string, image string) (err error) {
	run, runErr := newGPUTestRun()
	if runErr != nil {
		return runErr
	}
	r := &dgdRun{run: run, image: image}

	// Register cleanup BEFORE the first Create: an ambiguously created DGD would
	// otherwise stay bound and hold a GPU (see cleanupGPUTestNamespace).
	defer func() { //nolint:contextcheck // cleanup runs on its own bounded context
		teardown := deleteDGDAndWait(ctx, dynClient, r)
		recordRawTextArtifact(ctx, "Delete DynamoGraphDeployment",
			fmt.Sprintf("kubectl get dynamographdeployment %s -n %s --ignore-not-found", dgdName, run.namespace), teardown)
		status, cleanupErr := cleanupGPUTestNamespace(ctx.Clientset, run)
		recordRawTextArtifact(ctx, "Cleanup test namespace", cleanupInspectEquivalent(run.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()

	if err = createGPUTestNamespace(ctx.Ctx, ctx.Clientset, run); err != nil {
		return err
	}
	if r.runtimeClass, err = detectNvidiaRuntimeClass(ctx); err != nil {
		return err
	}

	// --- capture ---
	start := time.Now()
	dgd, err := buildDGD(r, ctx.Tolerations, nodes, ctx.NodeSelector)
	if err != nil {
		return err
	}
	r.createAttempted = true
	if _, err = dynClient.Resource(dgdGVR).Namespace(run.namespace).Create(ctx.Ctx, dgd, metav1.CreateOptions{}); err != nil {
		if strings.Contains(err.Error(), checkpointDisabledMessage) {
			return errors.Wrap(errors.ErrCodeUnavailable,
				"the Dynamo operator has checkpointing disabled: set dynamo-operator.checkpoint.enabled to true on dynamo-platform (the dynamo-snapshot mixin does)", err)
		}
		return errors.Wrap(errors.ErrCodeInternal, "failed to create the DynamoGraphDeployment", err)
	}
	last, captureErr := waitForCheckpoint(ctx, dynClient, r)
	captureTook := time.Since(start).Round(time.Second)
	recordRawTextArtifact(ctx, "DynamoGraphDeployment after capture",
		fmt.Sprintf("kubectl get dynamographdeployment %s -n %s -o yaml", dgdName, run.namespace), describeDGD(last))
	if captureErr != nil {
		return captureErr
	}
	r.checkpointName = dgdCheckpoint(last).name
	recordRawTextArtifact(ctx, "Checkpoint",
		fmt.Sprintf("kubectl get podsnapshot %s -n %s", r.checkpointName, run.namespace),
		fmt.Sprintf("PodSnapshot: %s/%s\nCapture took: %s (image pull, model download, engine start and the checkpoint)", run.namespace, r.checkpointName, captureTook))

	// --- restore ---
	restoreStart := time.Now()
	worker, restoreErr := waitForRestoredWorker(ctx, dynClient, r)
	restoreTook := time.Since(restoreStart).Round(time.Second)
	recordRawTextArtifact(ctx, "Restored worker",
		fmt.Sprintf("kubectl get pods -n %s -l %s=%s,%s=%s", run.namespace, dgdNameLabel, dgdName, dgdComponentLabel, dgdWorker),
		describeWorker(worker, r.checkpointName))
	if restoreErr != nil {
		return restoreErr
	}

	// --- serve ---
	answer, askErr := waitForAnswer(ctx, r)
	if askErr != nil {
		return askErr
	}
	recordRawTextArtifact(ctx, "Frontend answer", "",
		fmt.Sprintf("prompt: %q\nanswer: %q\nrestore took: %s (worker created to graph Ready)\nverdict: PASS", dgdPrompt, answer, restoreTook))
	return nil
}

// detectNvidiaRuntimeClass returns the nvidia RuntimeClass name (the one the GPU
// Operator creates) when the cluster has it, else "". Without it the pods use the
// node's default runtime. Only a RuntimeClass that is absent (NotFound) falls back
// to the default: a read that was denied or failed says nothing about whether the
// class exists, so it is an error rather than a silent change of runtime.
func detectNvidiaRuntimeClass(ctx *validators.Context) (string, error) {
	_, err := ctx.Clientset.NodeV1().RuntimeClasses().Get(ctx.Ctx, nvidiaRuntimeClassName, metav1.GetOptions{})
	switch {
	case err == nil:
		return nvidiaRuntimeClassName, nil
	case k8serrors.IsNotFound(err):
		return "", nil
	case k8serrors.IsForbidden(err):
		return "", errors.Wrap(errors.ErrCodeUnauthorized,
			"not permitted to get RuntimeClass "+nvidiaRuntimeClassName+" (verb get, resource runtimeclasses.node.k8s.io): grant it to the validator", err)
	default:
		return "", classifyK8sReadError(err, "RuntimeClass "+nvidiaRuntimeClassName)
	}
}

// podTolerations returns the pod tolerations: the context's when set, else
// tolerate-all, as the other GPU probes do.
func podTolerations(tolerations []corev1.Toleration) []corev1.Toleration {
	if tolerations != nil {
		return tolerations
	}
	return []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
}

func runtimeClassPtr(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

// buildDGD returns the DynamoGraphDeployment: a frontend and one vLLM worker
// whose checkpoint Dynamo takes before the worker serves (WaitForCheckpoint). The
// worker is confined to nodes (the device-plugin GPU nodes) and, when the caller
// set one (validators.Context.NodeSelector, from AICR_NODE_SELECTOR), also to the
// nodes that selector matches, so an override that names a node pool is honored.
func buildDGD(r *dgdRun, tolerations []corev1.Toleration, nodes []string, nodeSelector map[string]string) (*unstructured.Unstructured, error) {
	env := []corev1.EnvVar{
		{Name: "DYN_REQUEST_PLANE", Value: "tcp"},
		{Name: "DYN_EVENT_PLANE", Value: "zmq"},
	}
	frontend := corev1.PodSpec{
		NodeSelector:     nodeSelector,
		Tolerations:      podTolerations(tolerations),
		ImagePullSecrets: r.run.imagePullSecretRefs(),
		Containers: []corev1.Container{{
			Name:  dgdMainContainer,
			Image: r.image,
			Env: append([]corev1.EnvVar{
				{Name: "SERVED_MODEL_NAME", Value: dgdModel},
				{Name: "DYN_ROUTER_MODE", Value: "round-robin"},
			}, env...),
		}},
	}
	worker := corev1.PodSpec{
		RuntimeClassName: runtimeClassPtr(r.runtimeClass),
		NodeSelector:     nodeSelector,
		Tolerations:      podTolerations(tolerations),
		Affinity:         nodeNameAffinity(nodes),
		ImagePullSecrets: r.run.imagePullSecretRefs(),
		Containers: []corev1.Container{{
			Name:       dgdMainContainer,
			Image:      r.image,
			WorkingDir: "/workspace/examples/backends/vllm",
			// Dynamo's own entrypoint, unwrapped: restore admission accepts a
			// worker container only when it runs python -m <module> directly.
			Command: []string{gatewayTrafficInterpreter, "-m", "dynamo.vllm"},
			Args:    []string{"--model", dgdModel, "--gpu-memory-utilization", dgdGPUMemoryShare, "--max-model-len", dgdMaxModelLen},
			Env:     env,
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse("1"),
			}},
		}},
	}
	frontendMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&frontend)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to convert the frontend pod spec", err)
	}
	workerMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&worker)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to convert the worker pod spec", err)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": dgdGVR.Group + "/" + dgdGVR.Version,
		"kind":       "DynamoGraphDeployment",
		keyMetadata: map[string]any{
			keyName:       dgdName,
			"namespace":   r.run.namespace,
			"labels":      map[string]any{labels.GPUTestRun: r.run.token},
			"annotations": map[string]any{dgdGroveAnnotation: "false"},
		},
		keySpec: map[string]any{
			"backendFramework": "vllm",
			"components": []any{
				map[string]any{
					keyName: dgdFrontend, keyType: "frontend", "replicas": int64(1),
					"podTemplate": map[string]any{keySpec: frontendMap},
				},
				map[string]any{
					keyName: dgdWorker, keyType: "worker", "replicas": int64(1),
					"sharedMemorySize": dgdSharedMemory,
					"experimental": map[string]any{"checkpoint": map[string]any{
						"enabled": true, "startupPolicy": "WaitForCheckpoint",
					}},
					"podTemplate": map[string]any{keySpec: workerMap},
				},
			},
		},
	}}, nil
}

// condition is one entry of a status.conditions list.
type condition struct {
	Status  string
	Reason  string
	Message string
}

// conditionsOf indexes obj's status.conditions by type.
func conditionsOf(obj *unstructured.Unstructured) map[string]condition {
	out := map[string]condition{}
	if obj == nil {
		return out
	}
	items, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		out[t] = condition{Status: stringField(m, "status"), Reason: stringField(m, "reason"), Message: stringField(m, "message")}
	}
	return out
}

// sanitizeLine makes text that a controller or a pod supplied safe to quote on one
// line of the validator's output: every control character, line breaks included,
// becomes a space, so a message cannot start a line that looks like another
// artifact section or result (CWE-117).
func sanitizeLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// sanitizeLog is sanitizeLine for a multi-line log tail: line breaks and tabs
// stay, other control characters (terminal escapes among them) become spaces.
func sanitizeLog(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, s)
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// checkpointStatus is what the DGD reports for the worker's checkpoint.
type checkpointStatus struct {
	name  string
	ready bool
}

// dgdCheckpoint reads status.checkpoints.<worker> of a DGD.
func dgdCheckpoint(obj *unstructured.Unstructured) checkpointStatus {
	if obj == nil {
		return checkpointStatus{}
	}
	m, found, _ := unstructured.NestedMap(obj.Object, "status", "checkpoints", dgdWorker)
	if !found {
		return checkpointStatus{}
	}
	ready, _ := m["ready"].(bool)
	return checkpointStatus{name: stringField(m, "checkpointName"), ready: ready}
}

// describeDGD renders the DGD's state, conditions and checkpoint for evidence.
func describeDGD(obj *unstructured.Unstructured) string {
	if obj == nil {
		return "the DynamoGraphDeployment was never read"
	}
	state, _, _ := unstructured.NestedString(obj.Object, "status", "state")
	var b strings.Builder
	fmt.Fprintf(&b, "Name: %s/%s\nState: %s\n", obj.GetNamespace(), obj.GetName(), sanitizeLine(valueOrUnknown(state)))
	if c, ok := conditionsOf(obj)[dgdReadyCondition]; ok {
		fmt.Fprintf(&b, "Ready: %s (%s) %s\n", sanitizeLine(c.Status), sanitizeLine(c.Reason), sanitizeLine(c.Message))
	}
	cp := dgdCheckpoint(obj)
	fmt.Fprintf(&b, "Checkpoint: %s ready=%t", sanitizeLine(valueOrUnknown(cp.name)), cp.ready)
	return b.String()
}

// getDGD reads the DGD, tolerating a read that could not land: it returns
// (nil, nil) for a transient failure or a DGD not visible yet.
func getDGD(c context.Context, dynClient dynamic.Interface, r *dgdRun) (obj *unstructured.Unstructured, retry bool, err error) {
	obj, getErr := dynClient.Resource(dgdGVR).Namespace(r.run.namespace).Get(c, dgdName, metav1.GetOptions{})
	switch {
	case getErr == nil:
		r.dgdSeen = true
		r.trackFailedState(obj)
		return obj, false, nil
	case k8serrors.IsNotFound(getErr):
		if r.dgdSeen {
			return nil, false, errors.Wrap(errors.ErrCodeNotFound,
				"the DynamoGraphDeployment disappeared after it was created: something deleted it", getErr)
		}
		return nil, true, nil
	case isK8sTimeoutErr(getErr):
		return nil, true, nil
	default:
		return nil, false, classifyK8sReadError(getErr, "DynamoGraphDeployment "+dgdName)
	}
}

// dgdPods lists the pods of the DGD. A list that failed is an error, not an
// empty list: a poll that could not see the pods must not judge them.
func dgdPods(c context.Context, ctx *validators.Context, r *dgdRun) ([]corev1.Pod, error) {
	pods, err := ctx.Clientset.CoreV1().Pods(r.run.namespace).List(c, metav1.ListOptions{LabelSelector: dgdNameLabel + "=" + dgdName})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// isCapturePod reports whether the pod is the one the operator runs to take the
// checkpoint.
func isCapturePod(pod *corev1.Pod) bool {
	_, ok := pod.Labels[snapshotJobLabel]
	return ok
}

// workerPod returns the pod that serves the worker component: the newest worker
// pod that is not the capture pod.
func workerPod(pods []corev1.Pod) *corev1.Pod {
	var newest *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.Labels[dgdComponentLabel] != dgdWorker || isCapturePod(pod) {
			continue
		}
		if newest == nil || pod.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = pod
		}
	}
	return newest
}

// frontendPod returns a Ready frontend pod, or nil.
func frontendPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		if pods[i].Labels[dgdComponentLabel] == dgdFrontend && podIsReady(&pods[i]) {
			return &pods[i]
		}
	}
	return nil
}

// checkpointKilledCapturePod reports whether the capture pod ended the way a
// successful checkpoint ends it. Snapshot always terminates the checkpointed
// process: the container exits 137 with reason Error, seconds before the
// SnapshotJob completes and Dynamo reports the checkpoint ready. A different exit
// code, or an OOM kill, is the engine failing.
func checkpointKilledCapturePod(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.ExitCode == 137 && t.Reason == "Error" {
			return true
		}
	}
	return false
}

// podLastExit renders each container's previous termination, which is where a
// crash-looping pod keeps its exit code.
func podLastExit(pod *corev1.Pod) string {
	var parts []string
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.LastTerminationState.Terminated; t != nil {
			part := fmt.Sprintf("%s exited %d (%s)", cs.Name, t.ExitCode, sanitizeLine(t.Reason))
			if t.Message != "" {
				part += ": " + sanitizeLine(t.Message)
			}
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "; ")
}

// trackFailedState keeps the clock of an unbroken stretch of the DGD's state
// failed: it starts when the state is first seen as failed and restarts after any
// other state. It runs on every DGD read (getDGD), whatever the wait is doing with
// the result, so a recovery seen by one wait is not charged to the next. It
// reports whether the DGD has now been failed for the whole grace period.
func (r *dgdRun) trackFailedState(dgd *unstructured.Unstructured) bool {
	if state, _, _ := unstructured.NestedString(dgd.Object, "status", "state"); state != dgdStateFailed {
		r.failedSince = time.Time{}
		return false
	}
	if r.failedSince.IsZero() {
		r.failedSince = time.Now()
	}
	return time.Since(r.failedSince) >= dgdFailedGrace
}

// forbiddenError returns an ErrCodeUnauthorized error naming the verb and the
// resource when err is a Forbidden, else nil: a denied read must be reported as
// the permission it lacks, not retried until a timeout hides it.
func forbiddenError(err error, verb, resource, namespace string) error {
	if !k8serrors.IsForbidden(err) {
		return nil
	}
	return errors.Wrap(errors.ErrCodeUnauthorized, fmt.Sprintf(
		"not permitted to %s %s in namespace %s: grant it to the validator", verb, resource, namespace), err)
}

// dgdFailure returns why the graph cannot reach the state the check waits for,
// or "" while it may still: a DGD that stays failed, a failed checkpoint
// SnapshotJob, or a pod that cannot start or has failed. A capture pod that the
// checkpoint itself ended is not a failure, and once the checkpoint is ready
// (checkpointReady) the capture pod no longer matters: it stays Failed until the
// graph is deleted, and the SnapshotJob's own condition is the authority. A pod
// the scheduler has not placed yet is not a failure either (a GPU may be freed on
// the next poll); the wait's own timeout reports it. The error is set when the
// check lacks the permission to look.
func dgdFailure(c context.Context, ctx *validators.Context, dynClient dynamic.Interface, r *dgdRun, dgd *unstructured.Unstructured, checkpointReady bool) (string, error) {
	pods, listErr := dgdPods(c, ctx, r)
	if denied := forbiddenError(listErr, "list", "pods", r.run.namespace); denied != nil {
		return "", denied
	}
	return dgdFailureOfPods(c, ctx, dynClient, r, dgd, checkpointReady, pods, listErr == nil)
}

// dgdFailureOfPods is dgdFailure over a pod list the caller already took, so a
// wait that also picks its verdict from that list judges both from one snapshot.
// podsListed is false when the list failed for a reason other than a denial; the
// pods are then not judged.
func dgdFailureOfPods(c context.Context, ctx *validators.Context, dynClient dynamic.Interface, r *dgdRun, dgd *unstructured.Unstructured, checkpointReady bool, pods []corev1.Pod, podsListed bool) (string, error) {
	if r.trackFailedState(dgd) {
		return fmt.Sprintf("the DynamoGraphDeployment stayed failed for %s: %s", dgdFailedGrace, describeDGD(dgd)), nil
	}
	jobs, err := dynClient.Resource(snapshotJobGVR).Namespace(r.run.namespace).List(c, metav1.ListOptions{LabelSelector: dgdNameLabel + "=" + dgdName})
	if denied := forbiddenError(err, "list", "snapshotjobs."+apiGroupNVIDIA, r.run.namespace); denied != nil {
		return "", denied
	}
	if err == nil {
		for i := range jobs.Items {
			if f, ok := conditionsOf(&jobs.Items[i])[snapshotJobFailed]; ok && f.Status == snapshotConditionTrue {
				return fmt.Sprintf("Dynamo's checkpoint SnapshotJob %s failed (%s): %s", jobs.Items[i].GetName(), sanitizeLine(f.Reason), sanitizeLine(f.Message)), nil
			}
		}
	}
	if !podsListed {
		return "", nil
	}
	current := workerPod(pods)
	for _, pod := range pods {
		if isCapturePod(&pod) && checkpointReady {
			continue
		}
		// A worker pod that a newer worker has replaced (an evicted pod stays Failed
		// next to its replacement) says nothing about the graph: the verdict is about
		// the newest worker, which is the one the restore wait selects.
		if current != nil && pod.Labels[dgdComponentLabel] == dgdWorker && !isCapturePod(&pod) && pod.Name != current.Name {
			continue
		}
		if reason := podStuckReason(&pod); reason != "" && !strings.HasPrefix(reason, unschedulableReason) {
			detail := ""
			if last := podLastExit(&pod); last != "" {
				detail += "; last exit: " + last
			}
			if strings.Contains(reason, waitingReasonCrashLoopBackOff) {
				detail += "\nlogs: " + podLogs(c, ctx, &pod, true)
			}
			return fmt.Sprintf("pod %s stuck: %s%s", pod.Name, sanitizeLine(reason), detail), nil
		}
		if pod.Status.Phase == corev1.PodFailed && (!isCapturePod(&pod) || !checkpointKilledCapturePod(&pod)) {
			return fmt.Sprintf("pod %s failed: %s\nlogs: %s", pod.Name, podContainerStateSummary(&pod), podLogs(c, ctx, &pod, false)), nil
		}
	}
	return "", nil
}

// waitForCheckpoint polls the DGD until Dynamo reports the worker's checkpoint
// ready, or the capture timeout elapses. It returns the last DGD read.
func waitForCheckpoint(ctx *validators.Context, dynClient dynamic.Interface, r *dgdRun) (*unstructured.Unstructured, error) {
	waitCtx, cancel := context.WithTimeout(ctx.Ctx, dgdCaptureTimeout)
	defer cancel()

	var last *unstructured.Unstructured
	err := wait.PollUntilContextCancel(waitCtx, dgdPollInterval, true, func(c context.Context) (bool, error) {
		obj, retry, getErr := getDGD(c, dynClient, r)
		if getErr != nil {
			return false, getErr
		}
		if retry {
			return false, nil
		}
		last = obj
		if cp := dgdCheckpoint(obj); cp.ready && cp.name != "" {
			return true, nil
		}
		pods, listErr := dgdPods(c, ctx, r)
		if denied := forbiddenError(listErr, "list", "pods", r.run.namespace); denied != nil {
			return false, denied
		}
		failure, failErr := dgdFailureOfPods(c, ctx, dynClient, r, obj, false, pods, listErr == nil)
		if failErr != nil {
			return false, failErr
		}
		if failure != "" {
			return false, errors.New(errors.ErrCodeInternal, "the Dynamo checkpoint did not complete: "+failure)
		}
		return false, nil
	})
	if err == nil {
		return last, nil
	}
	if ctx.Ctx.Err() != nil {
		return last, errors.Wrap(errors.ErrCodeTimeout, "waiting for the Dynamo checkpoint canceled", ctx.Ctx.Err())
	}
	if waitCtx.Err() != nil {
		return last, errors.New(errors.ErrCodeTimeout, fmt.Sprintf(
			"Dynamo did not report the worker's checkpoint ready in %s (the capture pulls %s, downloads %s and starts the engine); the DGD was: %s%s",
			dgdCaptureTimeout, dgdRuntimeImage, dgdModel, strings.ReplaceAll(describeDGD(last), "\n", "; "), podsSummary(ctx, r)))
	}
	return last, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed waiting for the Dynamo checkpoint")
}

// waitForRestoredWorker polls until the DGD is Ready and the worker pod is Ready,
// restored (nvidia.com/Restored=True) and restored from the checkpoint the DGD
// reported. It returns the last worker pod seen.
func waitForRestoredWorker(ctx *validators.Context, dynClient dynamic.Interface, r *dgdRun) (*corev1.Pod, error) {
	waitCtx, cancel := context.WithTimeout(ctx.Ctx, dgdRestoreTimeout)
	defer cancel()

	var lastPod *corev1.Pod
	var lastDGD *unstructured.Unstructured
	err := wait.PollUntilContextCancel(waitCtx, dgdPollInterval, true, func(c context.Context) (bool, error) {
		obj, retry, getErr := getDGD(c, dynClient, r)
		if getErr != nil {
			return false, getErr
		}
		if retry {
			return false, nil
		}
		lastDGD = obj
		pods, listErr := dgdPods(c, ctx, r)
		if denied := forbiddenError(listErr, "list", "pods", r.run.namespace); denied != nil {
			return false, denied // a denied list never recovers: name the permission now
		}
		if listErr != nil {
			return false, nil // this poll could not see the pods; judge the next one
		}
		// The verdict uses the worker of THIS poll; lastPod is kept for the evidence
		// only, so a worker that vanished since an earlier poll cannot be what a
		// Ready graph is credited to.
		pod := workerPod(pods)
		if pod != nil {
			lastPod = pod
			if failure := snapshotRestoreFailure(pod); failure != "" {
				return false, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
					"the Dynamo worker %s did not restore from checkpoint %s: %s", pod.Name, r.checkpointName, failure))
			}
		}
		// The same list gives the verdict's worker and the failure scan, so a worker
		// deleted between two lists cannot pass one and escape the other.
		failure, failErr := dgdFailureOfPods(c, ctx, dynClient, r, obj, true, pods, true)
		if failErr != nil {
			return false, failErr
		}
		if failure != "" {
			return false, errors.New(errors.ErrCodeInternal, "the Dynamo graph did not come up from the checkpoint: "+failure)
		}
		return restoredWorkerReady(pod, r.checkpointName) && dgdIsReady(obj), nil
	})
	if err == nil {
		return lastPod, nil
	}
	if ctx.Ctx.Err() != nil {
		return lastPod, errors.Wrap(errors.ErrCodeTimeout, "waiting for the Dynamo restore canceled", ctx.Ctx.Err())
	}
	if waitCtx.Err() != nil {
		return lastPod, errors.New(errors.ErrCodeTimeout, fmt.Sprintf(
			"the Dynamo graph was not Ready with a restored worker in %s: worker: %s; %s%s",
			dgdRestoreTimeout, describeWorker(lastPod, r.checkpointName), strings.ReplaceAll(describeDGD(lastDGD), "\n", "; "), podsSummary(ctx, r)))
	}
	return lastPod, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed waiting for the Dynamo restore")
}

// dgdIsReady reports whether the DGD's Ready condition is True.
func dgdIsReady(obj *unstructured.Unstructured) bool {
	c, ok := conditionsOf(obj)[dgdReadyCondition]
	return ok && c.Status == snapshotConditionTrue
}

// restoredWorkerReady reports whether the worker pod is Ready, restored, and
// restored from the named checkpoint.
func restoredWorkerReady(pod *corev1.Pod, checkpointName string) bool {
	if pod == nil || !podIsReady(pod) || pod.Annotations[snapshotRestoreFromAnnotation] != checkpointName {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if string(cond.Type) == snapshotRestoredCondition {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// snapshotRestoreFailure returns why the restore cannot succeed, or "" while it
// may still: a failed Restored condition or a worker pod that has ended.
func snapshotRestoreFailure(pod *corev1.Pod) string {
	for _, cond := range pod.Status.Conditions {
		if string(cond.Type) != snapshotRestoredCondition || cond.Status == corev1.ConditionTrue {
			continue
		}
		switch cond.Reason {
		case snapshotRestoreFailedReason, snapshotRestoreIncompatReason, snapshotRestorePartialReason:
			return fmt.Sprintf("%s: %s", sanitizeLine(cond.Reason), sanitizeLine(cond.Message))
		}
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return "the worker pod ended (" + podContainerStateSummary(pod) + "); a restored pod's container ends when the restored process does, and the restore proxy exits 112 when the pod's one restore was already used and 113 when it lost the restored process"
	}
	return ""
}

// describeWorker summarizes how far the worker pod got.
func describeWorker(pod *corev1.Pod, checkpointName string) string {
	if pod == nil {
		return "no worker pod was created"
	}
	restoredFrom := pod.Annotations[snapshotRestoreFromAnnotation]
	msg := fmt.Sprintf("Name: %s/%s\nNode: %s\nPhase: %s, ready=%t\nrestore-from: %q (the checkpoint is %q)",
		pod.Namespace, pod.Name, valueOrUnknown(pod.Spec.NodeName), pod.Status.Phase, podIsReady(pod), restoredFrom, checkpointName)
	for _, cond := range pod.Status.Conditions {
		if string(cond.Type) == snapshotRestoredCondition {
			msg += fmt.Sprintf("\nRestored: %s (%s) %s", cond.Status, sanitizeLine(cond.Reason), sanitizeLine(cond.Message))
		}
	}
	if waiting := podWaitingStatus(pod); waiting != valueNone {
		msg += "\nwaiting: " + waiting
	}
	return msg
}

// podsSummary lists the DGD's pods for a timeout message.
func podsSummary(ctx *validators.Context, r *dgdRun) string {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx.Ctx), defaults.K8sCleanupTimeout)
	defer cancel()
	var b strings.Builder
	pods, listErr := dgdPods(c, ctx, r)
	if listErr != nil {
		return "\n  (could not list the pods: " + listErr.Error() + ")"
	}
	for _, pod := range pods {
		role := pod.Labels[dgdComponentLabel]
		if isCapturePod(&pod) {
			role += " (capture)"
		}
		fmt.Fprintf(&b, "\n  pod %s [%s]: phase=%s, %s", pod.Name, role, pod.Status.Phase, podWaitingStatus(&pod))
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				fmt.Fprintf(&b, "; not scheduled: %s", sanitizeLine(cond.Message))
			}
		}
	}
	if b.Len() == 0 {
		return "\n  no pods were created"
	}
	return "\npods:" + b.String()
}

// podContainerStateSummary renders each container's terminated state.
func podContainerStateSummary(pod *corev1.Pod) string {
	var parts []string
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			part := fmt.Sprintf("%s exited %d (%s)", cs.Name, t.ExitCode, sanitizeLine(t.Reason))
			if t.Message != "" {
				part += ": " + sanitizeLine(t.Message)
			}
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return noContainerState
	}
	return strings.Join(parts, "; ")
}

// podLogs returns the tail of the pod's main container logs (those of the
// previous container when previous is set, for a pod in CrashLoopBackOff), or the
// read error, for a failure message.
func podLogs(c context.Context, ctx *validators.Context, pod *corev1.Pod, previous bool) string {
	tail := snapshotLogTailLines
	raw, err := ctx.Clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: dgdMainContainer, TailLines: &tail, Previous: previous,
	}).DoRaw(c)
	if err != nil {
		return fmt.Sprintf("(could not read logs: %s)", sanitizeLine(err.Error()))
	}
	return sanitizeLog(strings.TrimSpace(string(raw)))
}

// waitForAnswer asks the frontend for a chat completion until it answers with
// the expected content or the answer timeout elapses. The frontend can answer
// "model not found" for a moment after the graph is Ready, so it retries.
func waitForAnswer(ctx *validators.Context, r *dgdRun) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx.Ctx, dgdAnswerTimeout)
	defer cancel()

	var lastProblem string
	var answer string
	err := wait.PollUntilContextCancel(waitCtx, dgdPollInterval, true, func(c context.Context) (bool, error) {
		pods, listErr := dgdPods(c, ctx, r)
		if listErr != nil {
			lastProblem = "could not list the graph's pods: " + listErr.Error()
			return false, nil
		}
		frontend := frontendPod(pods)
		if frontend == nil {
			lastProblem = "no Ready frontend pod"
			return false, nil
		}
		body, askErr := askFrontend(c, ctx, r, frontend)
		if askErr != nil {
			lastProblem = askErr.Error()
			return false, nil
		}
		content, parseErr := completionContent(body)
		if parseErr != nil {
			lastProblem = parseErr.Error()
			return false, nil
		}
		if normalizeAnswer(content) != dgdExpectedAnswer {
			return false, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"the restored Dynamo graph answered %q to %q, want %q", truncate(sanitizeLine(content)), dgdPrompt, dgdExpectedAnswer))
		}
		answer = strings.TrimSpace(content)
		return true, nil
	})
	if err == nil {
		return answer, nil
	}
	if ctx.Ctx.Err() != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout, "waiting for the frontend's answer canceled", ctx.Ctx.Err())
	}
	if waitCtx.Err() != nil {
		return "", errors.New(errors.ErrCodeTimeout, fmt.Sprintf(
			"the frontend gave no answer in %s (last problem: %s)", dgdAnswerTimeout, lastProblem))
	}
	return "", err
}

// normalizeAnswer lowercases a one-word answer and drops the whitespace,
// punctuation and markdown emphasis a model puts around it, so "Paris.", " **Paris** "
// and "paris" are the same answer while "Paris is not the capital" is not.
func normalizeAnswer(s string) string {
	return strings.ToLower(strings.Trim(s, " \t\r\n.!*_`\"'"))
}

// completionContent returns choices[0].message.content of a chat completion.
func completionContent(body string) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &resp); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "the frontend's response is not a chat completion: "+truncate(body), err)
	}
	if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return "", errors.New(errors.ErrCodeInternal, "the frontend's chat completion has no content: "+truncate(body))
	}
	return resp.Choices[0].Message.Content, nil
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= truncateLimit {
		return s
	}
	return s[:truncateLimit] + "..."
}

// execFrontendCompletion posts the chat completion from inside the frontend pod
// and returns the response body.
func execFrontendCompletion(c context.Context, ctx *validators.Context, r *dgdRun, frontend *corev1.Pod) (string, error) {
	execCtx, cancel := context.WithTimeout(c, 90*time.Second)
	defer cancel()
	res, err := execPodCommand(execCtx, ctx, r.run.namespace, frontend.Name,
		[]string{gatewayTrafficInterpreter, "-c", frontendScript, dgdModel, dgdPrompt, fmt.Sprint(dgdFrontendPort)},
		podExecOptions{PreferredContainerName: dgdMainContainer})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "could not ask the frontend: "+truncate(res.Stdout+res.Stderr), err)
	}
	if res.ExitCode != 0 {
		// A process that flushed a completion and then failed is not a clean answer.
		return "", errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"the frontend request exited with status %d: %s", res.ExitCode, truncate(sanitizeLine(res.Stdout+" "+res.Stderr))))
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return "", errors.New(errors.ErrCodeInternal, "the frontend returned nothing (is python3 missing from the image, or the frontend not listening?): "+truncate(res.Stderr))
	}
	return res.Stdout, nil
}

// deleteDGDAndWait deletes the DGD this run created and waits, within the teardown
// timeout, for Dynamo to finish removing it and its pods, which frees the GPU. It
// returns the text recorded as evidence; a timeout is reported there, and the
// namespace cleanup that follows removes whatever is left.
//
// The delete is bound to this run twice: it happens only if the run attempted to
// create a DGD, and only for a DGD that carries the run's ownership label, by UID,
// so a Create whose response was lost still gets cleaned up while a replacement
// object (or a DGD in a namespace the run never owned) is left alone.
func deleteDGDAndWait(ctx *validators.Context, dynClient dynamic.Interface, r *dgdRun) string {
	if r.run.nsForeign || r.run.nsNeverCreated || !r.createAttempted {
		return "this run created no DynamoGraphDeployment: nothing to delete"
	}
	// SIGTERM is read from the signal source, not from the work context: a deadline
	// that passed earlier leaves that context's error as DeadlineExceeded for good,
	// so a later SIGTERM could not be seen there.
	terminated := validators.SignalContext(ctx.Ctx)
	res := dynClient.Resource(dgdGVR).Namespace(r.run.namespace)

	// The ownership read and the delete must be issued even when the check was
	// canceled, so they run under a bounded context that neither the deadline nor
	// SIGTERM cancels. Only the passive wait for the pods stops on SIGTERM.
	opCtx, cancelOp := context.WithTimeout(context.WithoutCancel(ctx.Ctx), dgdTeardownOpTimeout)
	defer cancelOp()
	existing, getErr := res.Get(opCtx, dgdName, metav1.GetOptions{})
	deleted := false // the DGD is gone or its delete was issued, so only the pods remain
	absent := false  // the DGD was already gone when the teardown began
	switch {
	case k8serrors.IsNotFound(getErr):
		deleted, absent = true, true
	case getErr != nil:
		return fmt.Sprintf("could not read the DynamoGraphDeployment (%v); the namespace cleanup removes it", getErr)
	case existing.GetLabels()[labels.GPUTestRun] != r.run.token:
		return "a DynamoGraphDeployment named " + dgdName + " exists without this run's ownership label: left untouched"
	}
	if !deleted {
		opts := metav1.DeleteOptions{}
		if uid := existing.GetUID(); uid != "" {
			opts.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		if err := res.Delete(opCtx, dgdName, opts); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Sprintf("could not delete the DynamoGraphDeployment (%v); the namespace cleanup removes it", err)
		}
	}
	// A canceled check (SIGTERM) has only the pod's termination grace left, and the
	// restored worker alone takes about a minute to end: the delete is issued, which
	// Dynamo finishes server-side, and the namespace cleanup follows at once.
	if terminated.Err() != nil {
		return "the check was canceled: the DynamoGraphDeployment delete was issued and Dynamo finishes it; not waiting for the pods"
	}

	// The graph's pods may still be ending even when the DGD object is already
	// gone, so the wait covers both orderings. The restored worker takes about a
	// minute to end, and the GPU is free only once the pods are gone. The wait
	// outlives a check that ran out of time but not one that was canceled
	// (SIGTERM), which has only its termination grace left.
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx.Ctx), dgdTeardownTimeout)
	defer cancel()
	stopOnCancel := context.AfterFunc(terminated, cancel)
	defer stopOnCancel()
	pollErr := wait.PollUntilContextCancel(c, dgdPollInterval, true, func(c context.Context) (bool, error) {
		if _, getErr := res.Get(c, dgdName, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
			return false, nil
		}
		pods, listErr := ctx.Clientset.CoreV1().Pods(r.run.namespace).List(c, metav1.ListOptions{LabelSelector: dgdNameLabel + "=" + dgdName})
		return listErr == nil && len(pods.Items) == 0, nil
	})
	if pollErr != nil {
		if terminated.Err() != nil {
			return "the check was canceled while the DynamoGraphDeployment was being removed; the namespace cleanup continues"
		}
		return fmt.Sprintf("the DynamoGraphDeployment was still being removed after %s; the namespace cleanup continues", dgdTeardownTimeout)
	}
	if absent {
		return "no DynamoGraphDeployment to delete; its pods are gone"
	}
	return "DynamoGraphDeployment deleted; Dynamo removed its pods, SnapshotJob and PodSnapshot"
}
