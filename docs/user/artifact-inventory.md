# Artifact inventories and bindings

Artifact inventories let a release publisher supply chart and image identities independently of the recipe catalog.
This feature requires **AICR v1.3.0 or newer**.
Integrators must pin the AICR executable to **v1.3.0 or newer**.
Earlier releases from v0.21.0 through v1.2.0 refuse inventory catalogs.
Releases before v0.21.0 are unsupported and unsafe with inventory catalogs: some silently ignore `artifacts/` and use catalog pins, and very old readers also fail to reject saved artifact provenance.
Historical rejection behavior is not a compatibility guarantee for those unsupported releases.
The inventory and bindings use `aicr.run/v1beta1`; generated recipes with artifact provenance use the strict `aicr.run/v1beta2` RecipeResult format.

## Supply release identities

Place the inventory at `<data>/artifacts/inventory.yaml`, or select a local file with `spec.recipe.artifacts.inventory` in an [AICRConfig](cli-config.md).
Bindings alone do not activate composition.
Without an inventory, ordinary recipe loading and resolution retain their existing behavior.

```yaml
apiVersion: aicr.run/v1beta1
kind: ArtifactInventory
metadata:
  name: example-release
artifacts:
  - name: controller-chart
    type: chart
    reference: oci://registry.example.com/charts/controller
    version: v2.0.0_build.1
    manifestDigest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    archiveDigest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  - name: controller-image
    type: image
    reference: oci://registry.example.com/images/controller
    version: release-42
    manifestDigest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
```

These digests are illustrative; use the publisher's actual digests.
`reference` contains neither a tag nor a digest and starts with literal lowercase `oci://`.
Its registry host must contain a dot or port, or be `localhost`, so container tools cannot reinterpret it as a Docker Hub path.
The chart version is the literal OCI tag: an optional leading `v` is preserved, and `_` encodes SemVer build metadata.
The chart manifest digest and chart archive digest identify different bytes; an active chart binding requires the archive digest.

The parser accepts one UTF-8 YAML or JSON document with string scalar values, at most 8 MiB and depth 32.
It rejects unknown fields, duplicate keys, nulls, anchors, aliases, merge keys, explicit tags, non-string mapping keys and multiple documents.
Artifact names are unique and digests must be lowercase `sha256:` followed by 64 hexadecimal characters.

## Map identities to components

Catalog authors place bindings in `artifacts/bindings.yaml`.
Embedded bindings are layered with `--data` bindings by artifact name: an external chart, image or ignored disposition replaces the complete embedded disposition of that name.
The embedded document supplies `cert-manager-chart`, `slurm-accounting-mariadb-chart` and `slurm-accounting-mariadb-image`.
Provide additional mappings in your catalog; replace an embedded name to change its disposition.

```yaml
apiVersion: aicr.run/v1beta1
kind: ArtifactBindings
charts:
  controller-chart:
    component: controller
images:
  controller-image:
    targets:
      - component: controller
        valuesFile: components/controller/values.yaml
        write:
          repository: /image/repository
          digest: /image/digest
          clearTag: /image/tag
ignoredArtifacts: {}
```

The component must already exist in the catalog and use Helm.
An image binding writes existing string leaves addressed by JSON Pointers in a local YAML or JSON values file.
For charts taking a complete image reference, use `write: {reference: /controller/image}` instead of the three split fields.
The resulting identity is `registry.example.com/images/controller@sha256:…`; the optional `clearTag` pointer clears a tag field when the chart requires it.
Arrays and computed image layouts are unsupported, including GPU Operator and Network Operator image layouts.
Their charts may still be bound.

A single-manifest image can declare `platform: {os: linux, architecture: arm64, variant: v8}`.
An optional target platform assertion must match exactly, including the variant; AICR never infers it from the machine running the CLI.
An image-index entry without a platform needs no target platform assertion.

Every inventory entry must have one chart binding, image binding or explicit ignored disposition such as `ignoredArtifacts: {release-notes: {reason: not deployed}}`.
Unused binding names may remain in a shared catalog; only active targets require catalog files and components to exist.

## Resolve and consume recipes

```yaml
apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  recipe:
    artifacts:
      inventory: ./release-inventory.yaml
      allowCatalogPins:
        - cert-manager
```

`allowCatalogPins` names existing catalog components allowed to retain missing chart or image matches.
Names must be unique.
It does not disable active bindings or permit an override to change a bound identity.
Without an exception, each selected enabled Helm component needs its chart and all images declared for it in the merged bindings.
The bound values file must participate in that component's final values.

```shell
aicr recipe validate --data ./catalog --config aicr-config.yaml
aicr recipe --data ./catalog --config aicr-config.yaml \
  --service eks --accelerator h100 --os ubuntu --intent training -o recipe.yaml
aicr bundle --data ./catalog --config aicr-config.yaml \
  --recipe recipe.yaml --deployer helm -o bundle
```

Static validation checks schemas, inventory accounting and active binding targets.
It does not select a recipe or require inventory coverage of the whole catalog.
Standalone inventory and bindings files receive schema validation; accounting and target checks require catalog context.

Resolution records inventory, merged bindings and base catalog digests, exceptions, applied identities and ignored identities in `metadata.artifacts`.
A bundle captures the original external inputs under `data/`, including the selected inventory and external bindings; embedded bindings are retained by the matching base catalog.
Loading a saved recipe for validation, discovery or bundling requires the same inputs and exception set.
Use the captured `data/` directory and the same `allowCatalogPins` configuration to replay a bundle.
If an inventory override would replace a default inventory that also serves as catalog data, capture keeps that original file and stores the selected inventory as `artifacts/selected-inventory-<sha256>.yaml`; select that file explicitly when replaying.

Namespace-only inheritance can read a historical recipe with different inventory provenance, extracting only component names and namespaces.
Full inheritance requires matching provenance for an artifact-bearing source and rejects conflicting bound identities.
A legacy recipe may supply full inheritance identity, but it cannot be executed with an active inventory until re-resolved.

| Reason on `INVALID_REQUEST` | Action |
| --- | --- |
| `artifact-unbound` | Bind or explicitly ignore the named inventory entry. |
| `artifact-target-invalid` | Fix the active component, values file, pointer, platform assertion or participating file. |
| `artifact-coverage-required` | Supply missing chart/image entries or name a deliberate `allowCatalogPins` exception. |
| `artifact-identity-conflict` | Remove the conflicting override or inherited identity. |
| `artifact-provenance-required` | Re-resolve the legacy saved recipe under the selected inventory. |
| `artifact-inputs-required` | Supply the original inventory, bindings, catalog and exceptions. |
| `artifact-inputs-mismatch` | Restore the original inputs or re-resolve. |
| `artifact-upgrade-unsupported` | Artifact-bound inputs and inventory-enabled clients are unsupported by upgrade-check. |
| `artifact-source-unsupported` | Use an embedded or filesystem SDK source; OCI composition is unsupported. |
| `artifact-invalid` | Correct the named schema or semantic error. |

The HTTP server rejects artifact-bearing recipes with `artifact-inputs-required`; it has no inventory transport.

Provenance is marked `metadata-only`.
It records the supplied identities and composition, not publisher authentication, chart archive verification or runtime image completeness.
Mirror discovery guards final identities but its version-fetched renders are not verified against the pinned archive.
Terraform and Fleet GitRepo archive verification remains tracked separately in [issue 3653](https://github.com/NVIDIA/aicr/issues/3653).
For the complete format and trust boundary, see [ADR-031](https://github.com/NVIDIA/aicr/blob/main/docs/design/031-artifact-inventory-and-bindings.md).
