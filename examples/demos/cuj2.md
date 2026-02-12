# Eidos - Critical User Journey (CUJ) 2

> Assuming user is already authenticated to Kubernetes cluster

## Gen Recipe
TODO: add `gb200` accelerator
```shell
eidos recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent inference \
  --platform dynamo \
  --output recipe.yaml
```
Sample output
```
[cli] building recipe from criteria: criteria=criteria(service=eks, accelerator=h100, intent=inference, os=ubuntu, platform=dynamo)
[cli] recipe generation completed: output=recipe.yaml components=16 overlays=7
```

## Validate Recipe Constraints

```shell
eidos validate \
  --phase readiness \
  --namespace gpu-operator \
  --node-selector nodeGroup=customer-gpu \
  --recipe recipe.yaml
```

Sample output:
```
recipeSource: recipe.yaml
snapshotSource: agent:gpu-operator/eidos-validate
summary:
  passed: 4
  failed: 0
  skipped: 0
  total: 4
  status: pass
  duration: 477.583µs
phases:
  readiness:
    status: pass
    constraints:
      - name: K8s.server.version
        expected: '>= 1.34'
        actual: v1.34.3-eks-ac2d5a0
        status: passed
      - name: OS.release.ID
        expected: ubuntu
        actual: ubuntu
        status: passed
      - name: OS.release.VERSION_ID
        expected: "24.04"
        actual: "24.04"
        status: passed
      - name: OS.sysctl./proc/sys/kernel/osrelease
        expected: '>= 6.8'
        actual: 6.14.0-1018-aws
        status: passed
    duration: 477.583µs
```

> Assuming cluster meets recipe constraints

## Generate Bundle

> Assuming user updates selectors and tolerations as needed

```shell
eidos bundle \
  --recipe recipe.yaml \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=gpu-worker \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --output bundle
```

Sample output:
```
[cli] generating bundle: deployer=helm type=Helm per-component bundle recipe=recipe.yaml output=./bundle oci=false
[cli] bundle generated: type=Helm per-component bundle files=42 size_bytes=666795 duration_sec=0.053811959 output_dir=./bundle

Helm per-component bundle generated successfully!
Output directory: ./bundle
Files generated: 42

To deploy:
  1. cd ./bundle
  2. chmod +x deploy.sh
  3. ./deploy.sh
```

## Install Bundle into the Cluster

```shell
chmod +x deploy.sh
./deploy.sh
```

## Validate Cluster 

```shell
eidos validate \
  --phase readiness \
  --phase deployment \
  --phase conformance \
  --recipe recipe.yaml
```

Results (TODO: add full per-component health check and AI Conformance check)

```
recipeSource: recipe.yaml
snapshotSource: agent:gpu-operator/eidos-validate
summary:
  passed: 4
  failed: 0
  skipped: 0
  total: 4
  status: pass
  duration: 1.452461125s
phases:
  conformance:
    status: skipped
    reason: conformance phase not configured in recipe
    duration: 9.709µs
  deployment:
    status: skipped
    reason: deployment phase not configured in recipe
    duration: 7.042µs
  readiness:
    status: pass
    constraints:
      - name: K8s.server.version
        expected: '>= 1.34'
        actual: v1.34.3-eks-ac2d5a0
        status: passed
      - name: OS.release.ID
        expected: ubuntu
        actual: ubuntu
        status: passed
      - name: OS.release.VERSION_ID
        expected: "24.04"
        actual: "24.04"
        status: passed
      - name: OS.sysctl./proc/sys/kernel/osrelease
        expected: '>= 6.8'
        actual: 6.14.0-1018-aws
        status: passed
    duration: 64µs
```

## Run Job

TODO: Add simple Dynamo workload

## Success

1) Job success
2) Validation report correctly reflects the level of CNCF Conformance

> Synthetic workload, perf checks beyond the basic fabric validation is out of scope here.

