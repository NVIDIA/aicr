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

package defaults

const (
	// ProbeImage is the multi-arch (amd64+arm64) toolbox used by validator
	// probe Pods. busybox provides /bin/sh, grep, ls, sleep in ~2 MB.
	ProbeImage = "busybox:1.37"
)

// DynamoImageRepository and DynamoRuntimeImageTag locate the Dynamo runtime
// images the inference-perf validator runs. The validator tags them with the
// recipe's dynamo-platform version and falls back to DynamoRuntimeImageTag
// when the recipe's ref carries none. DynamoRuntimeImageTag must equal the
// dynamo-platform chart version in recipes/registry.yaml, as enforced by
// make check-dynamo-runtime-image.
const (
	DynamoImageRepository = "nvcr.io/nvidia/ai-dynamo"
	DynamoRuntimeImageTag = "1.4.2"
)
