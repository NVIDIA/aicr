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
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ncclRuntimeImageEnv overrides the NCCL launcher/worker workload image baked
// into the per-platform TrainingRuntime templates (CUDA/NCCL/MPI/SSH/transport
// runtime — NOT the validator's own snapshot-agent image, which is controlled
// separately by `aicr validate --image` / AICR_VALIDATOR_IMAGE_*). This is the
// validator-pod (reading) end; the orchestrator (forwarding) end defines the
// same literal as ncclRuntimeImageEnv in pkg/validator/v1/job_plan_internal.go
// — keep the two in sync. See NVIDIA/aicr#1751.
const ncclRuntimeImageEnv = "AICR_NCCL_RUNTIME_IMAGE"

// ncclWorkloadContainerNames are the container names, within either the
// "launcher" or "node" replicatedJob of a baked-in NCCL runtime template, that
// carry the NCCL workload image. All three currently resolve to the same
// image in every template (verified by TestNCCLRuntimeTemplatesShareOneImage);
// resolveNCCLRuntimeImage renders exactly one image into all of them so a run
// can never mix images across containers.
//
// Deliberately excludes "tcpxo-daemon" (the GKE TCPXO transport sidecar,
// testdata/h100/gke/runtime.yaml): that container ships the GPUDirect-TCPXO
// FastRak daemon, an unrelated artifact with its own release cadence, and is
// never part of the CUDA/NCCL/MPI workload contract this override governs.
var ncclWorkloadContainerNames = map[string]bool{
	"fix-ssh-perms": true,
	nodeJobName:     true, // "node" — shared name for both the launcher's inner container and the worker replicatedJob's container.
}

// resolveNCCLRuntimeImage reads the optional AICR_NCCL_RUNTIME_IMAGE override.
// Returns "" when unset or blank — callers keep the compiled-in per-platform
// default. A non-empty value that is not a syntactically valid image
// reference fails closed with ErrCodeInvalidRequest: success criteria for
// NVIDIA/aicr#1751 requires a malformed override to abort the check rather
// than silently fall back to the default, which would defeat the purpose of
// an explicit qualification run.
//
// Only syntax is validated here — the same shape check `docker pull` or any
// OCI-compliant tool would perform. Whether the image actually exists, is
// pullable, and provides the expected mpirun/all_reduce_perf_mpi paths is a
// runtime property this function cannot see; that contract is documented on
// the env var and surfaced operationally by the worker pods failing to start
// or the launcher's mpirun/ssh steps failing, not by a pre-flight probe here.
func resolveNCCLRuntimeImage() (string, error) {
	v := strings.TrimSpace(os.Getenv(ncclRuntimeImageEnv))
	if v == "" {
		return "", nil
	}
	if _, err := reference.ParseNormalizedNamed(v); err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s=%q: not a well-formed image reference", ncclRuntimeImageEnv, v), err)
	}
	return v, nil
}

// applyNCCLRuntimeImageOverride renders a single resolved image into every
// ncclWorkloadContainerNames container found under any replicatedJob of the
// TrainingRuntime object, so the launcher's fix-ssh-perms init container, the
// launcher's own "node" container, and the worker "node" container can never
// diverge onto a mixed image set (NVIDIA/aicr#1751 success criteria: "All
// containers belonging to the selected NCCL runtime use the same resolved
// workload image"). No-op when image is "" (override unset). Never touches
// initContainers/containers outside ncclWorkloadContainerNames — in
// particular the GKE tcpxo-daemon sidecar, which is not part of the NCCL
// workload contract this override governs.
//
// Deliberately unconditional on customRuntime: callers only invoke this for
// the baked-in template path (customRuntime == ""); a recipe-supplied
// nccl-benchmark-runtime owns its own image end to end, mirroring every other
// service-specific override in this package (GKE NIC discovery, EKS EFA
// wiring, AKS RDMA wiring — see applyNCCLResources).
func applyNCCLRuntimeImageOverride(obj *unstructured.Unstructured, image string) error {
	if image == "" {
		return nil
	}

	replicatedJobs, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil || !found {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, "replicatedJobs not found in TrainingRuntime")
	}

	touched := 0
	for i, jobRaw := range replicatedJobs {
		jobMap, ok := jobRaw.(map[string]interface{})
		if !ok {
			continue
		}
		podSpec, found := nestedMap(jobMap, "template", "spec", "template", "spec")
		if !found {
			continue
		}
		n, err := setWorkloadImages(podSpec, "initContainers", image)
		if err != nil {
			return err
		}
		touched += n
		n, err = setWorkloadImages(podSpec, "containers", image)
		if err != nil {
			return err
		}
		touched += n
		replicatedJobs[i] = jobMap
	}

	if touched == 0 {
		// A baked-in template that matched none of ncclWorkloadContainerNames
		// is a template/constant drift bug (e.g. a container renamed without
		// updating this file), not an operator input error — fail closed with
		// ErrCodeInternal rather than silently applying no override.
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("%s set but no %v containers found in TrainingRuntime — template/override drift", ncclRuntimeImageEnv, containerNameList()))
	}

	slog.Info("Applied AICR_NCCL_RUNTIME_IMAGE override", "image", image, "containers", touched)

	return unstructured.SetNestedSlice(obj.Object, replicatedJobs, "spec", "template", "spec", "replicatedJobs")
}

// setWorkloadImages rewrites the "image" field of every container in
// podSpec[listKey] whose name is in ncclWorkloadContainerNames. Returns the
// number of containers updated.
func setWorkloadImages(podSpec map[string]interface{}, listKey, image string) (int, error) {
	raw, ok := podSpec[listKey]
	if !ok {
		return 0, nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("TrainingRuntime %s is not a list", listKey))
	}

	count := 0
	for i, cRaw := range list {
		cMap, ok := cRaw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(cMap, "name")
		if !ncclWorkloadContainerNames[name] {
			continue
		}
		cMap["image"] = image
		list[i] = cMap
		count++
	}
	podSpec[listKey] = list
	return count, nil
}

// containerNameList returns ncclWorkloadContainerNames' keys, sorted, for
// error messages — map iteration order is randomized, and an unsorted list
// would make the drift error's container names flap across runs.
func containerNameList() []string {
	names := make([]string, 0, len(ncclWorkloadContainerNames))
	for n := range ncclWorkloadContainerNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
