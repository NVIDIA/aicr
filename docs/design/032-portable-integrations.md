# ADR-032: Enable portable AICR integrations across the broader Kubernetes ecosystem

## Status

**Proposed** — 2026-11-20.

## Context

Accelerators differ in how Kubernetes exposes them for allocation, which
runtime components they need, and how operators verify that workloads can use
them. When the core workflow assumes one environment, adding another can
require changes across recipe selection, discovery, and validation. That
raises the cost of contributing and maintaining an integration.

## Decision

AICR's core value is the framework it provides for configuring and validating
Kubernetes AI infrastructure. AICR will use vendor-neutral contracts across
the framework and make hardware- or platform-specific behavior pluggable where
needed. Operators can use one workflow across heterogeneous fleets while
keeping the components and tuning each environment needs. Platform teams and
community contributors can maintain integrations for the environments they
know.

Hardware differences most directly affect three areas in the AICR project.

- **Configuration catalog.** Use a common catalog model for runtime components
  and their optimizations. Contributors can maintain recipes for their
  environments, and operators can resolve those recipes and build deployment
  bundles through the same workflow.
- **Cluster discovery.** Use the snapshot as the common representation of the
  observed cluster state. Pluggable discovery captures details such as how
  accelerators are exposed for allocation and which topology affects
  placement. Recipe selection and validation use the same observations.
- **Validation and evidence.** Use a shared workflow to run checks, collect
  evidence, and publish verifiable results. Integrations can supply checks
  suited to their environments. Published evidence identifies the
  configuration tested and what the checks demonstrated.

## Qualification and maintenance

Project-maintained and community-maintained integrations must meet the same
bar for real-hardware validation, published evidence, and ongoing maintenance.
AICR maintainers steward the shared contracts and qualification criteria in
line with the project's goals and scope. Integration maintainers keep their
configurations and evidence current.

Existing catalogs and workflows remain supported.
