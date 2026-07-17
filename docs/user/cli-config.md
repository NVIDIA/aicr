# CLI Configuration File (AICRConfig)

`AICRConfig` is a Kubernetes-style YAML/JSON document that captures the inputs
to the four workflow commands — `aicr snapshot`, `aicr recipe`, `aicr bundle`,
and `aicr validate` — so an end-to-end run version-controls as a single file
instead of a shell script full of flags. Each command accepts it through the
same `--config` flag:

```shell
aicr snapshot --config aicr-config.yaml
aicr recipe   --config aicr-config.yaml
aicr bundle   --config aicr-config.yaml
aicr validate --config aicr-config.yaml
```

This page documents the complete document schema in one place. The
[CLI Reference](cli-reference.md) shows per-command usage in its
[Snapshot](cli-reference.md#snapshot-config-file-mode),
[Recipe](cli-reference.md#config-file-mode-recommended),
[Validate](cli-reference.md#validate-config-file-mode), and
[Bundle](cli-reference.md#bundle-config-file-mode) config-file-mode sections.
The schema's source of truth is
[`pkg/config`](https://github.com/NVIDIA/aicr/tree/main/pkg/config).

## Document Envelope

```yaml
kind: AICRConfig               # required, exactly this value
apiVersion: aicr.run/v1alpha2  # required, exactly this value
metadata:
  name: gke-h100-training      # optional, identifying only
spec:
  snapshot: {}                 # each section optional —
  recipe: {}                   # at least ONE must be present
  bundle: {}
  validate: {}
```

Each `spec.*` section is optional and each command reads only its own section,
so a file may carry just one section or any combination. A document with none
of the four sections is rejected.

## Loading, Precedence, and Secrets

**Sources.** `--config` accepts a local file path or an HTTP/HTTPS URL
(format detected from the extension; fetches are timeout- and size-bounded).
ConfigMap `cm://` URIs are intentionally rejected — extract the data with
`kubectl` and pass the resulting file.

**Precedence.** A CLI flag always wins over the matching config field. For
slice and map fields (tolerations, selectors, `--set`), a flag given on the
command line **replaces** the file's value; it does not append.

**Nil vs. empty.** For agent selectors and tolerations, omitting the field
entirely (nil) inherits the compiled-in defaults (`tolerations` defaults to
*tolerate all taints*), while an explicit empty value (`{}` / `[]`) clears the
default. Several booleans are tri-state for the same reason: absent means
"inherit the CLI default", an explicit `false` is an opt-out
(`spec.validate.execution.failOnError`, `failFast`,
`spec.snapshot.execution.privileged`, `spec.recipe.criteriaStrict`,
`spec.validate.evidence.*`).

**Secrets are never part of the schema.** The cosign OIDC identity token used
by attestation and evidence push is deliberately absent — supply it via the
`COSIGN_IDENTITY_TOKEN` environment variable or the `--identity-token` flag.

## Complete Example

```yaml
kind: AICRConfig
apiVersion: aicr.run/v1alpha2
metadata:
  name: eks-h100-training
spec:
  snapshot:
    output:
      path: snapshot.yaml            # same shape as -o
      format: yaml                   # yaml | json | table
      template: ""                   # optional Go template path
    agent:                           # in-cluster snapshot Job pod
      namespace: aicr-validation
      image: ""                      # default: ghcr.io/nvidia/aicr:latest
      imagePullSecrets: []
      jobName: aicr
      serviceAccountName: aicr
      nodeSelector:
        nodeGroup: gpu-worker
      tolerations:
        - dedicated=gpu-workload:NoSchedule
      requireGpu: false
      runtimeClassName: ""           # mutually exclusive with requireGpu
      os: ""                         # ubuntu | rhel | cos | amazonlinux | ol | talos
      requests: ""                   # "cpu=500m,memory=1Gi"
      limits: ""
    execution:
      timeout: 5m
      noCleanup: false
      privileged: true               # false for PSS-restricted namespaces
      maxNodesPerEntry: 0            # 0 = unlimited topology entries

  recipe:
    criteria:                        # mutually exclusive with input.snapshot
      service: eks
      accelerator: h100
      intent: training
      os: ubuntu
      platform: kubeflow
      nodes: 2
    # input:
    #   snapshot: snapshot.yaml      # derive criteria from a snapshot instead
    output:
      path: recipe.yaml
      format: yaml                   # yaml | json | table
    data: ""                         # optional data-overlay dir/archive
    criteriaStrict: false            # reject criteria outside the embedded catalog

  bundle:
    input:
      recipe: recipe.yaml            # must match recipe.output.path when both set
    output:
      target: ./bundles              # local dir or oci:// URI
      imageRefs: ""                  # external digest file; OCI output only
    deployment:
      deployer: helmfile             # helm | helmfile | argocd | argocd-helm | flux | ...
      repo: ""
      set: []                        # value overrides, "key:path=value"
      dynamic: []
      vendorCharts: false
      appName: ""                    # argocd parent Application name override
    scheduling:
      systemNodeSelector: {}
      systemNodeTolerations: []
      acceleratedNodeSelector:
        nodeGroup: gpu-worker
      acceleratedNodeTolerations:
        - nvidia.com/gpu=present:NoSchedule
      workloadGate: ""
      workloadSelector: {}
      nodes: 2
      storageClass: ""
    attestation:
      enabled: false
      certificateIdentityRegexp: ""
      oidcDeviceFlow: false
      fulcioURL: ""                  # private Sigstore overrides; empty = public good
      rekorURL: ""
      signingKey: ""                 # KMS key ref (awskms:// | gcpkms:// | ...); empty = keyless OIDC
    registry:                        # OCI transport for oci:// push
      insecureTLS: false
      plainHTTP: false

  validate:
    input:
      recipe: recipe.yaml
      snapshot: snapshot.yaml
    agent:                           # same nil-vs-empty semantics as snapshot.agent
      namespace: aicr-validation
      image: ""
      imagePullSecrets: []
      jobName: aicr
      serviceAccountName: aicr
      nodeSelector:
        nodeGroup: gpu-worker
      tolerations:
        - nvidia.com/gpu=present:NoSchedule
      requireGpu: false
    execution:
      phases: [deployment, conformance, performance]
      failOnError: true              # tri-state; absent = CLI default (true)
      failFast: false
      noCluster: false
      noCleanup: false
      timeout: 40m
    evidence:
      cncf:                          # CNCF AI Conformance markdown
        dir: ./evidence
        cncfSubmission: false        # requires dir
        features: []                 # empty = all features
      attestation:                   # recipe-evidence v1 bundle (ADR-007)
        out: evidence-result.json    # setting this enables the path
        bom: ""
        push: ""                     # OCI ref to push the signed bundle
        plainHTTP: false
        insecureTLS: false
```

## Field Reference

### spec.snapshot

Inputs to `aicr snapshot`. There is no input section — the snapshot is
produced from the live cluster.

| Field | Type | Notes |
|-------|------|-------|
| `output.path` | string | Output file path (same as `-o`) |
| `output.format` | string | `yaml` \| `json` \| `table` |
| `output.template` | string | Optional Go template path |
| `agent.*` | object | In-cluster capture Job pod: `namespace`, `image`, `imagePullSecrets`, `jobName`, `serviceAccountName`, `nodeSelector`, `tolerations`, `requireGpu`, `runtimeClassName` (mutually exclusive with `requireGpu`), `os`, `requests`, `limits`. Mirrors `spec.validate.agent` so one file pins matching placement for both |
| `execution.timeout` | duration string | e.g. `5m` |
| `execution.noCleanup` | bool | Keep the capture Job after completion |
| `execution.privileged` | bool (tri-state) | Set `false` for PSS-restricted namespaces |
| `execution.maxNodesPerEntry` | int | `0` = unlimited topology entries |

### spec.recipe

Inputs to `aicr recipe`. `criteria` and `input.snapshot` are **mutually
exclusive** — query by criteria or derive from a snapshot, not both.

| Field | Type | Notes |
|-------|------|-------|
| `criteria.service` / `.accelerator` / `.intent` / `.os` / `.platform` | string | Same names and values as the CLI flags |
| `criteria.nodes` | int | Target GPU node count |
| `input.snapshot` | string | Snapshot path to derive the recipe from |
| `output.path` | string | Recipe output path |
| `output.format` | string | `yaml` \| `json` \| `table` |
| `data` | string | Data-overlay directory/archive (same as `--data`) |
| `criteriaStrict` | bool (tri-state) | Reject criteria values outside the embedded catalog; mirrors `--criteria-strict` / `AICR_CRITERIA_STRICT` (any of the three enables it) |

### spec.bundle

Inputs to `aicr bundle`.

| Field | Type | Notes |
|-------|------|-------|
| `input.recipe` | string | Recipe to bundle |
| `output.target` | string | Local directory or `oci://` URI |
| `output.imageRefs` | string | Optional external image-reference output file for an OCI `output.target` only. Local output is rejected. Its parent must be an existing real directory, and the target must be outside and not aliased to the planned or completed bundle. |
| `deployment.deployer` | string | Deployer choice (same values as `--deployer`) |
| `deployment.repo` | string | GitOps repo for repo-shaped deployers |
| `deployment.set` / `.dynamic` | []string | Value overrides, `key:path=value` |
| `deployment.vendorCharts` | bool | Vendor charts into the bundle |
| `deployment.appName` | string | Argo CD parent `Application` name override (multi-bundle installs sharing a namespace) |
| `scheduling.*` | object | `systemNodeSelector`/`Tolerations`, `acceleratedNodeSelector`/`Tolerations`, `workloadGate`, `workloadSelector`, `nodes`, `storageClass`. Selectors are YAML maps; tolerations use the CLI's `key=value:effect` strings |
| `attestation.enabled` | bool | Enable bundle attestation (signing); keyless OIDC by default, KMS-backed when `signingKey` is set |
| `attestation.certificateIdentityRegexp` | string | Expected signer identity |
| `attestation.oidcDeviceFlow` | bool | Device-code flow for headless signing |
| `attestation.fulcioURL` / `.rekorURL` | string | Private Sigstore endpoints; empty = public-good defaults |
| `attestation.signingKey` | string | KMS key reference for key-based signing (`awskms://` \| `gcpkms://` \| `azurekms://` \| `hashivault://`); empty = keyless OIDC. Mutually exclusive with the keyless-only inputs (`oidcDeviceFlow`, `fulcioURL`, `--identity-token`) |
| `registry.insecureTLS` / `.plainHTTP` | bool | OCI transport options for push |

When `output.imageRefs` is set, AICR writes the published OCI digest through a
mode-`0600` temporary file and an anchored same-directory rename. The target
may be absent or an existing retained regular file; directories, symlinks,
other non-regular files, and bundle aliases are rejected. The final validation
and rename are ordered but are not one atomic identity-conditioned filesystem
operation, so no other process should mutate the target directory while the
bundle command runs.

### spec.validate

Inputs to `aicr validate`.

| Field | Type | Notes |
|-------|------|-------|
| `input.recipe` / `.snapshot` | string | Recipe + snapshot to validate |
| `agent.*` | object | In-cluster validation Job pod; same fields and nil-vs-empty semantics as `spec.snapshot.agent` (minus `runtimeClassName`/`os`/`requests`/`limits`) |
| `execution.phases` | []string | e.g. `[deployment, conformance, performance]` |
| `execution.failOnError` | bool (tri-state) | Absent = CLI default (`true`); explicit `false` opts out |
| `execution.failFast` | bool (tri-state) | Stop after the first failed phase |
| `execution.noCluster` | bool | Test mode: no cluster access, constraints evaluated inline |
| `execution.noCleanup` | bool | Keep validation Jobs after completion |
| `execution.timeout` | duration string | e.g. `40m` |
| `evidence.cncf.dir` | string | CNCF AI Conformance evidence directory (`--evidence-dir`) |
| `evidence.cncf.cncfSubmission` | bool (tri-state) | Emit submission layout; requires `dir` |
| `evidence.cncf.features` | []string | Empty = all features; honored only with `cncfSubmission` |
| `evidence.attestation.out` | string | Recipe-evidence v1 result path — setting it **enables** the attestation path |
| `evidence.attestation.bom` / `.push` | string | BOM input; OCI ref for the signed bundle push |
| `evidence.attestation.plainHTTP` / `.insecureTLS` | bool (tri-state) | Push transport options |

## Cross-Section Rules

- At least one of the four `spec.*` sections must be present.
- `spec.recipe.criteria` and `spec.recipe.input.snapshot` are mutually
  exclusive.
- When **both** `spec.recipe.output.path` and `spec.bundle.input.recipe` are
  set, they must reference the same file (compared after `filepath.Clean`;
  mixing absolute and relative forms is rejected). Mismatched paths in a
  workflow file are almost always a typo, so the loader fails up-front.
- Enum-valued fields (`criteria.*`, output formats, phases) are validated with
  the same parsers the CLI flags use, so error messages match the CLI's.

## See Also

- [CLI Reference](cli-reference.md) — per-command flags and config-file-mode
  examples
- [Validation](validation.md) — validation phases and evidence workflow
- [Bundling](bundling.md) — deployers and bundle outputs
