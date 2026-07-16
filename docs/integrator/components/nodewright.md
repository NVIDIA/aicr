# What is it

Nodewright and nodewright-customizations are two halves of the integration. [Nodewright](https://github.com/NVIDIA/nodewright) is a Kubernetes Operator that applies [nodewright packages](https://github.com/NVIDIA/nodewright-packages) with consistent, repeatable, and tested lifecycles within a cluster. Nodewright-customizations are instances of the [Skyhook Custom Resource](https://github.com/NVIDIA/nodewright/blob/main/chart/templates/skyhook-crd.yaml) that define one or more nodewright packages to deploy. These packages were selected to provide two main functions:
1. Optimize a node for inference or training workloads via grub, sysctl and systemd service settings.
2. Be able to install all of the necessary software to bring a vanilla kubernetes node to the AICR spec.

## References

1. [Nodewright documentation](https://github.com/NVIDIA/nodewright/blob/main/docs)

# Optimizer

Uses tuned to apply a sequence of profiles to optimize primarily grub and sysctl settings. Your mileage may vary depending on the particulars of the virtualization if not running baremetal.

Package: [nvidia-tuned](https://github.com/NVIDIA/nodewright-packages/tree/main/nvidia-tuned)

Configuration [documentation](https://github.com/NVIDIA/nodewright-packages/tree/main/nvidia-tuned#usage):

A full configuration supplies: `intent`, `accelerator`, `service`. A minimal configuration is just `accelerator`
```
configMap:
    intent: inference
    accelerator: h100
    service: eks
```

Supported accelerators: `h100`, `gb200`

Integration notes:
  * If you provide a service it MUST exist in the [profiles service directory](https://github.com/NVIDIA/nodewright-packages/tree/main/nvidia-tuned/profiles/service)
  * If you are integrating a new service beware that even tested paths may not fully work due to limitations in that service. For example you will notice that `eks` has overrides to remove setting `kernel.sched_latency_ns` and `kernel.sched_min_granularity_ns` as these are not available on AWS kernels. They cannot fail silently as the package will test to make sure the changes asked for actually happens and error if it does not.

## Secondary optimizer

A second, more stripped down, optimizer is available for operating systems that are mostly read only such as GKE's ContainerOptimizedOS. In this case the [nvidia-tuning-gke](https://github.com/NVIDIA/nodewright-packages/tree/main/nvidia-tuning-gke) is available to directly perform sysctl writes. Also note the change in Nodewright configuration to write to a different directory tree in order to have a writable FS and to re-apply changes every boot: [recipes/overlays/gke-cos.yaml](https://github.com/NVIDIA/aicr/blob/main/recipes/overlays/gke-cos.yaml#L69)
```
    - name: nodewright-operator
      type: Helm
      overrides:
        controllerManager:
          manager:
            env:
              # GKE COS has a read-only rootfs, so we need to use a different directory
              # /etc is stateless so better represents the flag and history on reboot
              copyDirRoot: /etc/nodewright
              # Because what nodewright does is generally on /etc we need to reapply on reboot
              reapplyOnReboot: "true"
```

## Versioning and extension notes

Both of these packages (nvidia-tuned and nvidia-tuning-gke) extend other nodewright packages (tuned and tuning) and as such could directly use those and provide the configuration via configmaps. The choice was made to go with specific versioned packages in order to provide a more clear path for upgrades and understanding differences. However, the base packages are still useful to quickly iterate on configurations without requiring new versions of the extended packages used in AICR.

# Setup

Uses a set of bash scripts to do the necessary actions to bring an ubuntu worker to the desired AICR spec.

Package: [nvidia-setup](https://github.com/NVIDIA/nodewright-packages/blob/main/nvidia-setup)

See the [Tuning status](#tuning-and-setup) table below for the current service + accelerator coverage. Each service must be added explicitly; the documentation for how to make this update is in the [nvidia-setup README](https://github.com/NVIDIA/nodewright-packages/tree/main/nvidia-setup).

The [version overview](https://github.com/NVIDIA/nodewright-packages/blob/main/nvidia-setup/VERSION_OVERVIEW.md) has all of the information about what each version for a service + accelerator pair will install or configure.

# Manifests

## Tuning and Setup

Tuning are typically alterations to sysctl, kernel boot parameters and service drop ins to make the system better optimized for AI workloads.

Setup generally is any configuration needed to make AI workloads work in that service that is not already provided by that service. In EKS for example this is kernel and EFA installation. For BCM it is symlinks to support gpu operator.

The table below is generated from the recipes by `make tuning-docs` — **do not edit it by hand**. **Setup** and **Tuning** are the pinned nodewright package versions applied for each service + accelerator. **Profile** is the tuning profile accelerator when it differs from the selected accelerator (for example, `h200` and `a100` nodes use the `h100` profile), and `-` when identical. In the **Setup** and **Tuning** columns, `-` means no such package is applied for that service + accelerator. `*` means the recipe pins no value for that dimension.

{/* BEGIN AICR-TUNING */}

| Service | Accelerator  | Profile | Setup              | Tuning                  |
|---------|--------------|---------|--------------------|-------------------------|
| aks     | a100         | h100    | nvidia-setup 0.5.0 | nvidia-tuned 0.3.2      |
| aks     | h100         | -       | nvidia-setup 0.5.0 | nvidia-tuned 0.3.2      |
| bcm     | *            | h100    | nvidia-setup 0.3.0 | -                       |
| bcm     | h100         | -       | nvidia-setup 0.3.0 | -                       |
| eks     | a100         | h100    | nvidia-setup 0.5.0 | nvidia-tuned 0.3.2      |
| eks     | gb200        | -       | nvidia-setup 0.5.0 | nvidia-tuned 0.3.2      |
| eks     | h100         | -       | nvidia-setup 0.5.0 | nvidia-tuned 0.3.2      |
| eks     | h200         | h100    | nvidia-setup 0.5.0 | nvidia-tuned 0.3.2      |
| eks     | rtx-pro-6000 | generic | -                  | nvidia-tuned 0.3.2      |
| gke     | a100         | h100    | -                  | nvidia-tuning-gke 0.1.2 |
| gke     | b200         | -       | -                  | nvidia-tuning-gke 0.1.2 |
| gke     | h100         | -       | -                  | nvidia-tuning-gke 0.1.2 |

{/* END AICR-TUNING */}

See [recipes/components/nodewright-customizations/manifests](https://github.com/NVIDIA/aicr/blob/main/recipes/components/nodewright-customizations/manifests) for the specifics on packages and their configuration.

## Tuning-gke

A GKE + Container Optimized OS (COS) specific tuning that only sets some of the sysctl settings and does NOT require any interrupts due to being able to configure seamlessly while workloads are running.

See [recipes/components/nodewright-customizations/manifests/tuning-gke.yaml](https://github.com/NVIDIA/aicr/blob/main/recipes/components/nodewright-customizations/manifests/tuning-gke.yaml)

## No-op

A no-op package may be used as a place holder until a full package suite can be tested. See [recipes/components/nodewright-customizations/manifests/no-op.yaml](https://github.com/NVIDIA/aicr/blob/main/recipes/components/nodewright-customizations/manifests/no-op.yaml)
