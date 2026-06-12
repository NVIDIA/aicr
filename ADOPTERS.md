# AICR Adopters

This is a non-comprehensive list of organizations and projects that publicly use —
or build on — NVIDIA AI Cluster Runtime (AICR).

💡 **Want to be listed?** Open a pull request adding a row to the table below with
your organization's name, a link, and a brief description of how you use or build
on AICR. Entries are ordered alphabetically. See [CONTRIBUTING.md](CONTRIBUTING.md)
for the PR process.

> Several organizations use AICR but aren't able to share publicly. We're grateful
> to every one of them, and to everyone building in the open below.

## Adopters

| Organization | Success Story |
|:-------------|:--------------|
| [Pulumi Labs](https://www.pulumi.com/registry/packages/nvidia-aicr/) | Publishes the **Pulumi provider for NVIDIA AICR** in the public [Pulumi Registry](https://www.pulumi.com/registry/packages/nvidia-aicr/). The provider wraps the AICR SDK and its validated recipes in a single `ClusterStack` resource: choose recipe criteria (accelerator, service, intent), point it at a target cluster, and it installs the NVIDIA GPU stack via Helm in dependency order. Usable from TypeScript, Python, Go, .NET, Java, and YAML, it lets infrastructure-as-code teams consume AICR without the `aicr` CLI. ([source](https://github.com/pulumi-labs/pulumi-nvidia-aicr)) |
