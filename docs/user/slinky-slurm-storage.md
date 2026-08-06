# Slurm Shared Storage

AICR can add persistent shared filesystems mounted at `/home` and `/scratch/fsw`
to Slinky Slurm login and compute pods. The feature is off by default and
requires a pre-existing Kubernetes StorageClass that supports `ReadWriteMany`
(RWX).
AICR creates the PVCs but does not provision the underlying EFS, Filestore,
Azure Files, NFS, or other filesystem service.

## Enable shared storage

Generate a Slurm recipe, then enable storage while creating the bundle:

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --platform slurm \
  --output recipe.yaml

aicr bundle \
  --recipe recipe.yaml \
  --set slinkyslurm:storage.enabled=true \
  --shared-storage-class efs-sc \
  --output bundle
```

Enabling storage creates two RWX PVCs in the `slurm` namespace:

| PVC | Default size | Mount path |
| --- | --- | --- |
| `shared-home` | `100Gi` | `/home` |
| `shared-data` | `100Gi` | `/scratch/fsw` |

Both claims are mounted in every configured LoginSet and NodeSet. The PVCs are
applied before the Slinky chart so the operator can reconcile pods against
existing claims.

`--shared-storage-class` applies to both PVCs. It is deliberately separate from
`--storage-class`: the generic class commonly points to block storage that only
supports `ReadWriteOnce`. AICR never falls back from the shared class to the
generic class.

## Override individual volumes

Use component values when home and data need different classes or capacities:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --set slinkyslurm:storage.enabled=true \
  --set slinkyslurm:storage.home.storageClassName=home-rwx \
  --set slinkyslurm:storage.home.size=500Gi \
  --set slinkyslurm:storage.data.storageClassName=data-rwx \
  --set slinkyslurm:storage.data.size=2Ti \
  --output bundle
```

Per-volume `storageClassName` values take precedence over
`--shared-storage-class`. When shared storage is enabled, both effective class
names must be non-empty; otherwise bundle creation fails. AICR always requests
`ReadWriteMany` for both claims.

## StorageClass requirements

StorageClass names are cluster-specific. For example, AKS commonly provides
`azurefile-csi`, GKE can provide `standard-rwx` when the Filestore CSI driver is
enabled, and EKS administrators commonly create an EFS class such as `efs-sc`.
These names are not portable defaults: driver enablement, filesystem identity,
networking, capacity, and access-point configuration remain operator-owned.

AICR does not query the destination cluster while creating a bundle, and
Kubernetes StorageClass objects do not advertise supported access modes.
Verify the selected class and its RWX capability before deployment.

## Lifecycle and deletion

The generated shared PVCs carry retention annotations for every supported
deployer. Helm, Helmfile, and Flux's Helm controller honor
`helm.sh/resource-policy: keep` when a release is upgraded or removed. Argo CD
treats that annotation like `Delete=false` during Application deletion; the
explicit `argocd.argoproj.io/sync-options: Delete=false,Prune=false` annotation
also prevents sync pruning, whether manual or automated, when shared storage is
disabled but the Application remains. Argo CD may report such a retained,
no-longer-desired PVC as `OutOfSync` until it is explicitly deleted or restored
to the desired manifests.

Re-enabling shared storage with the same namespace and claim names reuses the
retained claims and their data, provided the existing immutable PVC fields are
compatible. The PVC template does not hard-code Helm release ownership metadata;
Helm assigns it during installation. A different release name does not change
the claim names, but the installer may need to adopt that retained metadata
before managing them again.

Retention annotations do not protect claims from explicit deletion or from
namespace deletion. Before removing either claim, inspect its bound
PersistentVolume and reclaim policy:

```shell
kubectl -n slurm get pvc shared-home shared-data
kubectl get pv

# Destructive: run only after preserving any required data.
kubectl -n slurm delete pvc shared-home shared-data
```

Deleting a PVC can delete its backing volume or filesystem access point when
the PersistentVolume uses the `Delete` reclaim policy. The storage provider's
data-retention behavior remains operator-owned.

Shared `/etc` storage, per-user home provisioning, quotas, and filesystem
service provisioning are outside this feature's scope. The local Kind Slurm
recipe does not include shared storage support.
