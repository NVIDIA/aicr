---
title: "NVIDIA AI Cluster Runtime"
linkTitle: "AICR"
---

{{< blocks/cover title="NVIDIA AI Cluster Runtime" image_anchor="top" height="med" >}}
<p class="lead mt-4">Generate validated GPU-accelerated Kubernetes configurations</p>
<a class="btn btn-lg btn-primary me-3 mb-4" href="docs/">
Documentation
</a>
<a class="btn btn-lg btn-secondary me-3 mb-4" href="https://github.com/NVIDIA/aicr">
GitHub <i class="fab fa-github ms-2"></i>
</a>
{{< /blocks/cover >}}

{{% blocks/lead color="primary" %}}
AICR provides tooling for deploying optimized, validated GPU workloads on Kubernetes.

**Workflow:** Snapshot → Recipe → Validate → Bundle
{{% /blocks/lead %}}

{{< blocks/section color="dark" type="row" >}}

{{% blocks/feature icon="fa-cube" title="Snapshot" %}}
Capture cluster state including GPU topology, drivers, and Kubernetes configuration.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-cogs" title="Recipe" %}}
Generate optimized configurations based on hardware, intent, and cloud service.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-check-circle" title="Validate" %}}
Check constraints against actual cluster state to ensure compatibility.
{{% /blocks/feature %}}

{{< /blocks/section >}}

{{< blocks/section type="row" >}}

{{% blocks/feature icon="fa-box" title="Bundle" %}}
Create deployment-ready Helm values and manifests for your target environment.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-book" title="Multi-Persona Docs" %}}
Guides for [users](/docs/user/), [integrators](/docs/integrator/), and [contributors](/docs/contributor/).
{{% /blocks/feature %}}

{{% blocks/feature icon="fab fa-github" title="Open Source" url="https://github.com/NVIDIA/aicr" %}}
Apache 2.0 licensed. Contributions welcome.
{{% /blocks/feature %}}

{{< /blocks/section >}}
