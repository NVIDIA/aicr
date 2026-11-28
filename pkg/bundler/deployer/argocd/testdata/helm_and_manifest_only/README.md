# Argo CD Deployment Bundle

Bundler Version: v0.0.0-golden
Recipe Version: v1.0.0

## Overview

This bundle contains Argo CD Application manifests for deploying AI Cluster Runtime (AICR) components using the App of Apps pattern.

## Components

The following components are included in deployment order:

| Component | Type | Version | Sync Wave | Namespace |
|-----------|------|---------|-----------|-----------|
| cert-manager | Helm | 1.20.2 | 1 | cert-manager |
| nodewright-customizations | Local chart | - | 1 | skyhook |

## Prerequisites

- Kubernetes cluster with Argo CD installed
- Argo CD CLI (`argocd`) configured
- Git repository for storing these manifests
- kubectl configured with cluster access

## Deployment Steps

### 1. Prepare Git Repository

Push this bundle to your GitOps repository:

```bash
cd <bundle-directory>
git init
git add .
git commit -m "Add AICR manifests"
git remote add origin YOUR_REPO_URL
git push -u origin main
```

### 2. Update Repository URL

`repoURL` is baked into both `app-of-apps.yaml` *and* every per-component
`NNN-<component>/application.yaml` (path-based child Applications inherit
the bundle's repo URL — Argo CD does not propagate the parent's `repoURL`
to children). Update all of them in one pass:

```bash
# Update repoURL across the parent and every per-component application.yaml
find . -name 'application.yaml' -o -name 'app-of-apps.yaml' \
  | xargs sed -i 's|https://github.com/YOUR-ORG/YOUR-REPO.git|YOUR_ACTUAL_REPO_URL|g'
```

To avoid the rewrite, regenerate the bundle with `--repo` so the URL is
populated at bundle time:

```bash
aicr bundle --deployer argocd --repo YOUR_ACTUAL_REPO_URL ...
```

### 3. Apply App of Apps

```bash
kubectl apply -f app-of-apps.yaml
```

### 4. Monitor Deployment

```bash
# Watch application sync status
argocd app list

# Get detailed status for a specific application
argocd app get nvidia-stack

# Watch sync progress
argocd app sync nvidia-stack --watch
```

## Directory Structure

Each `NNN-<component>/` folder is one of two kinds, distinguished by the
presence of `Chart.yaml`. KindUpstreamHelm folders (no `Chart.yaml`) carry
just `values.yaml` + `application.yaml` — the Application is multi-source
(upstream Helm repo + your git repo for values). KindLocalHelm folders
(`Chart.yaml` + `templates/`) carry the wrapped chart bytes plus a
path-based single-source `application.yaml`.

```text
<bundle-directory>/
├── app-of-apps.yaml           # Parent application
├── README.md                  # This file
├── 001-cert-manager/
│   ├── values.yaml            # Static values (multi-source helm.valueFiles)
│   └── application.yaml       # Multi-source Application (sync-wave: 1)
├── 002-nodewright-customizations/
│   ├── Chart.yaml             # Wrapped local chart
│   ├── templates/             # Rendered manifests
│   ├── values.yaml            # Static values
│   └── application.yaml       # Path-based Application (sync-wave: 1)
```

`--dynamic` is not supported with `--deployer argocd` — use `--deployer
argocd-helm` for install-time values. The helm-deployer files
(`install.sh`, `upstream.env`, `cluster-values.yaml`) are intentionally
omitted; Argo's repo-server never consumes them.

## Sync Waves

Components are deployed in order using Argo CD sync-waves:
- **Wave 1**: cert-manager
- **Wave 1**: nodewright-customizations

## Customization

### Modifying Values

Edit the `values.yaml` file in each Helm component directory to customize the deployment.

### Changing Deployment Order

Modify the `sync-wave` annotation in each `application.yaml` to change deployment order.

## Troubleshooting

### Application Not Syncing

```bash
# Check application status
argocd app get <app-name>

# Force sync
argocd app sync <app-name> --force

# View application logs
argocd app logs <app-name>
```

### Resource Conflicts

If resources already exist, you may need to adopt them:

```bash
argocd app sync <app-name> --replace
```

## References

- [Argo CD Documentation](https://argo-cd.readthedocs.io/)
- [App of Apps Pattern](https://argo-cd.readthedocs.io/en/stable/operator-manual/cluster-bootstrapping/)
- [Sync Waves](https://argo-cd.readthedocs.io/en/stable/user-guide/sync-waves/)
