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

// performance is a validator container for all performance phase checks.
// Each check is selected via the first argument.
//
// Usage:
//
//	performance nccl-all-reduce-bw
//	performance nccl-all-reduce-bw-net
//	performance nccl-all-reduce-bw-nvls
package main

import (
	"github.com/NVIDIA/aicr/validators"
)

func main() {
	validators.Run(map[string]validators.CheckFunc{
		"nccl-all-reduce-bw":      checkNCCLAllReduceBW,
		"nccl-all-reduce-bw-net":  checkNCCLAllReduceBWNET,
		"nccl-all-reduce-bw-nvls": checkNCCLAllReduceBWNVLS,
		"inference-perf":          checkInferencePerf,
	})
}
