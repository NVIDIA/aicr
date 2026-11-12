FROM mcr.microsoft.com/devcontainers/base:ubuntu-24.04

# Bootstrap packages follow Ubuntu security updates; AICR tools are pinned by tools-setup.
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bc \
        build-essential \
        ca-certificates \
        curl \
        git \
        jq \
        libnss3-tools \
        python3 \
        python3-venv \
        unzip \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*