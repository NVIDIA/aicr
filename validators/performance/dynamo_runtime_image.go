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
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

// Dynamo runtime image names under defaults.DynamoImageRepository.
const (
	dynamoVLLMRuntimeImageName = "vllm-runtime"
	dynamoFrontendImageName    = "dynamo-frontend"
)

// dynamoRuntimeImage returns the reference for Dynamo runtime image name at
// defaults.DynamoRuntimeImageTag, redirected by AICR_VALIDATOR_IMAGE_REGISTRY
// and never retagged by AICR_VALIDATOR_IMAGE_TAG.
func dynamoRuntimeImage(name string) string {
	// The tag must match the deployed dynamo-platform operator, so only the
	// registry override applies.
	return catalog.ApplyRegistryOverride(
		defaults.DynamoImageRepository + "/" + name + ":" + defaults.DynamoRuntimeImageTag)
}
