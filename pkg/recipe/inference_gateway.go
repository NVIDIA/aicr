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

package recipe

// InferenceGatewayOverrideKey is the ComponentRef.Overrides key under which a
// component declares itself as the recipe's inference gateway for the
// validators (the inference-gateway conformance check and the performance
// validator's gateway-epp mode). The value is
// a mapping of gatewayClass (required), gateway (the Gateway object's name,
// default inference-gateway) and dataPlane (service-endpoints or
// gateway-address); pkg/validator/v1 parses it. Like every override it is
// merged into the component's values, so the chart sees it as a top-level key.
const InferenceGatewayOverrideKey = "inferenceGateway"
