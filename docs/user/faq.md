# Frequently Asked Questions

Short answers to common questions about AI Cluster Runtime (AICR). Each answer points to the full guide, which stays the source of truth.

For where to ask a question that is not covered here, see [SUPPORT.md](https://github.com/NVIDIA/aicr/blob/main/SUPPORT.md).

## Snapshot

**Does `aicr snapshot` need the NVIDIA driver or `nvidia-smi` on the node?**

No. Snapshot GPU detection is driver-free: it enumerates PCI devices through sysfs and resolves the accelerator SKU from the device ID. You can snapshot a freshly provisioned node before any driver is installed. See [GPU Detection Issues](installation.md#gpu-detection-issues).

**Can I capture a snapshot through the self-hosted `aicrd` API server?**

No. A snapshot deploys an agent into the cluster to capture node configuration, which the CLI does using your kubeconfig. The API server only generates artifacts (recipe, query, and bundle), so use `aicr snapshot` instead. See [API Reference](api-reference.md) and [Agent Deployment](agent-deployment.md).

## Recipes

**Why does `aicr recipe` fail with "requires additional criteria"?**

A partial query can match several recipes, so AICR fails with an actionable error instead of silently returning a partial one. For example, no single recipe covers `h100` and `ubuntu` without also stating `--service` (and, for some services, `--intent`). The error lists the supported combinations; add one and re-run. See [Recipe Not Found](cli-reference.md#recipe-not-found).

**Does AICR use the EFA device plugin or the AWS networking DRA driver on EKS?**

EKS recipes currently install the `aws-efa` device plugin, which handles only EFA and needs EFA-enabled instance types. On a cluster that already runs the AWS networking DRA driver, that plugin is redundant and can cause duplication or crash-loops. Whether AICR keeps the plugin, migrates to the DRA driver, chooses per instance type or Kubernetes version, or detects an existing driver and skips the install is an open backlog item in [#1326](https://github.com/NVIDIA/aicr/issues/1326). See the [Component Catalog](component-catalog.md).

## Bundles

**Why does `aicr bundle --vendor-charts` fail with "helm CLI not found on PATH"?**

Vendoring runs `helm pull` to download the upstream chart archives, so the `helm` binary must be on your `PATH`. Install Helm and re-run, or drop `--vendor-charts` to get a bundle that references the chart registries. See [Vendoring Charts for Air-Gap](cli-reference.md#vendoring-charts-for-air-gap).

**Can I bundle an older recipe with a newer `aicr` binary?**

No, it is not supported. A recipe is a point-in-time artifact of the binary that produced it, and a newer binary may no longer contain a manifest the recipe references ("failed to load manifest"). Regenerate the recipe with the binary you are bundling with. See [Bundle Generation Fails](cli-reference.md#bundle-generation-fails).

**Are CRDs upgraded when a component's Helm chart is upgraded?**

Helm skips `crds/` on upgrade, so whether CRDs update depends on the deployer, the component, and whether the recipe overrides the chart. Check the deployer table before upgrading: [Upgrade, uninstall, and troubleshooting](component-catalog.md#upgrade-uninstall-and-troubleshooting).
