# Installation Guide

This guide describes how to install Eidos (Eidos) CLI tool (`eidos`) on Linux, macOS, or Windows.

**What is Eidos**: Eidos generates validated configurations for GPU-accelerated Kubernetes deployments. See [README](../../README.md) for project overview.

## Prerequisites

- **Operating System**: Linux, macOS, or Windows (via WSL)
- **Kubernetes Cluster** (optional): For agent deployment or bundle generation testing
- **GPU Hardware** (optional): NVIDIA GPUs for full system snapshot capabilities
- **kubectl** (optional): For Kubernetes agent deployment

## Install eidos CLI

### Option 1: Automated Installation (Recommended)

Install the latest version using the installation script:

> Note: Temporally, while the repo is private, make sure to include your GitHub token first:

```shell
curl -sfL -H "Authorization: token $GITHUB_TOKEN" \
  https://raw.githubusercontent.com/NVIDIA/eidos/main/install | bash -s --
```

You can generate a personal access token at [GitHub Settings > Developer settings > Personal access tokens](https://github.com/settings/tokens). The token needs `repo` scope for private repository access.

This script:
- Detects your OS and architecture automatically
- Downloads the appropriate binary from GitHub releases
- Installs to `/usr/local/bin/eidos` (requires sudo)
- Verifies the installation
- Uses `GITHUB_TOKEN` environment variable for authenticated API calls (avoids rate limits)

> **Supply Chain Security**: Eidos includes SLSA Build Level 3 compliance with signed SBOMs and verifiable attestations. See [SECURITY](../SECURITY.md#supply-chain-security) for verification instructions.

### Option 2: Manual Installation

1. **Download the latest release**

Visit the [releases page](https://github.com/nvidia/eidos/releases/latest) and download the appropriate binary for your platform:

- **macOS ARM64** (M1/M2/M3): `eidos_v0.22.0_darwin_arm64`
- **macOS Intel**: `eidos_v0.22.0_darwin_amd64`
- **Linux ARM64**: `eidos_v0.22.0_linux_arm64`
- **Linux x86_64**: `eidos_v0.22.0_linux_amd64`

1. **Extract and install**

```shell
# Example for Linux x86_64
tar -xzf eidos_linux_amd64.tar.gz
sudo mv eidos /usr/local/bin/
sudo chmod +x /usr/local/bin/eidos
```

3. **Verify installation**

```shell
eidos --version
```

### Option 3: Build from Source

**Requirements:**
- Go 1.25 or higher

```shell
go install github.com/NVIDIA/eidos/cmd/eidos@latest
```

## Verify Installation

Check that eidos is correctly installed:

```shell
# Check version
eidos --version

# View available commands
eidos --help

# Test snapshot (requires GPU)
eidos snapshot --format json | jq '.measurements | length'
```

Expected output shows version information and available commands.

## Post-Installation

### Shell Completion (Optional)

Enable shell auto-completion for command and flag names:

**Bash:**
```shell
# Add to ~/.bashrc
source <(eidos completion bash)
```

**Zsh:**
```shell
# Add to ~/.zshrc
source <(eidos completion zsh)
```

**Fish:**
```shell
# Add to ~/.config/fish/config.fish
eidos completion fish | source
```

## Container Images

Eidos is also available as container images for integration into automated pipelines:

### CLI Image
```shell
docker pull ghcr.io/nvidia/eidos:latest
docker run ghcr.io/nvidia/eidos:latest --version
```

### API Server Image (Self-hosting)
```shell
docker pull ghcr.io/nvidia/eidosd:latest
docker run -p 8080:8080 ghcr.io/nvidia/eidosd:latest
```

## Next Steps

See [CLI Reference](cli-reference.md) for command usage

## Troubleshooting

### Command Not Found

If `eidos` is not found after installation:

```shell
# Check if binary is in PATH
echo $PATH | grep -q /usr/local/bin && echo "OK" || echo "Add /usr/local/bin to PATH"

# Add to PATH (bash)
echo 'export PATH="/usr/local/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

### Permission Denied

```shell
# Make binary executable
sudo chmod +x /usr/local/bin/eidos
```

### GPU Detection Issues

Snapshot GPU measurements require `nvidia-smi` in PATH:

```shell
# Verify NVIDIA drivers
nvidia-smi

# If missing, install NVIDIA drivers for your platform
```

## Uninstall

```shell
# Remove binary
sudo rm /usr/local/bin/eidos

# Remove shell completion (if configured)
# Remove the source line from your shell RC file
```

## Getting Help

- **Documentation**: [User Documentation](README.md)
- **Issues**: [GitHub Issues](https://github.com/NVIDIA/eidos/issues)
- **API Server**: See [Kubernetes Deployment](../integrator/kubernetes-deployment.md)
