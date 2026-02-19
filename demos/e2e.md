# Eidos End-to-End Demo

> Run from inside of the repo

## Setup

Clean up prior state: 

```shell
rm -rf ./bundle recipe.yaml /tmp/eidos-unpacked
```

## Commands

```shell
eidos
```

## Recipe

```shell
eidos recipe -h
```

Basic (parameters via flags):

```shell
eidos recipe --service eks --accelerator gb200 | yq .
```

From criteria file:

```shell
cat > /tmp/criteria.yaml << 'EOF'
kind: RecipeCriteria
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  name: h100-eks-training-kubeflow
spec:
  service: eks
  accelerator: h100
  os: ubuntu
  intent: training
  platform: kubeflow
EOF
```

Generate recipe from criteria file

```shell
eidos recipe --criteria /tmp/criteria.yaml --output recipe.yaml
```

> Metadata overlays: `components=11 overlays=7`

CLI flags override criteria file values

```shell
eidos recipe --criteria /tmp/criteria.yaml --service gke | yq .
```

> Metadata overlays: `components=7 overlays=2`

![data flow](images/recipe.png)

Recipe from API (GET):

```shell
curl -s "https://eidos.dgxc.io/v1/recipe?service=eks&accelerator=gb200&intent=training" | jq .
```

Recipe from API (POST with criteria body):

```shell
curl -s -X POST "https://eidos.dgxc.io/v1/recipe" \
  -H "Content-Type: application/x-yaml" \
  -d 'kind: RecipeCriteria
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  name: gb200-training
spec:
  service: eks
  accelerator: gb200
  intent: training' | jq .
```

Allowed list support in self-hosted API:

```shell
curl -s "https://eidos.dgxc.io/v1/recipe?service=eks&accelerator=l40&intent=training" | jq .
```

# Snapshot

> Requires auth'd cluster

```shell
eidos snapshot \
    --deploy-agent \
    --namespace gpu-operator \
    --node-selector nodeGroup=customer-gpu \
    --output cm://gpu-operator/eidos-snapshot
```

Check Snapshot in ConfigMap:

```shell
kubectl -n gpu-operator get cm eidos-snapshot -o jsonpath='{.data.snapshot\.yaml}' | yq .
```

Recipe from Snapshot:

```shell
eidos recipe \
  --snapshot cm://gpu-operator/eidos-snapshot \
  --intent training \
  --platform kubeflow | yq .
```

Recipe Constraints:

```shell
yq .constraints recipe.yaml
```

Validate Recipe: 

```shell
eidos validate \
  --recipe recipe.yaml \
  --require-gpu \
  --snapshot cm://gpu-operator/eidos-snapshot | yq .
```

## Bundle

Bundle from Recipe:

```shell
eidos bundle \
  --recipe recipe.yaml \
  --output ./bundle \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=customer-gpu \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule
```

Bundle from Recipe using API: 

```shell
curl -s "https://eidos.dgxc.io/v1/recipe?service=eks&accelerator=h100&intent=training" | \
  curl -X POST "https://eidos.dgxc.io/v1/bundle?deployer=argocd" \
    -H "Content-Type: application/json" -d @- -o bundle.zip
```

Navigate into the bundle:

```shell
cd ./bundle && tree .
```

![data flow](images/data.png)

Review the checksums: 

```shell
cat checksums.txt
```

Check the integrity of its content

```shell
shasum -a 256 -c checksums.txt
```

Prep the deployment: 

```shell
chmod +x deploy.sh && ./deploy.sh
```

Bundle as an OCI image:

```shell
eidos bundle \
  --recipe recipe.yaml \
  --output oci://ghcr.io/nvidia/eidos-bundle-example \
  --deployer argocd \
  --image-refs .digest
```

Review manifest: 

```shell
crane manifest "ghcr.io/nvidia/eidos-bundle-example@$(cat .digest)" | jq .
```

## Validate Cluster 

```shell
eidos validate \
  --recipe recipe.yaml \
  --require-gpu \
  --phase all
```

## Embedded Data

View embedded data files structure:

```shell
cd ../ && tree -L 2 ./recipes/
```

![data flow](images/workflow.png)

## Runtime Data Support

Need Teleport, add component: 

```shell
yq . examples/data/registry.yaml
```

Override existing recipe: 

```shell
yq . examples/data/overlays/dgxc-teleport.yaml
```

Generate recipe with external data:

```shell
eidos recipe \
  --service eks \
  --accelerator h100 \
  --os ubuntu \
  --intent training \
  --data ./examples/data \
  --output recipe.yaml
```

Output shows:
* `18` embedded + `1` external = `19` merged components
* `dgxc-teleport` appears as Kustomize component

Now `dgxc-teleport` is included in `componentRefs` and `deploymentOrder`

```shell
yq . recipe.yaml
```

Now generate bundles:

```shell
eidos bundle \
  --recipe recipe.yaml \
  --data ./examples/data \
  --deployer argocd \
  --output oci://ghcr.io/nvidia/eidos-bundle-example \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=customer-gpu \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule \
  --image-refs .digest
```

Unpack the image: 

```shell
skopeo copy "docker://ghcr.io/nvidia/eidos-bundle-example@$(cat .digest)" oci:image-oci
mkdir -p /tmp/eidos-unpacked
oras pull --oci-layout "image-oci@$(cat .digest)" -o /tmp/eidos-unpacked
tree /tmp/eidos-unpacked
```

## Links

* [Installation Guide](https://github.com/NVIDIA/eidos/blob/main/docs/user/installation.md)
* [CLI Reference](https://github.com/NVIDIA/eidos/blob/main/docs/user/cli-reference.md)
* [API Reference](https://github.com/NVIDIA/eidos/blob/main/docs/user/api-reference.md)
* [Data Reference](https://github.com/NVIDIA/eidos/blob/main/recipes/README.md)
