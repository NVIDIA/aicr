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
	"sort"
	"strconv"
)

// EKS H100 values align the CRE WorkloadRun with AICR's proven EFA runtime.
// CRE's compiled WorkloadRun overrides do not swap the image and mpirun path,
// so AICR supplies the complete profile until CRE carries it itself.
const (
	creEFANCCLImage = "public.ecr.aws/hpc-cloud/nccl-tests:cuda12.8.1-efa1.43.2-ofiv1.16.3-ncclv2.27.7-1-testsv2.16.9"
	creEFAMpirun    = "/opt/amazon/openmpi/bin/mpirun"
	creEFANCCLBin   = "/opt/nccl-tests/build/all_reduce_perf"

	creEFAResource  = "vpc.amazonaws.com/efa"
	creEFACountH100 = "32"
)

// creFabricProfile is the WorkloadRun image, MPI, environment, and
// extended-resource configuration for EKS H100.
type creFabricProfile struct {
	image       string
	mpirunPath  string
	binary      string
	env         map[string]string
	mpiArgs     []string
	extraLimits map[string]string
}

func creEKSH100EFAProfile() creFabricProfile {
	env := map[string]string{
		"NCCL_DEBUG":             "INFO",
		"PATH":                   "$PATH:/opt/amazon/efa/bin:/usr/bin",
		"FI_EFA_USE_DEVICE_RDMA": "1",
		"FI_PROVIDER":            "efa",
		"NCCL_SOCKET_IFNAME":     "eth0",
		// Last -x wins over CRE's compiled AWS mpiArgs NCCL_NET_PLUGIN=none.
		"NCCL_NET_PLUGIN": "ofi",
	}
	return creFabricProfile{
		image:      creEFANCCLImage,
		mpirunPath: creEFAMpirun,
		binary:     creEFANCCLBin,
		env:        env,
		mpiArgs:    mpiArgsFromEnv(env),
		extraLimits: map[string]string{
			creEFAResource: creEFACountH100,
		},
	}
}

func mpiArgsFromEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, len(env)*2)
	for _, k := range keys {
		args = append(args, "-x", k+"="+env[k])
	}
	return args
}

func creResourceRequirements(gpuPerNode int, extra map[string]string) map[string]any {
	limits := map[string]any{
		"nvidia.com/gpu": strconv.Itoa(gpuPerNode),
	}
	for k, v := range extra {
		limits[k] = v
	}
	return map[string]any{
		"limits":   limits,
		"requests": limits,
	}
}
