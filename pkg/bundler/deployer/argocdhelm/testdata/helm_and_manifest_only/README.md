# Argo CD Helm Chart Deployment Bundle

This bundle is a Helm chart whose templates generate one Argo CD
Application per component plus a parent Application that manages
the chart deployment as a whole. The bundle's `repoURL` defaults
to the registry it was pushed to; override with `--set
repoURL=oci://mirror` when deploying from a different registry.

## Deploy

`<your-registry>/<path>` below is the **parent namespace** you
publish into. The chart name (`aicr-bundle`) is appended by Helm at
push time and by the parent Application at sync time — do NOT
include the chart name in `--set repoURL` or it will be appended
twice and the parent Application will fail to resolve.

```bash
# 1. Publish to your chart registry (any HTTPS OCI / Helm chart repo).
helm package . --destination /tmp/
helm push /tmp/aicr-bundle-*.tgz oci://<your-registry>/<path>

# 2. Install from the published chart — supply repoURL (the
#    parent namespace, NOT including the chart name) and
#    targetRevision so the parent Application and path-based child
#    Applications can pull from the registry you pushed to.
helm install aicr-bundle oci://<your-registry>/<path>/aicr-bundle \
  --version <chart-version> -n argocd \
  --set repoURL=oci://<your-registry>/<path> \
  --set targetRevision=<chart-version>
```

`helm install` against this local directory works only when the
recipe contains pure-Helm components — child Applications whose
source is path-based (manifest-only, mixed `-post`) need Argo's
repo-server to fetch from a remote, so the chart must be published
first for those cases.

```bash
# Local install (pure-Helm-only recipes)
helm install aicr-bundle . -n argocd \
  --set repoURL=oci://<your-registry>/<path> \
  --set targetRevision=<chart-version> \
  --set certmanager.replicaCount=VALUE
```

## Deployer Options

Child Application deployer options are install-time overridable:

- `--set deployer.namePrefix=<prefix>` — prefix prepended to every
  child Application name (multi-tenant collision avoidance).
- `--set deployer.destinationServer=<url>` — target cluster API URL
  for child Applications (default in-cluster).
- `--set deployer.project=<project>` — Argo CD project for child
  Applications.
- `--set deployer.includeRootApp=false` — render children-only,
  omitting the parent app-of-apps Application. Use when an external
  root Application (e.g. created by a controller) already points at
  this chart; two roots owning the same children fight via automated
  prune/selfHeal.

`cascadeDelete` is bundle-time only (finalizers cannot round-trip as
a template expression): `aicr bundle --argocd-cascade-delete`. Child
sync retry is bundle-time only too: `aicr bundle --argocd-sync-retry-limit`.

## Dynamic Values

- `certmanager.replicaCount`
