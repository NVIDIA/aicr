# AICR - CUJ 1 — GKE (config-driven + evidence)

Same flow as [`cuj1-gke.md`](./cuj1-gke.md), but driven by a single `AICRConfig`
file via `--config` for `recipe`, `bundle`, and `validate`, and finished with
`--emit-attestation` + `aicr evidence verify` (see [`evidence.md`](./evidence.md)
for the standalone evidence walkthrough). Reproducible inputs in, signed
recipe-evidence bundle out.

> Note: This demo duplicates a lot of the validation demo from demos/evidence.md. 
>       The will be to generalize the cluster validation and evidence collection 
>       into standalone demo that will work across all services, although whether 
>       that's possible remains to be seen. 

## Assumptions

* Target cluster is the UAT GKE cluster provisioned by
  [`.github/workflows/uat-gcp.yaml`](../.github/workflows/uat-gcp.yaml). To
  bring it up without running the UAT suite or tearing it down:

  ```shell
  gh workflow run uat-gcp.yaml -f skip_delete=true -f skip_tests=true
  ```

  > Make sure to cleanup after yourself

  Then connect locally:

  ```shell
  gcloud container clusters get-credentials "aicr-<run-id>" \
    --region us-central1 --project eidosx
  kubectl get nodes
  ```

  The cluster has 2× `a3-megagpu-8g` (H100, 8 GPUs/node) GPU nodes labeled
  `nodeGroup=gpu-worker` with taint `dedicated=gpu-workload:NoSchedule`, and
  system nodes labeled `nodeGroup=system-worker` (no custom taints — GKE
  managed pods don't tolerate them).
* GKE nodes run Container-Optimized OS (COS) with GPU drivers pre-installed.
* `aicr trust update` has been run once on this machine to bootstrap the
  Sigstore TUF root (prerequisite for `evidence verify`).
* OCI registry write access for `--push` (e.g. `ghcr.io/<owner>/aicr-evidence`).
  Skip `--push` to produce an unsigned local bundle.

## Config

Single source of truth for recipe criteria, bundle scheduling, validate input,
and the evidence emit path. Drop this into `aicr-config.yaml` once and reuse it
across the workflow:

```shell
cat > aicr-config.yaml <<'EOF'
kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: gke-h100-training
spec:
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
> drives both the pre-deploy dry-run validate and the post-deploy conformance
> validate — phase and `--no-cluster` are toggled on the command line.

## Snapshot

`snapshot` does not read `--config` yet. Capture it manually:

```shell
aicr snapshot \
    --namespace aicr-validation \
    --node-selector nodeGroup=gpu-worker \
    --toleration dedicated=gpu-workload:NoSchedule \
    --toleration nvidia.com/gpu=present:NoSchedule \
    --output snapshot.yaml
```

## Gen Recipe

```shell
aicr recipe --config aicr-config.yaml
```

Writes `recipe.yaml` per `spec.recipe.output.path`.

## Validate Recipe Constraints (dry-run, pre-deploy)

```shell
aicr validate --config aicr-config.yaml \
    --phase deployment \
    --no-cluster \
    --output dry-run.json
```

`--phase deployment` and `--no-cluster` are CLI overrides — neither is pinned
in the config so the same file works for the conformance run below.

## Generate Bundle

```shell
aicr bundle --config aicr-config.yaml
```

Writes `./bundle/` per `spec.bundle.output.target` as a helmfile release
graph (`helmfile.yaml` + per-component values), with node selectors,
tolerations, and `storageClass` already wired from `spec.bundle.scheduling`.

## Install Bundle

Requires the `helmfile` and `helm` CLIs on `$PATH`
([install](https://helmfile.readthedocs.io/en/latest/#installation)) plus the
[`helm-diff`](https://github.com/databus23/helm-diff) plugin (helmfile uses it
to render upgrade diffs):

```shell
helm plugin install https://github.com/databus23/helm-diff
```

GKE's `ResourceQuota` admission plugin blocks pods that declare
`priorityClassName: system-*-critical` outside `kube-system`. The gpu-operator
controller (and its DaemonSets) request `system-node-critical`, so apply a
permissive quota in the gpu-operator namespace before the helmfile run.
See https://github.com/NVIDIA/aicr/issues/915.

```shell
kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ResourceQuota
metadata:
  name: gpu-operator-critical-pods
  namespace: gpu-operator
spec:
  hard:
    pods: "16"
  scopeSelector:
    matchExpressions:
      - operator: In
        scopeName: PriorityClass
        values:
          - system-node-critical
          - system-cluster-critical
EOF
```

`helmfile.yaml` carries the release graph and ordering; helmfile handles
parallelism and idempotent re-apply. This will take a few min.

```shell
cd ./bundle
# --skip-diff-on-install avoids a fresh-cluster CRD/CR ordering trap where
# helm-diff renders nodewright-customizations (kind: Skyhook) against live
# discovery before nodewright-operator has registered the Skyhook CRD.
# See https://github.com/NVIDIA/aicr/issues/914.
helmfile apply --skip-diff-on-install
cd ..
```

## Validate Cluster + Emit Evidence

`AICR_VALIDATOR_IMAGE_TAG=edge` works around tagged-release binaries asking for
a `:sha-<commit>` validator image the release workflow never publishes (it
publishes `:v<version>` instead). Drop the env var once running a binary built
from a `main` push (whose `:sha-<commit>` images exist).
See https://github.com/NVIDIA/aicr/issues/916.

```shell
AICR_VALIDATOR_IMAGE_TAG=edge aicr validate --config aicr-config.yaml \
    --phase conformance \
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
    │   └── deployment.json
    ├── manifest.json                # per-file sha256 inventory
    ├── recipe.yaml                  # canonical post-resolution recipe
    ├── snapshot.yaml                # cluster snapshot at validate-time
    └── statement.intoto.json        # unsigned in-toto Statement
```

## Verify Evidence

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

## Run Job

Distributed PyTorch training via the [Kubeflow TrainJob
API](https://blog.kubeflow.org/trainer/intro/) — the
`torch-distributed` ClusterTrainingRuntime already carries the cluster-aware
nodeSelector + tolerations baked in at bundle time from
`spec.bundle.scheduling`, so no `podTemplateOverrides` are needed:

```shell
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
  runtimeRef:
    name: torch-distributed
    apiGroup: trainer.kubeflow.org
    kind: ClusterTrainingRuntime
EOF

kubectl get trainjobs -n kubeflow
kubectl logs -f -n kubeflow -l job-name=pytorch-mnist-node-0
```

## Performance Validation

`aicr validate --phase performance` is not yet automated for GKE (TCPXO daemon
sidecar required for GPUDirect differs from the EKS TrainJob path).

## Success

Job success + signed evidence verifies (`exit 0`) + fabric bandwidth within
range from the manual NCCL run.
