# Installation Guide

This guide describes how to install the AI Cluster Runtime (AICR) CLI tool (`aicr`) on Linux, macOS, or Windows.

**What is AICR**: AICR generates validated configurations for GPU-accelerated Kubernetes deployments. See [README](https://github.com/NVIDIA/aicr#readme) for project overview.

## Prerequisites

- **Operating System**: Linux, macOS, or Windows (via WSL)
- **Kubernetes Cluster** (optional): For agent deployment or bundle generation testing
- **GPU Hardware** (optional): NVIDIA GPUs for full system snapshot capabilities
- **kubectl** (optional): For Kubernetes agent deployment

## Install aicr CLI

### Option 1: Homebrew (macOS/Linux)

```shell
brew tap NVIDIA/aicr
brew install aicr
```

### Option 2: Automated Installation

Install the latest version using the installation script:

```shell
curl -sfL https://raw.githubusercontent.com/NVIDIA/aicr/main/install | bash -s --
```

To install to a custom directory instead of the default `/usr/local/bin`:

```shell
curl -sfL https://raw.githubusercontent.com/NVIDIA/aicr/main/install | bash -s -- -d ~/bin
```

Optional: if you hit GitHub API rate limits, set `GITHUB_TOKEN` before running the install command. No special repository scope is required for public releases.

This script:
- Detects your OS and architecture automatically
- Downloads the appropriate binary from GitHub releases
- Installs to `/usr/local/bin/aicr` by default (use `-d <dir>` for a custom location)
- Installs shell completions for bash, zsh, and fish (set `AICR_NO_COMPLETIONS=1` to skip)
- Verifies the installation
- Uses `GITHUB_TOKEN` environment variable for authenticated API calls (avoids rate limits)

> **Supply Chain Security**: AICR includes SLSA Build Level 3 compliance with signed SBOMs and verifiable attestations. See [SECURITY](https://github.com/NVIDIA/aicr/blob/main/SECURITY.md#supply-chain-security) for verification instructions.

### Option 3: Manual Installation

1. **Download the latest release**

Visit the [releases page](https://github.com/nvidia/aicr/releases/latest) and download the appropriate binary for your platform:

- **macOS ARM64** (M1/M2/M3): `aicr_<version>_darwin_arm64.tar.gz`
- **macOS Intel**: `aicr_<version>_darwin_amd64.tar.gz`
- **Linux ARM64**: `aicr_<version>_linux_arm64.tar.gz`
- **Linux x86_64**: `aicr_<version>_linux_amd64.tar.gz`

2. **Extract and install**

```shell
# Example for Linux x86_64
tar -xzf aicr_linux_amd64.tar.gz
sudo mv aicr /usr/local/bin/
sudo chmod +x /usr/local/bin/aicr
```

3. **Verify installation**

```shell
aicr --version
```

### Option 4: Build from Source

**Requirements:**
- Go 1.26 or higher

```shell
go install github.com/NVIDIA/aicr/cmd/aicr@latest
```

## Verify Installation

Check that aicr is correctly installed:

```shell
# Check version
aicr --version

# View available commands
aicr --help

# Test snapshot (requires GPU)
aicr snapshot --format json | jq '.measurements | length'
```

Expected output shows version information and available commands.

## Post-Installation

### Shell Completion

Tab completion for commands and flags is installed automatically by both the Homebrew formula and the install script. No manual setup is required.

**Opt out** (install script only): set `AICR_NO_COMPLETIONS=1` before running the script:

```shell
AICR_NO_COMPLETIONS=1 curl -sfL https://raw.githubusercontent.com/NVIDIA/aicr/main/install | bash -s --
```

**Manual setup** (build from source or `go install`):

Bash:
```shell
aicr completion bash > "${BASH_COMPLETION_USER_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion}/completions/aicr"
```

Zsh:
```shell
aicr completion zsh > "${XDG_DATA_HOME:-$HOME/.local/share}/zsh/site-functions/_aicr"
```

Fish:
```shell
aicr completion fish > ~/.config/fish/completions/aicr.fish
```

Alternatively, source completions dynamically in your shell RC file (evaluates on every shell start):

```shell
# Bash (~/.bashrc)
source <(aicr completion bash)

# Zsh (~/.zshrc)
source <(aicr completion zsh)
```

## Container Images

AICR is also available as container images for integration into automated pipelines:

### CLI Image

```shell
docker pull ghcr.io/nvidia/aicr:latest
docker run ghcr.io/nvidia/aicr:latest --version
```

### API Server Image (Self-hosting)

```shell
docker pull ghcr.io/nvidia/aicrd:latest
docker run -p 8080:8080 ghcr.io/nvidia/aicrd:latest
```

## Next Steps

See [CLI Reference](cli-reference.md) for command usage

## Troubleshooting

### Command Not Found

If `aicr` is not found after installation:

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
sudo chmod +x /usr/local/bin/aicr
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
sudo rm /usr/local/bin/aicr

# Remove shell completions (remove whichever exist)
sudo rm -f /usr/share/bash-completion/completions/aicr
sudo rm -f /usr/local/share/zsh/site-functions/_aicr
sudo rm -f /opt/homebrew/share/zsh/site-functions/_aicr
sudo rm -f /opt/homebrew/etc/bash_completion.d/aicr
sudo rm -f /usr/local/etc/bash_completion.d/aicr
rm -f "${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions/aicr"
rm -f "${XDG_DATA_HOME:-$HOME/.local/share}/zsh/site-functions/_aicr"
rm -f "${XDG_CONFIG_HOME:-$HOME/.config}/fish/completions/aicr.fish"
```

## Getting Help

- **Documentation**: [User Documentation](index.md)
- **Issues**: [GitHub Issues](https://github.com/NVIDIA/aicr/issues)
- **API Server**: See [Kubernetes Deployment](../integrator/kubernetes-deployment.md)
