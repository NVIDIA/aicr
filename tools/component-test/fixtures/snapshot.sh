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

# Test-only storage for the snapshot component test. The chart's checkpoint claim
# is ReadWriteMany, which Kind's local-path class cannot bind, so the operator pod
# would never start. Production storage requirements are unchanged.

snapshot_claim="snapshot-pvc"

kubectl create namespace "${HELM_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${snapshot_claim}
  namespace: ${HELM_NAMESPACE}
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
EOF

BUNDLE_EXTRA_ARGS="${BUNDLE_EXTRA_ARGS:-} --set snapshot:storage.pvc.create=false --set snapshot:storage.pvc.name=${snapshot_claim}"
log_info "snapshot fixture: using a test-only ReadWriteOnce claim ${HELM_NAMESPACE}/${snapshot_claim}"
