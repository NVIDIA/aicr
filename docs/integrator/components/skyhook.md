# What is it

Skyhook and Skyhook-customizations are two halves of a component. [Skyhook](https://github.com/NVIDIA/skyhook) is a Kubernetes Operator to apply [skyhook packages](https://github.com/NVIDIA/skyhook-packages) in a consistent, repeatable and tested lifecycles within a cluster. Skyhook-customizations are instances of the [Skyhook Custom Resource](https://github.com/NVIDIA/skyhook/blob/main/chart/templates/skyhook-crd.yaml) that define one or more skyhook-packages to deploy. These packages were selected to provide two main functions:
1. Optimize a node for inference or training workloads via grub, sysctl and systemd service settings.
2. Be able to install all of the necessary software to bring a vanilla kubernetes node to the AICR spec.

## References

1. [Skyhook documentation](https://github.com/NVIDIA/skyhook/blob/main/docs)

# Optimizer

Uses tuned to apply a sequence of profiles to optimize primarily grub and sysctl settings. Your mileage may vary depending on the particulars of the virtualization if not running baremetal.

Package: [nvidia-tuned](https://github.com/NVIDIA/skyhook-packages/tree/main/nvidia-tuned)

Configuration [documentation](https://github.com/NVIDIA/skyhook-packages/tree/main/nvidia-tuned#usage):

A full configuration supplies: `intent`, `accelerator`, `service`. A minimal configuration is just `accelerator`
```
configMap:
    intent: inference
    accelerator: h100
    service: eks
```

Supported accelerators: `h100`, `gb200`

Integration notes:
  * If you provide a service it MUST exist in the [profiles service directory](https://github.com/NVIDIA/skyhook-packages/tree/main/nvidia-tuned/profiles/service)
  * If you are integrating a new service beware that even tested paths may not fully work due to limitations in that service. For example you will notice that `eks` has overrides to remove setting `kernel.sched_latency_ns` and `kernel.sched_min_granularity_ns` as these are not available on AWS kernels. They cannot fail silently as the package will test to make sure the changes asked for actually happens and error if it does not.

## Secondary optimizer

A second, more stripped down, optimizer is available for operating systems that are mostly read only such as GKE's ContainerOptimizedOS. In this case the [nvidia-tuning-gke](https://github.com/NVIDIA/skyhook-packages/tree/main/nvidia-tuning-gke) is available to directly perform sysctl writes. Also note the change in Skyhook configuration to write to a different directory tree in order to have a writable FS and to re-apply changes every boot: [recipes/overlays/gke-cos.yaml](https://github.com/NVIDIA/aicr/blob/main/recipes/overlays/gke-cos.yaml#L69)
```
    - name: skyhook-operator
      type: Helm
      overrides:
        controllerManager:
          manager:
            env:
              # GKE COS has a read-only rootfs, so we need to use a different directory
              # /etc is stateless so better represents the flag and history on reboot
              copyDirRoot: /etc/skyhook
              # Because what skyhook does is generally on /etc we need to reapply on reboot
              reapplyOnReboot: "true"
```

## Versioning and extension notes

Both of these packages (nvidia-tuned and nvidia-tuning-gke) extend other skyhook packages (tuned and tuning) and as such could directly use those and provide the configuration via configmaps. The choice was made to go with specific versioned packages in order to provide a more clear path for upgrades and understanding differences. However, the base packages are still useful to quickly iterate on configurations without requiring new versions of the extended packages used in AICR.

# Setup

Uses a set of bash scripts to do the necessary actions to bring an ubuntu worker to the desired AICR spec.

Package: [nvidia-setup](https://github.com/NVIDIA/skyhook-packages/blob/main/nvidia-setup)

Currently supports: eks and gb200/h100. Each service must be added explicitly and the documentation for the addition is in the readme for how to make this update.

The [version overview](https://github.com/NVIDIA/skyhook-packages/blob/main/nvidia-setup/VERSION_OVERVIEW.md) has all of the information about what each version for a service + accelerator pair will install or configure.

# Manifests

## Tuning

Includes the setup and optimizations for a specific service, accelerator and intent. Note that while it does have if statements around the service and intent due to the inclusion of the nvidia-setup which does require service these are all not optional and would need to be split it to properly support this. Currently tested with:
 * eks, gb200, multiNodeTraining
 * eks, gb200, inference
 * eks, h100, multiNodeTraining
 * eks, h100, inference

 To support non service specific tuning for example: h100, inference. The nvidia-tuned package would need to be separated out or nvidia-setup updated to support additional services or have less assumptions about what it is installing as it is currently opinionated towards EKS.

 See [recipes/components/skyhook-customizations/manifests/tuning.yaml](https://github.com/NVIDIA/aicr/blob/main/recipes/components/skyhook-customizations/manifests/tuning.yaml)

## Tuning-gke

A GKE + Container Optimized OS (COS) specific tuning that only sets some of the sysctl settings and does NOT require any interrupts due to being able to configure seamlessly while workloads are running.

See [recipes/components/skyhook-customizations/manifests/tuning-gke.yaml](https://github.com/NVIDIA/aicr/blob/main/recipes/components/skyhook-customizations/manifests/tuning-gke.yaml)

## No-op

A no-op package may be used as a place holder until a full package suite can be tested. See [recipes/components/skyhook-customizations/manifests/no-op.yaml](https://github.com/NVIDIA/aicr/blob/main/recipes/components/skyhook-customizations/manifests/no-op.yaml)