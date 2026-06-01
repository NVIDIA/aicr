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
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/validators"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	// envModelCacheSize sizes the PVC-backed model-weights cache. The validator
	// provisions a PVC, downloads the model into it once via a populate Job, and
	// mounts it read-only into the inference workers — so the N workers don't
	// each re-download the weights from Hugging Face (avoiding per-IP rate
	// limiting). The cache is ON BY DEFAULT (defaultModelCacheSize); set a
	// different Kubernetes quantity (e.g. "200Gi") to resize, or one of the
	// disable sentinels ("off", "0", "none", "disabled") to turn it off and have
	// workers download from HF directly. See docs/contributor/validator.md.
	envModelCacheSize = "AICR_INFERENCE_PERF_MODEL_CACHE_SIZE"

	// defaultModelCacheSize is the cache PVC size when the operator does not set
	// envModelCacheSize. ~6× a Qwen3-8B BF16 snapshot (~16 GB), with headroom for
	// larger models; the PVC is per-run and torn down on cleanup.
	defaultModelCacheSize = "100Gi"

	// defaultStorageClassAnnotation / defaultStorageClassAnnotationBeta mark a
	// StorageClass as the cluster default. Used by the cache pre-flight to fail
	// fast when the cache is enabled, no MODEL_CACHE_STORAGE_CLASS is set, and the
	// cluster has no default SC (which would otherwise leave the PVC Pending until
	// the populate-Job timeout). The beta key is still emitted by older clusters /
	// installers, so both are honored.
	defaultStorageClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	defaultStorageClassAnnotationBeta = "storageclass.beta.kubernetes.io/is-default-class"

	// envModelCacheStorageClass sets the StorageClass for the cache PVC.
	// Required on clusters without a default StorageClass — otherwise the PVC
	// stays Pending ("no persistent volumes available … and no storage class is
	// set"). Unset uses the cluster's default StorageClass.
	envModelCacheStorageClass = "AICR_INFERENCE_PERF_MODEL_CACHE_STORAGE_CLASS"

	// modelCachePVCName is the per-run PVC holding the Hugging Face cache.
	modelCachePVCName = "aicr-model-cache"

	// modelCacheMountPath is where the cache PVC is mounted (HF_HOME points here
	// in both the populate Job and the workers).
	modelCacheMountPath = "/model-cache"

	// modelCacheVolumeName is the pod volume name for the cache PVC.
	modelCacheVolumeName = "model-cache"

	// modelCachePopulateJobPrefix prefixes the one-time download Job (suffixed
	// with the run ID so concurrent runs don't collide).
	modelCachePopulateJobPrefix = "aicr-model-cache-populate"

	// cacheWorkerImage is the image used by the populate Job to download
	// weights. It must carry python3 + huggingface_hub; reuse the vLLM runtime
	// image so it is already node-cached. Keep in sync with the worker image in
	// testdata/inference/dynamo-deployment.yaml.
	//
	// NOTE: this is pinned to nvcr.io and is NOT run through
	// catalog.ResolveImage, so AICR_VALIDATOR_IMAGE_REGISTRY does not rewrite it
	// (unlike the AIPerf image) — a registry-override consistency gap. Propagating
	// imagePullSecrets only helps when the private creds are for nvcr.io itself.
	// This matches the Dynamo worker image (also pinned in the template,
	// drift-guarded by TestCacheWorkerImageMatchesTemplate). Routing this through
	// ResolveImage for registry-override parity is tracked in #1159. Note that
	// registry parity alone is not air-gap support: the populate Job's
	// snapshot_download still reaches huggingface.co for the weights.
	cacheWorkerImage = "nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.9.0"

	// Resource requests for the populate container. snapshot_download is
	// network/IO-bound, not compute-bound, so requests stay small; they exist so
	// the pod gets a Burstable QoS class and schedules under ResourceQuota /
	// LimitRange admission. Deliberately NO memory limit: the container streams
	// the full model (tens of GB) to the PVC, and on cgroup v2 the page cache
	// from those writes is billed to the container's memory cgroup — a hard
	// limit gets OOMKilled (exit 137) on any large model regardless of actual
	// RSS. Model sizes span 0.6B–70B+, so no fixed limit is safe; requests
	// handle scheduling, and the node's own memory pressure reclaims page cache.
	cacheJobCPURequest    = "100m"
	cacheJobMemoryRequest = "512Mi"

	// cacheWorkerFSGroup is the pod FSGroup applied to the populate Job so the
	// freshly provisioned volume is group-writable by the cacheWorkerImage's
	// non-root user. It must match that image's non-root UID/GID — if the vLLM
	// runtime image rebuilds under a different non-root UID, the workers' RO
	// mount would silently fail to load weights. Co-located with cacheWorkerImage
	// so the drift surface is in one place; the worker RO mount relies on the
	// same group ownership.
	cacheWorkerFSGroup = int64(1000)
)

// modelCacheEnabled reports whether the PVC cache is active for this run.
// config.modelCacheSize is "" only when the operator explicitly disabled it
// (see parseModelCacheSize); the unset case has already been defaulted on.
func modelCacheEnabled(config *inferenceWorkloadConfig) bool {
	return strings.TrimSpace(config.modelCacheSize) != ""
}

// parseModelCacheSize resolves the cache size from the raw env value, applying
// the on-by-default policy: unset → defaultModelCacheSize (enabled); an explicit
// disable sentinel ("off"/"0"/"none"/"disabled", case-insensitive) → disabled
// (empty size); anything else must be a valid Kubernetes quantity. A malformed
// value fails closed with ErrCodeInvalidRequest. Returns the effective size
// ("" when disabled) and whether the cache is enabled.
func parseModelCacheSize(raw string) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultModelCacheSize, true, nil
	}
	switch strings.ToLower(raw) {
	case "off", "0", "none", "disabled":
		return "", false, nil
	}
	qty, err := resource.ParseQuantity(raw)
	if err != nil {
		return "", false, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s=%q: must be a Kubernetes quantity (e.g. 100Gi) or a disable sentinel (off/0/none/disabled)", envModelCacheSize, raw), err)
	}
	// ParseQuantity accepts zero and negative values (e.g. "0Gi", "-1Gi"); those
	// would enable the cache and then fail PVC creation with an invalid storage
	// request. Fail closed here instead of deploying a doomed PVC.
	if qty.Sign() <= 0 {
		return "", false, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s=%q: cache size must be positive (use off/0/none/disabled to disable the cache)", envModelCacheSize, raw))
	}
	return raw, true, nil
}

// clusterHasDefaultStorageClass reports whether any StorageClass on the cluster
// is annotated as the default. Used to fail fast before provisioning a cache
// PVC with no StorageClass on a cluster that has no default.
func clusterHasDefaultStorageClass(ctx *validators.Context) (bool, error) {
	listCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()
	scs, err := ctx.Clientset.StorageV1().StorageClasses().List(listCtx, metav1.ListOptions{})
	if err != nil {
		return false, errors.Wrap(errors.ErrCodeInternal, "failed to list StorageClasses for cache pre-flight", err)
	}
	for i := range scs.Items {
		ann := scs.Items[i].Annotations
		if ann[defaultStorageClassAnnotation] == "true" || ann[defaultStorageClassAnnotationBeta] == "true" {
			return true, nil
		}
	}
	return false, nil
}

// ensureModelCache provisions the model-weights cache when enabled: an RWO PVC
// plus a one-time populate Job (pinned to the same node as the workers, so the
// WaitForFirstConsumer RWO volume binds there) that downloads the model into it.
// It blocks until the download completes so the cache is ready before the
// DynamoGraphDeployment workers start. A no-op when the cache is disabled.
func ensureModelCache(ctx *validators.Context, config *inferenceWorkloadConfig) error {
	if !modelCacheEnabled(config) {
		return nil
	}
	qty, err := resource.ParseQuantity(strings.TrimSpace(config.modelCacheSize))
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s=%q: must be a Kubernetes quantity (e.g. 100Gi)", envModelCacheSize, config.modelCacheSize), err)
	}

	// Fail fast when there is no StorageClass to bind the cache PVC to: with no
	// explicit MODEL_CACHE_STORAGE_CLASS, the PVC relies on a cluster default,
	// and without one it sits Pending until the populate Job times out (minutes).
	// Surface an actionable error immediately instead.
	if strings.TrimSpace(config.modelCacheStorageClass) == "" {
		hasDefault, derr := clusterHasDefaultStorageClass(ctx)
		if derr != nil {
			return derr
		}
		if !hasDefault {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("model-weights cache is enabled but the cluster has no default StorageClass and %s is unset; "+
					"set %s=<name> (e.g. gp2/gp3 on EKS, standard-rwo on GKE) or disable the cache with %s=off",
					envModelCacheStorageClass, envModelCacheStorageClass, envModelCacheSize))
		}
	}

	// Bound the create calls so a slow/wedged apiserver can't burn the check
	// budget before the (separately bounded) populate-Job wait even starts.
	pvc := buildModelCachePVC(config.namespace, qty, strings.TrimSpace(config.modelCacheStorageClass))
	pvcCtx, pvcCancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	_, err = ctx.Clientset.CoreV1().PersistentVolumeClaims(config.namespace).Create(pvcCtx, pvc, metav1.CreateOptions{})
	pvcCancel()
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create model-cache PVC", err)
	}

	jobName := fmt.Sprintf("%s-%s", modelCachePopulateJobPrefix, config.runID)
	// Propagate the validator pod's imagePullSecrets to the populate Job, same
	// as the AIPerf Job — so a private-mirror / air-gapped cluster can pull the
	// cacheWorkerImage. nil on a public-registry cluster (a no-op there).
	job := buildModelCachePopulateJob(jobName, config, getOwnPullSecrets(ctx))
	jobCtx, jobCancel := context.WithTimeout(ctx.Ctx, defaults.K8sJobCreationTimeout)
	_, err = ctx.Clientset.BatchV1().Jobs(config.namespace).Create(jobCtx, job, metav1.CreateOptions{})
	jobCancel()
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create model-cache populate Job", err)
	}
	slog.Info("Populating model cache (one-time download)",
		"pvc", modelCachePVCName, "model", config.model, "job", jobName)

	// The download can be slow for large models; reuse the (configurable)
	// workload-ready budget as the wait bound.
	readyTimeout, terr := durationFromEnv(envWorkloadReadyTimeout, defaults.InferenceWorkloadReadyTimeout)
	if terr != nil {
		return terr
	}
	if werr := pod.WaitForJobCompletion(ctx.Ctx, ctx.Clientset, config.namespace, jobName, readyTimeout); werr != nil {
		// WaitForJobCompletion returns coded errors (ErrCodeTimeout on the ready
		// deadline, ErrCodeUnavailable when the watch closes). Preserve that
		// classification instead of flattening every failure to ErrCodeInternal.
		return errors.PropagateOrWrap(werr, errors.ErrCodeInternal, "model-cache populate Job did not complete")
	}
	slog.Info("Model cache populated", "pvc", modelCachePVCName)
	return nil
}

// buildModelCachePVC builds the RWO cache PVC. A non-empty storageClass is set
// explicitly — required on clusters with no default StorageClass, otherwise the
// claim stays Pending ("no storage class is set"). Empty uses the default.
func buildModelCachePVC(namespace string, qty resource.Quantity, storageClass string) *v1.PersistentVolumeClaim {
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: modelCachePVCName, Namespace: namespace},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources:   v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: qty}},
		},
	}
	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}
	return pvc
}

// buildModelCachePopulateJob builds the one-time Job that downloads the model
// into the cache PVC. It is pinned to the workers' node (gpuNodeSelector) so the
// RWO PVC binds there, mounts the PVC read-write at HF_HOME, and uses the
// optional HF token. fsGroup makes the freshly provisioned volume group-writable
// for the image's non-root user.
func buildModelCachePopulateJob(name string, config *inferenceWorkloadConfig, pullSecrets []v1.LocalObjectReference) *batchv1.Job {
	// BackoffLimit 0 (single attempt): snapshot_download does not resume across
	// pods, so each Job retry restarts the multi-GB download from scratch and
	// burns the parent context deadline (a 100Gi-class download retried 2× could
	// consume ~3× the budget). One attempt fails fast; huggingface_hub already
	// retries transient HTTP errors in-process, and the parent deadline bounds
	// the single attempt. Early/cheap failures (image pull, auth) surface
	// immediately rather than re-running the whole fetch.
	backoff := int32(0)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.namespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "aicr-model-cache-populate"},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					RestartPolicy:    v1.RestartPolicyNever,
					NodeSelector:     config.gpuNodeSelector,
					Tolerations:      config.gpuTolerations,
					ImagePullSecrets: pullSecrets,
					SecurityContext:  &v1.PodSecurityContext{FSGroup: ptr.To(cacheWorkerFSGroup)},
					Containers: []v1.Container{{
						Name:  "populate",
						Image: cacheWorkerImage,
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse(cacheJobCPURequest),
								v1.ResourceMemory: resource.MustParse(cacheJobMemoryRequest),
							},
							// No Limits: see the cacheJobMemoryRequest comment — a
							// memory limit OOMKills large-model downloads via page
							// cache on cgroup v2.
						},
						// snapshot_download fetches the full repo into HF_HOME; HF_TOKEN
						// (if present) raises rate limits / unlocks gated models.
						Command: []string{"python3", "-c",
							"import os; from huggingface_hub import snapshot_download; snapshot_download(os.environ['AICR_MODEL'])"},
						Env: []v1.EnvVar{
							{Name: "AICR_MODEL", Value: config.model},
							{Name: "HF_HOME", Value: modelCacheMountPath},
							{Name: "HF_TOKEN", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{
								LocalObjectReference: v1.LocalObjectReference{Name: hfTokenSecretName},
								Key:                  hfTokenSecretKey,
								Optional:             ptr.To(true),
							}}},
						},
						VolumeMounts: []v1.VolumeMount{{Name: modelCacheVolumeName, MountPath: modelCacheMountPath}},
					}},
					Volumes: []v1.Volume{{
						Name: modelCacheVolumeName,
						VolumeSource: v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: modelCachePVCName,
						}},
					}},
				},
			},
		},
	}
}

// injectModelCacheMounts adds the read-only cache PVC volume, its mount, and the
// HF_HOME / HF_HUB_OFFLINE env to a service's extraPodSpec so the worker loads
// weights from the pre-populated PVC and never reaches out to Hugging Face.
// Offline mode fails closed if the cache is incomplete rather than silently
// falling back to a (rate-limited) download. Operates on the unstructured map
// in place, merging with any env the template already declares.
//
// Applied per-service by applyInferenceWorkerScheduling, so the Frontend pod
// also receives the RO mount even though it does not need weights. This is
// intentional and harmless: the RWO PVC is co-located on one node (which the
// validator already enforces) and snapshot_download fetches the full repo
// (tokenizer included), so an offline frontend still resolves.
func injectModelCacheMounts(extraPodSpec map[string]interface{}) {
	vols, _ := extraPodSpec["volumes"].([]interface{})
	vols = append(vols, map[string]interface{}{
		keyName: modelCacheVolumeName,
		"persistentVolumeClaim": map[string]interface{}{
			"claimName": modelCachePVCName,
			"readOnly":  true,
		},
	})
	extraPodSpec["volumes"] = vols

	mc, _ := extraPodSpec["mainContainer"].(map[string]interface{})
	if mc == nil {
		mc = map[string]interface{}{}
	}
	mounts, _ := mc["volumeMounts"].([]interface{})
	mounts = append(mounts, map[string]interface{}{
		keyName:     modelCacheVolumeName,
		"mountPath": modelCacheMountPath,
		"readOnly":  true,
	})
	mc["volumeMounts"] = mounts

	env, _ := mc["env"].([]interface{})
	env = append(env,
		map[string]interface{}{keyName: "HF_HOME", "value": modelCacheMountPath},
		map[string]interface{}{keyName: "HF_HUB_OFFLINE", "value": "1"},
	)
	mc["env"] = env
	extraPodSpec["mainContainer"] = mc
}
