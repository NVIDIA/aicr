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

export RESOURCE_GROUP="h100"
export CLUSTER_NAME="h100"
export LOCATION="westeurope"
export K8S_VERSION="1.34"

az group create --name $RESOURCE_GROUP --location $LOCATION
az aks create --resource-group $RESOURCE_GROUP --name $CLUSTER_NAME \
  --kubernetes-version $K8S_VERSION \
  --enable-oidc-issuer \
  --enable-workload-identity \
  --enable-managed-identity \
  --generate-ssh-keys

# GPU node pool: --gpu-driver none lets GPU Operator manage drivers
# DRA (DynamicResourceAllocation) is GA in K8s 1.34 (resource.k8s.io/v1)
# No special AKS feature flag required — the API server feature gate is on by default.
# To use DRA with NVIDIA GPUs, deploy the nvidia-dra-driver-gpu component and
# disable the device plugin in GPU Operator (devicePlugin.enabled=false) to
# avoid dual GPU advertisement.
az aks nodepool add -g $RESOURCE_GROUP -n h100pool2 --cluster-name $CLUSTER_NAME \
  -s Standard_NC40ads_H100_v5 -c 1 --gpu-driver none

az aks nodepool delete -g $RESOURCE_GROUP -n h100pool --cluster-name $CLUSTER_NAME --no-wait
