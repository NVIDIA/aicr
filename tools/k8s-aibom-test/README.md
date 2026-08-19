# k8s-aibom Behavior Test

```bash
make k8s-aibom-test
```

ADR-019 requires a separate, named test for `k8s-aibom` because the generic
component harness stops after the read-only health check: it proves the
controller is up, never that it produces a correct AIBOM. This is that test,
and only that. Cluster lifecycle, recipe synthesis, bundling, installation,
teardown, and the health check are delegated to `tools/component-test`, so the
shared Kind cluster (`aicr-component-test`) and the single-component bundle
path are exercised exactly as every other component's are.

Set `KEEP_CLUSTER=true` to leave the cluster and fixture namespace in place for
debugging, or `OUTPUT_DIR=<path>` to keep the evidence files.

## What it proves

- system scheduling injected by AICR reaches the **rendered controller
  Deployment** — the half of the claim no unit test can make, because only the
  real chart knows whether the registry's declared value paths are the ones it
  reads;
- an explicitly labeled namespace produces a Ready AIBOM for a digest-pinned,
  replicas-zero Deployment;
- the AIBOM carries a controller owner reference to that Deployment;
- `status.bomDocument.sha256` matches the decoded canonical bytes;
- both the initial and updated documents are valid CycloneDX 1.6;
- a reconcile with unchanged inventory input preserves `status.inputHash`;
- changing the workload image digest changes `status.inputHash`; and
- deleting the Deployment garbage-collects its AIBOM.

It deliberately makes no `status.bomHash` stability assertion: that hash covers
output bytes carrying a generation timestamp, so it changes on every
regeneration by design.

The fixtures cannot start pods — they request zero replicas. Their image
references are immutable inputs for the controller only.

## What is proven elsewhere

This test is the cluster-behavior tier. The render-level claims ADR-019 also
requires are hermetic Go tests that run on every PR, which is where they
belong:

| Claim | Where |
|---|---|
| Renders through `helm`, `helmfile`, `argocd`, `argocd-helm`, `flux` with the qualified digest and secure defaults | `pkg/bundler/k8s_aibom_render_parity_test.go` |
| Vendored-chart output at the shared-writer level | same file, `TestK8sAIBOM_VendoredChartRendersThroughWriter` |
| `hasSelfRefCRDs` produces Helmfile `disableValidation: true` | same file, `TestK8sAIBOM_HelmfileDisablesValidation` |
| Scheduling injected at the registry-declared value paths | same file, `TestK8sAIBOM_SystemSchedulingInjectionPaths` |
| Mirror hands the qualified, digest-pinned coordinates to the renderer | `pkg/mirror/k8s_aibom_discover_test.go` |
| Health check fails closed on every unhealthy cluster state | `pkg/chainsaw/k8s_aibom_check_states_test.go` |
| Stock recipes resolve to unchanged bytes | `pkg/recipe/catalog_parity_golden_test.go` |
| Stock recipes render to unchanged bytes | `pkg/bundler/stock_render_parity_golden_test.go` |

OCI bundle publication and pull-through are covered generically for every
deployer by the ADR-008 / ADR-010 KWOK lanes
(`.github/workflows/kwok-recipes.yaml`), which run on every PR. Digest
preservation under `crane copy` is a property of crane/oras, not of this
component, and is not re-proven per component.

## validate-bom

`validate-bom/` decodes an emitted document through
`github.com/CycloneDX/cyclonedx-go` — already an AICR dependency, used by
`pkg/bom` to build AICR's own BOMs — and asserts `bomFormat`, `specVersion`,
non-empty components, and metadata.

This is deliberately weaker than validating against the published CycloneDX
JSON Schema. Schema validation would mean carrying a JSON-Schema library as a
production dependency solely for a local qualification command. The one-time
full schema verdict for the qualified v1.2.0 artifact set is recorded in the
ADR-019 implementation PR; this command is the standing regression check.
