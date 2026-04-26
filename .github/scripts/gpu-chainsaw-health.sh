#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

if [[ $# -ne 1 ]]; then
  echo "::error::Usage: $0 <test_dir>"
  exit 2
fi
test_dir="$1"
if [[ ! -d "${test_dir}" ]]; then
  echo "::error::Test directory not found: ${test_dir}"
  exit 1
fi

CHAINSAW_TEST_TIMEOUT="${CHAINSAW_TEST_TIMEOUT:-30m}"
CHAINSAW_CLEANUP_TIMEOUT="${CHAINSAW_CLEANUP_TIMEOUT:-120s}"
CHAINSAW_DELETE_TIMEOUT="${CHAINSAW_DELETE_TIMEOUT:-120s}"

timeout "${CHAINSAW_TEST_TIMEOUT}" chainsaw test \
  --test-dir "${test_dir}" \
  --config tests/chainsaw/chainsaw-config.yaml \
  --cleanup-timeout "${CHAINSAW_CLEANUP_TIMEOUT}" \
  --delete-timeout "${CHAINSAW_DELETE_TIMEOUT}"
