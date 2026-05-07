#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

RECIPE_ARGS=(
  --service kind
  --accelerator "${AICR_ACCELERATOR}"
  --os ubuntu
  --intent "${AICR_INTENT}"
)
if [[ -n "${AICR_PLATFORM:-}" ]]; then
  RECIPE_ARGS+=(--platform "${AICR_PLATFORM}")
fi

./aicr recipe "${RECIPE_ARGS[@]}" --output recipe.yaml
echo "Recipe written to recipe.yaml"
