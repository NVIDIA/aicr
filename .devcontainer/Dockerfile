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

FROM mcr.microsoft.com/devcontainers/base:ubuntu-24.04

# Bootstrap packages follow Ubuntu security updates; AICR tools are pinned by tools-setup.
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bc \
        build-essential \
        ca-certificates \
        curl \
	gawk \
        git \
        jq \
        libnss3-tools \
        python3 \
        python3-pip \
        python3-venv \
        unzip \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*
