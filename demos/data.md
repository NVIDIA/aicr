# Recipe Data Architecture Demo

This demo walks through the recipe metadata system, showing how multi-level inheritance, criteria matching, and component configuration work together.

## Intro 

> Rule-based configuration engine over Metadata composes optimized REcipes for given set of criteria

![](images/data.png)

Demo: 

1. **Base recipe** - Universal component definitions and constraints applicable to every recipe
2. **Environment-specific overlays** - Config optimization based on query criteria 
3. **Inheritance chains** - Resolving overlays via intermediate recipes
4. **Merging strategy** - Components, constraints, and values are merged with overlay precedence
5. **Computing deployment order** - Topological sort of components based on dependency references

> Terminology (see [glossary](https://github.com/NVIDIA/aicr/blob/main/docs/README.md))

## Recipe Data (Design time == files in git)

View embedded recipe files structure:

```shell
tree -L 2 recipes/
```

### Base

Base recipe (foundation for all recipes):

```shell
yq . recipes/overlays/base.yaml
```

### Constraints

Based on measurements:

```shell
# Capture a live snapshot, then inspect it
aicr snapshot --output snapshot.yaml
yq . snapshot.yaml | head -n 20
```

Constraint format: `{MeasurementType}.{Subtype}.{Key}`

Examples:
- `K8s.server.version` - Kubernetes version
- `OS.release.ID` - Operating system ID
- `GPU.smi.driver_version` - GPU driver version

### Base Values

GPU Operator 

```shell
cat recipes/components/gpu-operator/values.yaml | yq .
```

### Multi-Level Inheritance

EKS recipe (example of inheritance from base):

```shell
yq . recipes/overlays/eks.yaml
```

EKS training recipe (inherits from eks):

```shell
yq . recipes/overlays/eks-training.yaml
```

View GB200 EKS training recipe (inherits from eks-training):

```shell
yq . recipes/overlays/gb200-eks-training.yaml
```

### Multi-Level Inheritance (Values)

Training-optimized values:

```shell
cat recipes/components/gpu-operator/values-eks-training.yaml | yq .
```

Values are merged in order (later = higher priority):

```
Base ValuesFile → Overlay ValuesFile → Overlay Overrides → CLI --set flags
```

View leaf recipe (inherits from gb200-eks-training):

```shell
yq recipes/overlays/gb200-eks-ubuntu-training.yaml
```

## Criteria Matching (runtime == at query time, compiled binary)

At query time, a de facto graph is created, user queries then "selects" the things that match.

### Broad Query (matches multiple overlays)

```shell
aicr recipe --service eks | yq .metadata
```

This matches:

```yaml
  appliedOverlays:
    - base
    - eks
```

Versions: 

```shell
aicr -v
```

### More Specific Query

```shell
aicr recipe \
    --service eks \
    --intent training \
    | yq .metadata
```

This matches:

```yaml
  appliedOverlays:
    - base
    - eks
    - eks-training
```

### Most Specific Query

```shell
aicr recipe \
    --service eks \
    --accelerator gb200 \
    --intent training \
    --os ubuntu \
    --platform kubeflow \
    | yq .metadata
```

This matches all levels:

```yaml
  appliedOverlays:
    - base
    - eks
    - eks-training
    - gb200-eks-training
    - gb200-eks-ubuntu-training
    # Platform-specific overlays (e.g., kubeflow) would be applied here
```

## Deployment Order

Recipes define their own dependencies:

```shell
yq . recipes/overlays/base.yaml
```

View computed deployment order is computed at recipe composition time and sorted based on dependencies:

```shell
aicr recipe \
    --service eks \
    --accelerator gb200 \
    --intent training \
    --os ubuntu \
    --platform kubeflow \
    | yq .deploymentOrder
```

Order in `dependencyRefs`:
1. `cert-manager` (no dependencies)
2. `gpu-operator` (depends on cert-manager)
3. Other components...

> Asymmetric rule matching based on [Kahn's algorithm](https://www.geeksforgeeks.org/dsa/topological-sorting-indegree-based-solution/) algorithm.

## API Access

Same recipe via API:

```shell
curl -s "https://aicr-demo.dgxc.io/v1/recipe?service=eks&accelerator=gb200&intent=training" | jq .
```

View applied overlays:

```shell
curl -s "https://aicr-demo.dgxc.io/v1/recipe?service=eks&accelerator=gb200&intent=training" | jq .metadata.appliedOverlays
```

## Validation Tests

Run recipe data validation tests (checks inheritance ref, criteria enums, cycle refs, etc.):

```shell
go test -v ./pkg/recipe/...
```

E2E tests runs every recipe for every combo of criteria:

```shell
make e2e
```

> All of this is executed on PRs, can't merge sans these tests passing

Integrity of the metadata is paramount!

![](images/recipe.png)

## Links

### Demo

- [This Demo](https://github.com/NVIDIA/aicr/blob/main/demos/data.md) - Full architecture documentation

### Documentation
- [Data Architecture](https://github.com/NVIDIA/aicr/blob/main/docs/contributor/data.md) - Full architecture documentation
- [Recipe Development Guide](https://github.com/NVIDIA/aicr/blob/main/docs/integrator/recipe-development.md) - Adding/modifying recipes
- [CLI Reference](https://github.com/NVIDIA/aicr/blob/main/docs/user/cli-reference.md) - Recipe command options

### Source Code
- [Recipe Data Files](https://github.com/NVIDIA/aicr/tree/main/recipes) - YAML recipe definitions
- [Metadata Store](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/metadata_store.go) - Inheritance resolution
- [Criteria Matching](https://github.com/NVIDIA/aicr/blob/main/pkg/recipe/criteria.go) - Matching algorithm
