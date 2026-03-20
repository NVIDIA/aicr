# AICR Extendability Demo

## Embedded Data

View embedded data files structure:

```shell
tree -L 2 recipes/
```

## Runtime Data Support

Generate recipe with external data:

```shell
aicr recipe \
  --service eks \
  --accelerator gb200 \
  --os ubuntu \
  --intent training \
  --data ./my-data \
  --output recipe.yaml
```

Output shows:
* `7` embedded + `1` external = `8` merged components
* `dgxc-teleport` appears as Kustomize component
* Included in `deploymentOrder`

Now generate bundles:

```shell
aicr bundle \
  --recipe recipe.yaml \
  --data ./my-data \
  --deployer argocd \
  --output oci://ghcr.io/nvidia/aicr-bundle \
  --system-node-selector nodeGroup=system-pool \
  --accelerated-node-selector nodeGroup=customer-gpu \
  --accelerated-node-toleration nvidia.com/gpu=present:NoSchedule
```

### Debug Mode

The `--debug` flag shows which files are loaded from external vs embedded sources:

```bash
aicr --debug recipe \
  --service eks \
  --accelerator gb200 \
  --data ./my-data
```

## Links

* [Installation Guide](https://github.com/NVIDIA/aicr/blob/main/docs/user/installation.md)
* [CLI Reference](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md)
* [Data Reference](https://github.com/NVIDIA/aicr/blob/main/recipes/README.md)
