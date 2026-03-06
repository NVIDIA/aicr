#!/bin/sh
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

# External validator example: Cluster DNS check
#
# Protocol: exit 0 = pass, exit 1 = fail
# The validator framework captures stdout as evidence and reads
# /dev/termination-log (or last 10 lines of stdout) for failure reason.

echo "=== External Validator: Cluster DNS Check ==="
echo "Checking if kubernetes.default.svc.cluster.local resolves..."

if nslookup kubernetes.default.svc.cluster.local > /dev/null 2>&1; then
    resolved=$(nslookup kubernetes.default.svc.cluster.local 2>/dev/null | grep -A1 "Name:" | tail -1)
    echo "PASS: DNS resolution works"
    echo "Resolved: ${resolved}"
    exit 0
else
    echo "FAIL: DNS resolution failed for kubernetes.default.svc.cluster.local"
    exit 1
fi
