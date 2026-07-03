# Integrator Documentation

Documentation for engineers integrating AI Cluster Runtime (AICR) into CI/CD pipelines, GitOps workflows, or larger platforms.

## Audience

This section is for integrators who:
- Build automation pipelines using the AICR API
- Deploy and operate the AICR API server in Kubernetes
- Create custom recipes for their environments
- Integrate AICR into GitOps workflows (Argo CD, Flux)

## Documents

| Document | Description |
|----------|-------------|
| [Public API Surface](public-api.md) | Stability tiers for every exported Go package; facade type ownership |
| [Measurement Schema](measurement-api.md) | Cross-repo Measurement contract: Type cardinality, Subtype layout, NetworkTopology shape, constraint paths |
| [Go Library Integration](go-library.md) | Using `github.com/NVIDIA/aicr/pkg/client/v1` as a Go library |
| [Automation](automation.md) | CI/CD integration patterns for GitHub Actions, GitLab CI, Jenkins, and Terraform |
| [Data Flow](data-flow.md) | Understanding snapshots, recipes, validation, and bundles data transformations |
| [Kubernetes Deployment](kubernetes-deployment.md) | Self-hosted API server deployment with Kubernetes manifests |
| [EKS Dynamo Networking](eks-dynamo-networking.md) | Security group prerequisites for Dynamo overlays on EKS |
| [GKE TCPXO Networking](gke-tcpxo-networking.md) | GPUDirect TCPXO prerequisites for GKE training overlays |
| [AKS GPU Setup](aks-gpu-setup.md) | AKS prerequisites: Kubernetes 1.34+ (DRA GA), GPU driver setup, DRA configuration |
| [Talos Integration](talos-integration.md) | Running AICR on Talos Linux |
| [OpenShift Deployment](openshift.md) | OpenShift/OCP-specific Helm and OLM integration and two-phase operator deployment |
| [Recipe Development](recipe-development.md) | Creating and modifying recipe metadata for custom environments |
| [Data Extension](data-extension.md) | Extending the embedded catalog via `--data` — overlays, components, and runtime criteria values without a rebuild |
| [Validator Extension](validator-extension.md) | Adding custom validators and overriding embedded ones via `--data` |
| [Supply Chain Verification](supply-chain-verification.md) | Verifying SLSA provenance, SBOMs, and attestations; admission policies; offline verification |
| [NodeWright Component](components/nodewright.md) | NodeWright component reference and configuration |

## Quick Start

### API Server Deployment

See [Kubernetes Deployment](kubernetes-deployment.md) for full manifests. After deployment:

```shell
# Generate recipe via API
curl "http://aicrd.aicr.svc/v1/recipe?service=eks&accelerator=h100"
```

### CI/CD Integration

```yaml
# GitHub Actions example
- name: Generate recipe
  run: |
    curl -s "http://aicrd.aicr.svc/v1/recipe?service=eks&accelerator=h100" \
      -o recipe.json

- name: Generate bundles
  # recipe.json is the fully-hydrated RecipeResult from the GET above; the
  # bundle endpoint adopts it as-is and bundles all of its componentRefs.
  # To bundle a subset, add the `bundlers` query parameter (comma-delimited
  # component names, e.g. "?bundlers=gpu-operator,network-operator") rather
  # than hand-trimming componentRefs, which drops required dependencies.
  # Unknown or disabled component names are rejected with HTTP 400.
  run: |
    curl -X POST "http://aicrd.aicr.svc/v1/bundle" \
      -H "Content-Type: application/json" \
      -d @recipe.json \
      -o bundles.zip
```

## Related Documentation

- **Users**: See [User Documentation](../user/index.md) for CLI usage and installation
- **Contributors**: See [Contributor Documentation](../contributor/index.md) for architecture and development guides
