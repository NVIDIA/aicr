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
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func checkCRETrainingGoodput(ctx *validators.Context) error {
	constraint, found := findPerformanceConstraint(ctx, checkNameCRETrainingGoodput)
	if !found {
		return validators.Skip(fmt.Sprintf("no %s constraint in recipe", checkNameCRETrainingGoodput))
	}
	if ctx.ValidationInput == nil {
		return validators.Skip("no validation input")
	}
	if ctx.ValidationInput.Criteria.Service != recipe.CriteriaServiceEKS ||
		ctx.ValidationInput.Criteria.Accelerator != recipe.CriteriaAcceleratorH100 {

		return validators.Skip(fmt.Sprintf(
			"%s currently supports only eks × h100, got %s × %s",
			checkNameCRETrainingGoodput,
			ctx.ValidationInput.Criteria.Service,
			ctx.ValidationInput.Criteria.Accelerator,
		))
	}

	threshold, err := parseThreshold(constraint.Value)
	if err != nil {
		return err
	}

	gpuConfig, err := determineGPUConfig(
		ctx,
		ctx.ValidationInput.Criteria.Service,
		ctx.ValidationInput.Criteria.Accelerator,
		ctx.NodeSelector,
	)
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to determine GPU configuration", err)
	}
	if gpuConfig.WorkerCount < 2 {
		return aicrErrors.New(aicrErrors.ErrCodeNotFound,
			fmt.Sprintf("recipe declares %s but the cluster has fewer than 2 GPU nodes", checkNameCRETrainingGoodput))
	}
	if ctx.DynamicClient == nil {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, "dynamic client is required to create a WorkloadRun")
	}

	runName, err := uniqueCREResourceName(creTrainingRunName)
	if err != nil {
		return err
	}
	runObj := buildCRETrainingWorkloadRun(ctx.Namespace, runName, gpuConfig, ctx.NodeSelector)
	if deleteErr := deleteCREWorkloadRun(ctx.Ctx, ctx.DynamicClient, ctx.Namespace, runName); deleteErr != nil {
		return deleteErr
	}
	keepWorkloadRun := false
	defer func() {
		if keepWorkloadRun {
			slog.Warn("leaving CRE training WorkloadRun for diagnosis", "workloadRun", runName)
			return
		}
		if deleteErr := deleteCREWorkloadRun(
			context.Background(),
			ctx.DynamicClient,
			ctx.Namespace,
			runName,
		); deleteErr != nil {
			slog.Warn("failed to delete CRE training WorkloadRun", "error", deleteErr)
		}
	}()

	if createErr := createUnstructured(ctx.Ctx, ctx.DynamicClient, workloadRunGVR, ctx.Namespace, runObj); createErr != nil {
		return createErr
	}
	run, err := waitForWorkloadRunTerminal(ctx.Ctx, ctx.DynamicClient, ctx.Namespace, runName)
	if err != nil {
		return err
	}
	if unstructuredConditionTrue(run, "Failed") {
		keepWorkloadRun = true
		summary := creTerminalConditionSummary(run)
		slog.Error("CRE training WorkloadRun failed", "workloadRun", runName, "summary", summary)
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("CRE training WorkloadRun failed: %s", summary))
	}

	status, err := getGoodputStatus(
		ctx.Ctx,
		ctx.DynamicClient,
		ctx.Namespace,
		runName,
		run.GetCreationTimestamp(),
	)
	if err != nil {
		return err
	}
	ratio, err := parseGoodputRatio(status)
	if err != nil {
		return err
	}

	actual := strconv.FormatFloat(ratio, 'f', 4, 64)
	fmt.Printf("CRE NeMo training goodput ratio: %s\n", actual)
	fmt.Printf("Constraint: %s → %v\n", constraint.Value, ratio >= threshold)
	for _, metric := range []string{
		"avgTFLOPSPerGPU",
		"avgStepTimeSec",
		"interruptionCount",
		"lostWorkTimeSec",
	} {
		if value, ok := status[metric]; ok {
			fmt.Printf("%s: %v\n", metric, value)
		}
	}

	if ratio < threshold {
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("goodput ratio %s does not satisfy constraint %q", actual, constraint.Value))
	}
	return nil
}

const (
	creTrainingRunName           = "aicr-cre-nemo"
	creLogProfileTrainingGoodput = "megatron-training"
	creTrainingImage             = "nvcr.io/nvidia/pytorch:25.08-py3"
	// creTrainingScript is CRE's training/nemotron5-8b exec workload (not the
	// public 56B WorkloadRun sample). 56B requires minGPUs=32; this check's
	// eks×h100 gate is 2 nodes × 8 GPUs. H100 catalog TP is 2.
	creTrainingScript = `#!/bin/bash
set -euo pipefail

WORKSPACE_DIR=${WORKSPACE_DIR:-/mnt/workspace}
MEGATRON_PATH=${MEGATRON_PATH:-${WORKSPACE_DIR}/megatron-lm}
CHECKPOINT_DIR=${CHECKPOINT_DIR:-${WORKSPACE_DIR}/checkpoints}
TENSORBOARD_DIR=${TENSORBOARD_DIR:-${WORKSPACE_DIR}/tensorboard}
mkdir -p "${CHECKPOINT_DIR}" "${TENSORBOARD_DIR}"

if ! ls "${MEGATRON_PATH}"/megatron/core/datasets/helpers_cpp*.so 1>/dev/null 2>&1; then
  echo "Building Megatron helpers_cpp..."
  cd "${MEGATRON_PATH}"
  pip install -e . --no-deps --no-build-isolation 2>&1 | tail -5
  cd "${WORKSPACE_DIR}"
fi

TOTAL_GPUS=$((PET_NNODES * PET_NPROC_PER_NODE))
TP=${TENSOR_PARALLELISM:-2}
PP=${PIPELINE_PARALLELISM:-1}
MBS=${MICRO_BATCH_SIZE:-1}
GBS=${GLOBAL_BATCH_SIZE:-$TOTAL_GPUS}

echo "TOTAL_GPUS=${TOTAL_GPUS} GBS=${GBS} MBS=${MBS} TP=${TP} PP=${PP}"

exec torchrun \
  --nnodes "${PET_NNODES}" \
  --nproc-per-node "${PET_NPROC_PER_NODE}" \
  "${MEGATRON_PATH}/pretrain_gpt.py" \
  --attention-backend flash \
  --distributed-timeout-minutes 230 \
  --use-mcore-models \
  --no-mmap-bin-files \
  --sequence-parallel \
  --untie-embeddings-and-output-weights \
  --disable-bias-linear \
  --init-method-std 0.014 \
  --position-embedding-type rope \
  --rotary-base 1000000 \
  --rotary-percent 1.0 \
  --squared-relu \
  --group-query-attention \
  --kv-channels 128 \
  --normalization RMSNorm \
  --attention-dropout 0.0 \
  --hidden-dropout 0.0 \
  --exit-duration-in-mins 30 \
  --train-iters 50 \
  --lr-decay-iters 1830030 \
  --lr 6e-4 \
  --min-lr 6e-6 \
  --weight-decay 0.1 \
  --clip-grad 1.0 \
  --lr-decay-style cosine \
  --lr-warmup-iters 5 \
  --eval-iters 1 \
  --eval-interval 50 \
  --log-interval 10 \
  --tokenizer-type NullTokenizer \
  --vocab-size 131072 \
  --mock-data \
  --num-workers 1 \
  --no-create-attention-mask-in-dataloader \
  --log-progress \
  --timing-log-option minmax \
  --log-params-norm \
  --log-num-zeros-in-grad \
  --log-throughput \
  --bf16 \
  --adam-beta1 0.9 \
  --adam-beta2 0.95 \
  --use-distributed-optimizer \
  --overlap-grad-reduce \
  --overlap-param-gather \
  --manual-gc \
  --log-straggler \
  --disable-straggler-on-startup \
  --straggler-minmax-count 16 \
  --check-weight-hash-across-dp-replicas-interval 20000 \
  --ckpt-fully-parallel-save \
  --ckpt-fully-parallel-load \
  --async-save \
  --ckpt-assume-constant-structure \
  --ckpt-format torch_dist \
  --num-layers 32 \
  --hidden-size 4096 \
  --ffn-hidden-size 21504 \
  --num-attention-heads 32 \
  --seq-length 8192 \
  --max-position-embeddings 8192 \
  --num-query-groups 8 \
  --tensor-model-parallel-size "${TP}" \
  --pipeline-model-parallel-size "${PP}" \
  --micro-batch-size "${MBS}" \
  --global-batch-size "${GBS}" \
  --save-interval "${SAVE_INTERVAL:-250}" \
  --save-retain-interval "${SAVE_RETAIN_INTERVAL:-1000}" \
  --tensorboard-dir "${TENSORBOARD_DIR}"
`
	creTrainingCloneScript = `set -euo pipefail
if [ ! -d "/mnt/workspace/megatron-lm/.git" ]; then
  git clone --depth 1 -b core_v0.15.2 \
    https://github.com/NVIDIA/Megatron-LM.git /mnt/workspace/megatron-lm
fi
`
	creWorkspaceVolume = "workspace"
)

var goodputMeasurementGVR = schema.GroupVersionResource{
	Group: creAPIGroup, Version: versionV1alpha1, Resource: "goodputmeasurements",
}

func buildCRETrainingWorkloadRun(namespace, name string, gpuConfig *gpuConfiguration, nodeSelector map[string]string) *unstructured.Unstructured {
	spec := map[string]any{
		"image":       creTrainingImage,
		"numNodes":    int64(gpuConfig.WorkerCount),
		"gpusPerNode": int64(gpuConfig.GPUCountPerNode),
		"framework": map[string]any{
			"exec": map[string]any{
				"command": []any{"/bin/bash", "/config/train.sh"},
			},
		},
		"config": map[string]any{
			"inline": map[string]any{"train.sh": creTrainingScript},
		},
		"initContainers": []any{
			map[string]any{
				keyName:   "megatron-clone",
				"image":   creTrainingImage,
				"command": []any{"/bin/bash", "-c"},
				"args":    []any{creTrainingCloneScript},
				"volumeMounts": []any{
					map[string]any{keyName: creWorkspaceVolume, keyMountPath: "/mnt/workspace"},
				},
			},
		},
		"volumes": []any{
			map[string]any{
				keyName:    creWorkspaceVolume,
				"emptyDir": map[string]any{"medium": "Memory"},
			},
		},
		"volumeMounts": []any{
			map[string]any{keyName: creWorkspaceVolume, keyMountPath: "/mnt/workspace"},
		},
		"env": []any{
			map[string]any{keyName: "PYTHONPATH", keyValue: "/mnt/workspace/megatron-lm"},
			map[string]any{keyName: "CUDA_DEVICE_MAX_CONNECTIONS", keyValue: "1"},
			map[string]any{keyName: "TENSOR_PARALLELISM", keyValue: "2"},
			map[string]any{keyName: "PYTORCH_CUDA_ALLOC_CONF", keyValue: "expandable_segments:True"},
			map[string]any{keyName: "NVTE_FWD_LAYERNORM_SM_MARGIN", keyValue: "16"},
			map[string]any{keyName: "NVTE_BWD_LAYERNORM_SM_MARGIN", keyValue: "16"},
			map[string]any{keyName: "NVTE_FUSED_ATTN", keyValue: "0"},
			map[string]any{keyName: "TORCHINDUCTOR_WORKER_START", keyValue: "fork"},
			map[string]any{keyName: "TORCH_NCCL_AVOID_RECORD_STREAMS", keyValue: "1"},
			map[string]any{keyName: "TORCH_NCCL_HIGH_PRIORITY", keyValue: "1"},
			map[string]any{keyName: "ENABLE_CHECKPOINT", keyValue: "false"},
			map[string]any{keyName: "TRAIN_ITERS", keyValue: "50"},
		},
		"goodputMeasurement": map[string]any{
			"logProfileRef":  creLogProfileTrainingGoodput,
			"sampleInterval": "10s",
		},
	}
	addCRETarget(spec, gpuConfig, nodeSelector)
	return newCREWorkloadRun(namespace, name, spec)
}

func creTerminalConditionSummary(obj *unstructured.Unstructured) string {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "no status.conditions"
	}
	var parts []string
	for _, raw := range conds {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := fmt.Sprint(m["type"])
		status := fmt.Sprint(m["status"])
		if status != "True" {
			continue
		}
		reason := strings.TrimSpace(fmt.Sprint(m["reason"]))
		msg := strings.TrimSpace(fmt.Sprint(m["message"]))
		switch {
		case reason != "" && msg != "" && reason != "<nil>" && msg != "<nil>":
			parts = append(parts, fmt.Sprintf("%s (%s): %s", typ, reason, msg))
		case reason != "" && reason != "<nil>":
			parts = append(parts, fmt.Sprintf("%s (%s)", typ, reason))
		case msg != "" && msg != "<nil>":
			parts = append(parts, fmt.Sprintf("%s: %s", typ, msg))
		default:
			parts = append(parts, typ)
		}
	}
	if len(parts) == 0 {
		return "Failed with empty condition reason/message"
	}
	return strings.Join(parts, "; ")
}

func parseGoodputRatio(status map[string]any) (float64, error) {
	raw, ok := status["result"]
	if !ok || raw == nil {
		return 0, aicrErrors.New(aicrErrors.ErrCodeNotFound, "GoodputMeasurement status.result is empty")
	}
	switch value := raw.(type) {
	case string:
		ratio, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "invalid goodput result", err)
		}
		return ratio, nil
	case float64:
		return value, nil
	default:
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported goodput result type %T", raw))
	}
}

func getGoodputStatus(ctx context.Context, client dynamic.Interface, namespace, runName string, createdAt metav1.Time) (map[string]any, error) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	list, err := client.Resource(goodputMeasurementGVR).Namespace(namespace).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list GoodputMeasurements", err)
	}
	for i := range list.Items {
		if !measurementBelongsToRun(&list.Items[i], runName, createdAt) {
			continue
		}
		status, found, nestedErr := unstructured.NestedMap(list.Items[i].Object, "status")
		if nestedErr != nil {
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read GoodputMeasurement status", nestedErr)
		}
		if found {
			return status, nil
		}
	}
	return nil, aicrErrors.New(aicrErrors.ErrCodeNotFound, "no GoodputMeasurement status for CRE training WorkloadRun")
}
