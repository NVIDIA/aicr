---
title: "Documentation"
linkTitle: "Documentation"
weight: 1
description: "NVIDIA AI Cluster Runtime documentation"
---

AICR generates validated GPU-accelerated Kubernetes configurations through a four-stage workflow:

1. **Snapshot** — Capture cluster state (GPU topology, drivers, Kubernetes config)
2. **Recipe** — Generate optimized configuration for your hardware and intent
3. **Validate** — Check constraints against actual cluster state
4. **Bundle** — Create deployment-ready Helm values and manifests

## Getting Started

New to AICR? Start with the [Getting Started guide](/docs/getting-started/).

## Documentation by Role

| Role | Description | Guide |
|------|-------------|-------|
| **User** | Install and operate AICR | [User Guide](/docs/user/) |
| **Integrator** | Embed AICR in automation pipelines | [Integrator Guide](/docs/integrator/) |
| **Contributor** | Understand internals, add components | [Contributor Guide](/docs/contributor/) |

## Additional Resources

- [Conformance Evidence](/docs/conformance/) — CNCF AI conformance test results
- [Project Info](/docs/project/) — Contributing guidelines, development setup, releases
