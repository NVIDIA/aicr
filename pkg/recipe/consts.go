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

// Shared map keys used in structured error contexts and hydrated recipe output.
// Declared here so the same key literal is not repeated across files.
const (
	keyRequested = "requested"
	keyAllowed   = "allowed"
	keyValue     = "value"
	keyError     = "error"
	keyStage     = "stage"
)

// stageInitialization is the "stage" value used in error contexts emitted
// during recipe builder/metadata-store initialization.
const stageInitialization = "initialization"
