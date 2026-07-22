# End-to-End Demo

> Run from inside of the repo

## Setup

Clean up prior state:

```shell
rm -rf ./bundle ./oci-refs recipe.yaml /tmp/aicr-unpacked
```

## Commands

```shell
aicr
```

## Recipe

```shell
aicr recipe -h
```

Basic (parameters via flags):

```shell
aicr recipe --service eks --accelerator gb200 | yq .
```

From explicit criteria flags:

```shell
aicr recipe \
  --service eks --accelerator h100 --os ubuntu \
  --intent training --platform kubeflow \
  --output recipe.yaml
```


Inspect the generated recipe on stdout (pipe to `yq`):

```shell
aicr recipe --service eks --accelerator h100 --os ubuntu \
  --intent training --platform kubeflow | yq .
```


![data flow](images/recipe.png)

Recipe from API (GET):

```shell
curl -s "https://aicr-demo.dgxc.io/v1/recipe?service=eks&accelerator=gb200&intent=training" | jq .
```

Recipe from API (POST with criteria body):

```shell
curl -s -X POST "https://aicr-demo.dgxc.io/v1/recipe" \
  -H "Content-Type: application/x-yaml" \
  -d 'kind: RecipeCriteria
apiVersion: aicr.run/v1alpha2
metadata:
  name: gb200-training
spec:
  service: eks
  accelerator: gb200
  intent: training' | jq .
```

Allowed list support in self-hosted API:

```shell
curl -s "https://aicr-demo.dgxc.io/v1/recipe?service=eks&accelerator=l40&intent=training" | jq .
```

## Snapshot

> Requires auth'd cluster

```shell
aicr snapshot \
    --namespace gpu-operator \
    --node-selector nodeGroup=customer-gpu \
    --output cm://gpu-operator/aicr-snapshot
```

Check Snapshot in ConfigMap:

```shell
kubectl -n gpu-operator get cm aicr-snapshot -o jsonpath='{.data.snapshot\.yaml}' | yq .
```

Recipe from Snapshot:

```shell
aicr recipe \
  --snapshot cm://gpu-operator/aicr-snapshot \
  --intent training \
  --platform kubeflow | yq .
```

Recipe Constraints:

```shell
yq .constraints recipe.yaml
```

Validate Recipe:

```shell
aicr validate \
  --recipe recipe.yaml \
  --require-gpu \
  --snapshot cm://gpu-operator/aicr-snapshot | yq .
```

## Bundle

Bundle from Recipe:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --output ./bundle \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=customer-gpu \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule
```

Bundle from Recipe using API:

```shell
curl -s "https://aicr-demo.dgxc.io/v1/recipe?service=eks&accelerator=h100&intent=training" | \
  curl -X POST "https://aicr-demo.dgxc.io/v1/bundle?deployer=argocd" \
    -H "Content-Type: application/json" -d @- -o bundle.zip
```

Navigate into the bundle:

```shell
(cd ./bundle && tree .)
```

![data flow](images/data.png)

Review the checksums:

```shell
cat ./bundle/checksums.txt
```

Verify the complete bundle before deployment:

```shell
(cd ./bundle && aicr verify .)
```

This is the full closed-world verification gate: it checks every regular
payload file, including `recipe.yaml`, and rejects additional files or
directories, symlinks, and other non-regular filesystem objects. Legacy bundles
with incomplete manifests report `unknown` trust and must be regenerated.

Deploy:

```shell
(cd ./bundle && aicr verify . && chmod +x deploy.sh && ./deploy.sh)
```

Bundle as an OCI image:

```shell
mkdir -p ./oci-refs && chmod 0700 ./oci-refs
aicr bundle \
  --recipe recipe.yaml \
  --output oci://ghcr.io/nvidia/aicr-bundle-example \
  --deployer argocd \
  --image-refs ./oci-refs/bundle.digest
```

`--image-refs` is OCI-output-only. Its target must have an existing real parent
directory and be outside, and not aliased to, the planned or completed bundle.
AICR writes the reference file with mode `0600` and replaces its directory
entry with an anchored same-directory rename. Validation plus rename is not an
atomic identity-conditioned operation, so do not allow concurrent mutation of
the parent directory.

Review manifest:

```shell
crane manifest "ghcr.io/nvidia/aicr-bundle-example@$(cat ./oci-refs/bundle.digest)" | jq .
```

## Validate Cluster

```shell
aicr validate \
  --recipe recipe.yaml \
  --require-gpu \
  --phase all
```

## Embedded Data

```shell
tree -L 2 ./recipes/
```

![data flow](images/workflow.png)

## Runtime Data Support

Need Teleport, add component to a custom data directory (e.g. `./my-data/`):

```shell
yq . ./my-data/registry.yaml
```

Override existing recipe:

```shell
yq . ./my-data/overlays/dgxc-teleport.yaml
```

Generate recipe with external data:

```shell
aicr recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --data ./my-data \
  --output recipe.yaml
```

Output shows:
* `<N>` embedded + `<M>` external = `<N+M>` merged components
* `dgxc-teleport` appears as Kustomize component

Now `dgxc-teleport` is included in `componentRefs` and `deploymentOrder`

```shell
yq . recipe.yaml
```

Now generate bundles:

```shell
mkdir -p ./oci-refs && chmod 0700 ./oci-refs
aicr bundle \
  --recipe recipe.yaml \
  --data ./my-data \
  --deployer argocd \
  --output oci://ghcr.io/nvidia/aicr-bundle-example \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=customer-gpu \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --image-refs ./oci-refs/external-data-bundle.digest
```

Unpack the image:

```shell
skopeo copy "docker://ghcr.io/nvidia/aicr-bundle-example@$(cat ./oci-refs/external-data-bundle.digest)" oci:image-oci
mkdir -p /tmp/aicr-unpacked
oras pull --oci-layout "image-oci@$(cat ./oci-refs/external-data-bundle.digest)" -o /tmp/aicr-unpacked
tree /tmp/aicr-unpacked
```

## Summary

![data flow](images/e2e.png)

## Links

* [Installation Guide](https://github.com/NVIDIA/aicr/blob/main/docs/user/installation.md)
* [CLI Reference](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md)
* [API Reference](https://github.com/NVIDIA/aicr/blob/main/docs/user/api-reference.md)
* [Data Reference](https://github.com/NVIDIA/aicr/blob/main/recipes/README.md)
