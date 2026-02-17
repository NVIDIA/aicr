# Eidos End-to-End Demo

## Recipe

Basic (query parameters):

```shell
eidos recipe \
  --service eks \
  --accelerator gb200 \
  --os ubuntu \
  --intent training \
  --platform kubeflow | yq .
```

From criteria file:

```shell
# Create criteria file
cat > /tmp/criteria.yaml << 'EOF'
kind: RecipeCriteria
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-training
spec:
  service: eks
  accelerator: gb200
  os: ubuntu
  intent: training
  platform: kubeflow
EOF

# Generate recipe from criteria file
eidos recipe --criteria /tmp/criteria.yaml | yq .

# CLI flags override criteria file values
eidos recipe --criteria /tmp/criteria.yaml --service gke | yq .
```

Metadata overlays: `components=5 overlays=5`

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

Make Snapshot: 

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
  --platform kubeflow \
  --output recipe.yaml
```

Recipe Constraints:

```shell
yq .constraints recipe.yaml
```

Validate Recipe: 

```shell
eidos validate \
  --recipe recipe.yaml \
  --namespace gpu-operator \
  --snapshot cm://gpu-operator/eidos-snapshot | yq .
```

Validate Recipe sans Snapshot

```shell
eidos validate \
  --recipe recipe.yaml \
  --node-selector nodeGroup=customer-gpu
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
cd ./bundle
```

Check bundle content: 

```shell
tree .
```

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

View bundle README: 

```shell
grip --browser --quiet ./bundle/README.md
```

Bundle as an OCI image:

```shell
eidos bundle \
  --recipe recipe.yaml \
  --output oci://ghcr.io/NVIDIA/eidos-bundle \
  --deployer argocd \
  --image-refs .digest
```

Review manifest: 

```shell
crane manifest "ghcr.io/NVIDIA/eidos-bundle@$(cat .digest)" | jq .
```

Unpack the image: 

```shell
skopeo copy "docker://ghcr.io/NVIDIA/eidos-bundle@$(cat .digest)" oci:image-oci
mkdir -p ./eidos-unpacked
oras pull --oci-layout "image-oci@$(cat .digest)" -o ./eidos-unpacked
```

## Links

* [Installation Guide](https://github.com/NVIDIA/eidos/blob/main/docs/user/installation.md)
* [CLI Reference](https://github.com/NVIDIA/eidos/blob/main/docs/user/cli-reference.md)
* [API Reference](https://github.com/NVIDIA/eidos/blob/main/docs/user/api-reference.md)
