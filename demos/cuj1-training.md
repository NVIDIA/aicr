# AICR - Critical User Journey (CUJ) 1 — Training

End-to-end training journey: snapshot → recipe → validate → bundle → deploy →
validate → run a distributed training job. Shared steps are shown once; the
**EKS** and **GKE** subsections call out only where commands or flags genuinely
differ.

A config-driven variant of the GKE flow (single `AICRConfig` via `--config`,
finished with signed evidence) is covered at the end under
[Config-Driven GKE Variant](#config-driven-gke-variant).

## Assumptions

**EKS**

* User is already authenticated to an EKS cluster with 2+ H100 (p5.48xlarge) nodes.
* Values used in `--accelerated-node-selector`, `--accelerated-node-toleration`,
  `--system-node-selector`, and `--system-node-toleration` flags are examples only.
  Update them to match your cluster.

**GKE**

* User is already authenticated to a GKE cluster with 2+ H100 (a3-megagpu-8g) nodes.
* GKE cluster runs Container-Optimized OS (COS) with GPU drivers pre-installed.
* Values used in `--accelerated-node-selector`, `--accelerated-node-toleration`
  flags are examples only. Update them to match your cluster.
* System nodes have no custom taints (GKE managed pods don't tolerate them).

## Snapshot

**EKS**

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --output snapshot.yaml
```

**GKE**

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=gpu-workload:NoSchedule \
    --toleration nvidia.com/gpu=present:NoSchedule \
    --output snapshot.yaml
```

## Gen Recipe

**EKS**

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --intent training \
  --os ubuntu \
  --platform kubeflow \
  --output recipe.yaml
```

**GKE**

```shell
aicr recipe \
  --service gke \
  --accelerator h100 \
  --intent training \
  --os cos \
  --platform kubeflow \
  --output recipe.yaml
```

## Validate Recipe Constraints

Same for both clouds:

```shell
aicr validate \
    --recipe recipe.yaml \
    --snapshot snapshot.yaml \
    --no-cluster \
    --phase deployment \
    --output dry-run.json
```

## Generate Bundle

The selector and toleration values below mirror AICR's reference clusters: nodes
carry the label `nodeGroup={system-worker,gpu-worker}` and an unrelated taint key.
The selector key (`nodeGroup`) and toleration key (`dedicated`) intentionally
differ — the label drives scheduling targeting and the taint drives admission.
Adjust both pairs to your cluster's actual labels and taints.

> Selector / toleration flags accept comma-separated values. See the [bundle](../docs/user/cli-reference.md#aicr-bundle) section for the full flag set.
>
> Set `--storage-class` to the name of a StorageClass that exists on the target cluster (check with `kubectl get storageclass`). The cloud overlay configures `kube-prometheus-stack` with a `volumeClaimTemplate` but no `storageClassName`, so without this flag the PVC falls to the cluster's default StorageClass — and if no default is configured, the deploy hangs on a Pending PVC.

**EKS**

```shell
aicr bundle \
  --recipe recipe.yaml \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=worker-workload:NoSchedule \
  --accelerated-node-toleration dedicated=worker-workload:NoExecute \
  --system-node-selector nodeGroup=system-worker \
  --system-node-toleration dedicated=system-workload:NoSchedule \
  --system-node-toleration dedicated=system-workload:NoExecute \
  --storage-class <storage-class> \
  --output bundle
```

**GKE**

```shell
aicr bundle \
  --recipe recipe.yaml \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration dedicated=gpu-workload:NoSchedule \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --system-node-selector nodeGroup=system-worker \
  --storage-class <storage-class> \
  --output bundle
```

> **GKE only:** system nodes should not have custom taints (breaks konnectivity-agent and other GKE managed pods). Only `--system-node-selector` is needed, no `--system-node-toleration`.

## Install Bundle into the Cluster

```shell
cd ./bundle && chmod +x deploy.sh && ./deploy.sh
```

> **GKE only:** If nodewright-operator is already installed on the cluster, comment out or skip the nodewright-operator and nodewright-customizations sections in deploy.sh to avoid upgrade conflicts.

## Validate Cluster

**EKS**

```shell
aicr validate \
    --recipe recipe.yaml \
    --toleration dedicated=worker-workload:NoSchedule \
    --toleration dedicated=worker-workload:NoExecute \
    --phase all \
    --output report.json
```

**GKE**

```shell
aicr validate \
    --recipe recipe.yaml \
    --toleration dedicated=gpu-workload:NoSchedule \
    --toleration nvidia.com/gpu=present:NoSchedule \
    --phase all \
    --output report.json
```

## Run Job

Run a simple distributed PyTorch training job using the [Kubeflow TrainJob API](https://blog.kubeflow.org/trainer/intro/). Same for both clouds:

```shell
# Create the TrainJob
kubectl apply -f - <<EOF
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainJob
metadata:
  name: pytorch-mnist
  namespace: kubeflow
spec:
  trainer:
    numNodes: 1
    image: kubeflow/pytorch-dist-mnist:v1-9e12c68
    command:
      - "python3"
      - "/opt/mnist/src/mnist.py"
      - "--epochs=1"
    resourcesPerNode:
      requests:
        nvidia.com/gpu: 1
      limits:
        nvidia.com/gpu: 1
  # No podTemplateOverrides / runtimePatches needed — the torch-distributed
  # ClusterTrainingRuntime carries the cluster-aware nodeSelector and
  # tolerations baked in at bundle time from --accelerated-node-selector /
  # --accelerated-node-toleration flags.
  runtimeRef:
    name: torch-distributed
    apiGroup: trainer.kubeflow.org
    kind: ClusterTrainingRuntime
EOF

# Monitor the TrainJob
kubectl get trainjobs -n kubeflow
kubectl get pods -n kubeflow -l trainer.kubeflow.org/job-name=pytorch-mnist
kubectl logs -f -n kubeflow -l trainer.kubeflow.org/job-name=pytorch-mnist
```

## Performance Validation (GKE)

> **Note:** `aicr validate --phase performance` runs the NCCL all-reduce
> benchmark automatically for `h100-gke-cos-training` — it deploys the
> TrainJob and injects the GKE multi-NIC / TCPXO annotations for you. It
> requires at least 2 *discovered* schedulable GPU nodes (fewer than 2 returns
> a successful *skipped* result); the selected nodes also need free GPU capacity
> or the TrainJob stays Pending and the check times out — it does not skip. See [GKE TCPXO Networking](../docs/integrator/gke-tcpxo-networking.md#running-the-nccl-benchmark)
> for prerequisites, TCPXO runtime requirements, result expectations, and a
> manual standalone benchmark for debugging the data path directly.

## Success

Job success + Fabric bandwidth within range.

> Synthetic workload, perf checks beyond the basic fabric validation is out of scope here.

## Config-Driven GKE Variant

The same GKE flow can be driven by a single `AICRConfig` file via `--config` for
`snapshot`, `recipe`, `bundle`, and `validate`, and finished with
`--emit-attestation` + `aicr evidence verify` (see [`evidence.md`](./evidence.md)
for the standalone evidence walkthrough). Reproducible inputs in, signed
recipe-evidence bundle out.

### Assumptions

* Target cluster is the UAT GKE cluster provisioned by
  [`.github/workflows/uat-gcp.yaml`](../.github/workflows/uat-gcp.yaml). To
  bring it up without running the UAT suite or tearing it down:

  ```shell
  gh workflow run uat-gcp.yaml -f skip_delete=true -f skip_tests=true
  ```

  > Make sure to cleanup after yourself

  The cluster has 2× `a3-megagpu-8g` (H100, 8 GPUs/node) GPU nodes labeled
  `nodeGroup=gpu-worker` with taint `dedicated=gpu-workload:NoSchedule`, and
  system nodes labeled `nodeGroup=system-worker` (no custom taints — GKE
  managed pods don't tolerate them).
* GKE nodes run Container-Optimized OS (COS) with GPU drivers pre-installed.
* `aicr trust update` has been run once on this machine to bootstrap the
  Sigstore TUF root (prerequisite for `evidence verify`).
* OCI registry write access for `--push` (e.g. `ghcr.io/<owner>/aicr-evidence`).
  Skip `--push` to produce an unsigned local bundle.

### Config

Single source of truth for recipe criteria, bundle scheduling, validate input,
and the evidence emit path. Drop this into `aicr-config.yaml` once and reuse it
across the workflow:

```shell
cat > aicr-config.yaml <<'EOF'
kind: AICRConfig
apiVersion: aicr.run/v1alpha2
metadata:
  name: gke-h100-training
spec:
  snapshot:
    output:
      path: snapshot.yaml
    agent:
      namespace: aicr-validation
      nodeSelector:
        nodeGroup: gpu-worker
      tolerations:
        - dedicated=gpu-workload:NoSchedule
        - nvidia.com/gpu=present:NoSchedule

  recipe:
    criteria:
      service: gke
      accelerator: h100
      os: cos
      intent: training
      platform: kubeflow
    output:
      path: recipe.yaml

  bundle:
    input:
      recipe: recipe.yaml
    output:
      target: ./bundle
    deployment:
      deployer: helmfile
    scheduling:
      acceleratedNodeSelector:
        nodeGroup: gpu-worker
      acceleratedNodeTolerations:
        - dedicated=gpu-workload:NoSchedule
        - nvidia.com/gpu=present:NoSchedule
      systemNodeSelector:
        nodeGroup: system-worker
      # GKE pd-ssd-backed default; matches the UAT cluster.
      storageClass: premium-rwo

  validate:
    input:
      recipe: recipe.yaml
      snapshot: snapshot.yaml
    agent:
      namespace: aicr-validation
      tolerations:
        - dedicated=gpu-workload:NoSchedule
        - nvidia.com/gpu=present:NoSchedule
    evidence:
      attestation:
        # Setting `out` enables emit. Push target is the OCI repo;
        # the signer's OIDC identity is resolved at sign time.
        out: ./evidence
        push: ghcr.io/nvidia/aicr-evidence-cuj1-gke-demo
EOF
```

> CLI flags always win over the same field in `--config`, so the same config
> drives both the pre-deploy dry-run validate and the post-deploy `--phase all`
> validate — phase and `--no-cluster` are toggled on the command line.

### Snapshot

```shell
aicr snapshot --config aicr-config.yaml
```

Reads `spec.snapshot.*` from the config — agent namespace, GPU-node
selector, GPU-taint tolerations, and the `snapshot.yaml` output path are
all pinned there.

### Gen Recipe

```shell
aicr recipe --config aicr-config.yaml
```

Writes `recipe.yaml` per `spec.recipe.output.path`.

### Validate Recipe Constraints (dry-run, pre-deploy)

```shell
aicr validate --config aicr-config.yaml \
    --phase deployment \
    --no-cluster \
    --output dry-run.json
```

`--phase deployment` and `--no-cluster` are CLI overrides — neither is pinned
in the config so the same file works for the post-deploy `--phase all` run below.

### Generate Bundle

```shell
aicr bundle --config aicr-config.yaml
```

Writes `./bundle/` per `spec.bundle.output.target` as a helmfile release
graph (`helmfile.yaml` + per-component values), with node selectors,
tolerations, and `storageClass` already wired from `spec.bundle.scheduling`.

### Install Bundle

Requires the `helmfile` and `helm` CLIs on `$PATH`
([install](https://helmfile.readthedocs.io/en/latest/#installation)) plus the
[`helm-diff`](https://github.com/databus23/helm-diff) plugin (helmfile uses it
to render upgrade diffs):

```shell
helm plugin install https://github.com/databus23/helm-diff
```

`helmfile.yaml` carries the release graph and ordering; helmfile handles
parallelism and idempotent re-apply. This will take a few min.

```shell
cd ./bundle
helmfile apply
cd ..
```

### Validate Cluster + Emit Evidence

```shell
aicr validate --config aicr-config.yaml \
    --phase all \
    --output report.json
```

Because `spec.validate.evidence.attestation.out` is set in the config, this run
also writes a recipe-evidence bundle to `./evidence/` and pushes it (signed via
cosign keyless OIDC — opens a browser, or uses ambient GitHub Actions OIDC if
present) to `ghcr.io/<owner>/aicr-evidence`.

```text
./evidence
├── pointer.yaml                     # commit this; locator for the OCI artifact
└── summary-bundle/
    ├── attestation.intoto.jsonl     # SIGNED Sigstore Bundle (DSSE + Fulcio + Rekor)
    ├── bom.cdx.json                 # CycloneDX SBOM
    ├── ctrf/                        # per-phase CTRF test results
    │   ├── conformance.json
    │   ├── deployment.json
    │   └── performance.json
    ├── manifest.json                # per-file sha256 inventory
    ├── recipe.yaml                  # canonical post-resolution recipe
    ├── snapshot.yaml                # cluster snapshot at validate-time
    └── statement.intoto.json        # unsigned in-toto Statement
```

### Verify Evidence

Maintainer path — pull, verify signature, recompute every per-file hash, render
a Markdown summary:

```shell
aicr evidence verify ./evidence/pointer.yaml
```

Pin the expected signer when only one identity should be accepted:

> Make sure to replace the `--expected-identity-regexp` flag with your identity

```shell
aicr evidence verify ./evidence/pointer.yaml \
    --expected-issuer https://github.com/login/oauth \
    --expected-identity-regexp '^user@domain\.com$'
```

JSON for CI:

```shell
aicr evidence verify ./evidence/pointer.yaml -o evidence-result.json -t json
jq '.exit' evidence-result.json     # 0 ok, 1 validator-failed, 2 bundle invalid
```

Or, with no `--push` in the config, verify the local bundle directly (no
signature — manifest-hash chain becomes self-consistency only):

```shell
aicr evidence verify ./evidence/summary-bundle
```

### Run Job

Use the same TrainJob shown under [Run Job](#run-job) above — the
`torch-distributed` ClusterTrainingRuntime already carries the cluster-aware
nodeSelector + tolerations baked in at bundle time from `spec.bundle.scheduling`,
so no `podTemplateOverrides` are needed.

### Success

Job success + signed evidence verifies (`exit 0`) + fabric bandwidth within
range from the automated post-deploy `aicr validate --phase all` run's included performance phase (see
[GKE TCPXO Networking](../docs/integrator/gke-tcpxo-networking.md#running-the-nccl-benchmark)).
