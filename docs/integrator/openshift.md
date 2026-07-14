# OpenShift Deployment

## Overview

The **OpenShift deployment** models each OCP operator as two in-tree local Helm charts, following a two-phase lifecycle per component:

1. **Phase 1 — OLM chart (`*-ocp-olm`):** A local Helm chart whose templates contain the OLM resources (Namespace, OperatorGroup, Subscription). Values control channel, version, approval strategy, and source catalog. A readiness gate ensures the operator CSV reaches `Succeeded` before the next phase deploys.

2. **Phase 2 — CR chart (`*-ocp`):** A local Helm chart whose templates contain the operator's Custom Resource (e.g., `ClusterPolicy`, `NodeFeatureDiscovery`, `NicClusterPolicy`). Values control the CR spec. Applied after Phase 1 completes via `dependencyRefs`.

Every OCP-managed operator follows this same two-phase pattern. As additional operators are added to the OCP overlay, each one is modeled as an `*-ocp-olm` / `*-ocp` pair — no new deployment mechanisms are introduced.

### Why Helm?

AICR's entire generation pipeline — recipe resolution, value overrides, bundle rendering, and deployer integration — is built on Helm as its universal packaging format. Rather than introducing a separate OLM-specific code path, the OpenShift support models OLM resources (Subscriptions, OperatorGroups) and operator Custom Resources as standard in-tree Helm chart templates. This means OCP components benefit from the same overlay system, `--set` value overrides, readiness hooks, and deployer support that every other AICR-managed component uses — without any OLM-specific adapter or deployment logic.

Helm is the packaging and rendering layer, not a runtime requirement. OpenShift environments that do not use Helm in their deployment pipelines can run `helm template` on any emitted chart folder to produce plain Kubernetes YAML with all values fully resolved. The resulting manifests are suitable for direct application via `oc apply -f` or ingestion into any manifest-based pipeline. This applies equally to Argo CD deployer output — the generated `Application` CRs can be rendered into static manifests the same way, allowing teams to adopt AICR's validated configurations without changing their existing GitOps tooling.

### Two-Phase Architecture

The Subscription (Phase 1) and the Custom Resource (Phase 2) follow independent lifecycles: the Subscription bootstraps the operator environment while the CR acts as the ongoing trigger for the operator to provision and manage the workload. This separation creates two distinct operational flows — one for the operator installation and one for the workload configuration.

When `--readiness-hooks` is enabled, a readiness gate is inserted between the two phases. This gate uses a Chainsaw assertion to verify that the operator's ClusterServiceVersion (CSV) has reached `Succeeded` phase before the CR chart is applied:

```text
Phase 1 (OLM)         Readiness Gate              Phase 2 (CR)
┌─────────────────┐    ┌──────────────────────┐    ┌──────────────────┐
│ OperatorGroup   │    │ CSV phase=Succeeded  │    │ Custom Resource  │
│ Subscription    │───▶│                      │───▶│ (operator CR)    │
└─────────────────┘    └──────────────────────┘    └──────────────────┘
     install.sh         install.sh --wait            install.sh
                        --timeout
```

### OLM Architecture

The Operator Lifecycle Manager provides a declarative approach to managing operator lifecycles:

- **CatalogSource**: Defines the operator registry (Red Hat Certified Operators, Community Operators, etc.)
- **Subscription**: Requests installation of an operator from a catalog
- **InstallPlan**: Automatic approval/installation of operator resources (CSV, CRDs, etc.)
- **ClusterServiceVersion (CSV)**: Describes the operator and its resource requirements
- **Custom Resources (CRs)**: User-defined configurations that the operator reconciles

**OpenShift-Specific Constraints:**

- **Certified Operators**: OCP components use Red Hat-certified operator catalogs (`certified-operators`, `redhat-operators`) when available
- **Security Context Constraints (SCC)**: Operators may require privileged access for driver installation
- **Entitlement**: RHEL-based driver builds may require Red Hat entitlement ConfigMaps
- **Version Alignment**: Operator channel versions must align with OpenShift Container Platform (OCP) version

### Readiness Gates

Each OLM component carries a `readiness.yaml` using a Chainsaw assertion that
checks one condition:

1. **CSV Succeeded** — the ClusterServiceVersion has reached `phase: Succeeded`

OLM advances the CSV to `Succeeded` only once the operator's install strategy
(its Deployment) reports available, so the CSV phase is the single signal the
gate waits on before CRs are applied.

When `--readiness-hooks` is enabled, the bundler emits a `-readiness` folder between the OLM and CR folders for each operator. The readiness Job runs with `helm install --wait --timeout`, blocking the deployment pipeline until the operator is fully ready.

### Naming Convention

OCP components use dedicated names following the pattern `<operator>-ocp-olm` and `<operator>-ocp` rather than reusing the base component names. The base OCP overlay disables the upstream Helm-based components (e.g., `gpu-operator`, `nfd`) that are replaced by their OLM equivalents, and also disables components that are not applicable to the OCP platform (e.g., components managed natively by OpenShift or not yet supported).

The list of supported components and their OLM/CR pairs grows over time. Refer to the base OCP overlay (`recipes/overlays/ocp.yaml`) and the component registry (`recipes/registry.yaml`) for the current set of supported operators.

## Complete Deployment Workflow

This section demonstrates the end-to-end deployment process on OpenShift with commands and expected outputs.

### 1. Generate Recipe

Generate a recipe by specifying OpenShift as the service:

```bash
aicr recipe \
  --service ocp \
  --accelerator h100 \
  --os rhel \
  --intent training \
  --output recipe.yaml
```

**Expected Output:**

```text
[cli] building recipe from criteria: criteria=criteria(service=ocp, accelerator=h100, intent=training, os=rhel)
[cli] recipe generation completed: output=recipe.yaml
```

**Verify Recipe Contents:**

```bash
cat recipe.yaml
```

The recipe includes OpenShift-specific component references with two-phase OLM deployment. Each operator appears as a pair — an OLM component with `manifestFiles` and a CR component with `dependencyRefs` back to the OLM component. For example, the GPU Operator entry looks like:

```yaml
...
spec:
  componentRefs:
    - name: gpu-operator-ocp-olm
      type: Helm
      valuesFile: components/gpu-operator-ocp-olm/values.yaml
      manifestFiles:
        - components/gpu-operator-ocp-olm/manifests/operatorgroup.yaml
        - components/gpu-operator-ocp-olm/manifests/subscription.yaml
      dependencyRefs:
        - nfd-ocp        # waits for NFD CR to be applied

    - name: gpu-operator-ocp
      type: Helm
      valuesFile: components/gpu-operator-ocp/values.yaml
      manifestFiles:
        - components/gpu-operator-ocp/manifests/clusterpolicy.yaml
      dependencyRefs:
        - gpu-operator-ocp-olm  # waits for OLM phase to complete
...
```

The `dependencyRefs` create a deployment ordering chain across all operators. Each CR component depends on its OLM counterpart, and operators that require prerequisites (e.g., GPU Operator depends on NFD labels) declare cross-operator dependencies.

### 2. Generate Bundle

Create a deployment bundle from the recipe. Use `--readiness-hooks` to insert readiness gates between OLM and CR phases:

```bash
aicr bundle \
  --recipe recipe.yaml \
  --readiness-hooks \
  --output ./ocp-bundle
```

**Bundle Directory Structure:**

The bundler emits three numbered folders per operator — OLM, readiness gate, and CR — following the standard local Helm chart layout:

```text
ocp-bundle/
├── deploy.sh                               # Deploys all components in order
├── undeploy.sh                             # Cleanup script
├── README.md                               # Bundle documentation
├── recipe.yaml                             # Recipe used to generate bundle
├── checksums.txt                           # SHA256 of all listed files (recipe.yaml excluded, #1549)
│
│   # ── Per-operator three-folder cycle ──
│
├── 0XX-<operator>-ocp-olm/                 # Phase 1: OLM Subscription
│   ├── Chart.yaml
│   ├── templates/
│   │   ├── operatorgroup.yaml
│   │   └── subscription.yaml
│   ├── values.yaml
│   └── install.sh
├── 0XX-<operator>-ocp-olm-readiness/       # Readiness gate
│   ├── Chart.yaml
│   ├── templates/
│   │   └── check-job.yaml
│   └── install.sh                          # runs with --wait --timeout
├── 0XX-<operator>-ocp/                     # Phase 2: Operator CR
│   ├── Chart.yaml
│   ├── templates/
│   │   └── <custom-resource>.yaml
│   ├── values.yaml
│   └── install.sh
│
│   # ... repeated for each operator in the recipe
```

Each numbered folder is a standard local Helm chart. The `deploy.sh` script installs them sequentially. Readiness folders (`*-readiness`) use `helm install --wait --timeout` to block until the gate passes.

### 3. Deploy Components

The `deploy.sh` script installs all components in dependency order. Each folder is a self-contained Helm chart:

```bash
cd ocp-bundle
./deploy.sh
```

The deployment proceeds through the three-folder cycle per operator: OLM install → readiness gate → CR apply.

**Manual step-by-step deployment** is also supported. Each folder can be installed independently using standard Helm:

```bash
helm upgrade --install <release> ./<folder> --create-namespace -n <namespace>
```

For readiness gate folders, add the wait flags:

```bash
helm upgrade --install <release> ./<readiness-folder> -n <namespace> --wait --timeout 10m
```

### 4. Monitor Operator Readiness

After OLM subscriptions are installed, verify operator readiness by checking CSV status and Deployment availability in the operator's namespace:

**Check CSV phase:**

```bash
oc get csv -n <operator-namespace>
```

**Expected Output:**

```text
NAME                                  DISPLAY        VERSION   REPLACES   PHASE
<operator-name>.v25.10.1              <Display>      25.10.1              Succeeded
```

**Check operator Deployment:**

```bash
oc get deployment -n <operator-namespace>
```

The readiness gate checks both conditions — if you used `--readiness-hooks`, the bundle already waited for these before applying CRs.

### 5. Monitor Component Rollout

After CRs are applied, monitor the operator workloads in the respective namespace:

```bash
watch oc get pods -n <operator-namespace>
```

Pods should reach Running or Completed status. The specific set of pods depends on the operator and the CR configuration (e.g., the GPU Operator creates DaemonSets for driver, toolkit, device plugin, DCGM, and related components).

### 6. Capture Snapshot

After deployment, capture a snapshot of the cluster state for validation or record-keeping:

```bash
aicr snapshot --output snapshot.yaml
```

**Expected Output:**

```text
[cli] deploying agent: namespace=default
[cli] agent deployed successfully
[cli] waiting for Job completion: job=aicr timeout=5m0s
[cli] job completed successfully
[cli] snapshot saved to file: path=snapshot.yaml
```

### 7. Validate Deployment

Validate the deployed components against the recipe and snapshot:

```bash
aicr validate \
  --recipe recipe.yaml \
  --snapshot snapshot.yaml
```

The OCP overlay defines validation checks for both deployment and conformance phases. These checks verify operator health, expected CRDs and resources, and GPU driver functionality. The set of checks is defined in the overlay's `validation` section and grows as new components are added.

## Customization

### Value Overrides

OCP component values can be customized at bundle time using `--set` with the component's override key. Each component in the registry declares a `valueOverrideKeys` entry that determines the `--set` prefix. The general pattern is:

```bash
aicr bundle -r recipe.yaml \
  --set <override-key>:<value-path>=<value> \
  --readiness-hooks \
  -o ./ocp-bundle
```

**OLM phase overrides** control operator installation parameters — subscription channel, catalog source, and approval strategy:

```bash
--set gpuoperatorocpolm:subscription.channel=v25.6
```

**CR phase overrides** control operator behavior — the Custom Resource spec fields that the operator reconciles:

```bash
--set gpuoperatorocp:driver.rdma.enabled=false
```

CR values keys are **flat** (`driver.rdma.enabled`, not `spec.driver.rdma.enabled`) — the template maps them into the ClusterPolicy CR `spec` itself, so an override must not include a `spec.` prefix.

> **Note:** The GPU driver version is **not** overridable on OCP. Unlike the Helm-based GPU Operator, the OCP operator manages the driver via the certified driver container, so `driver.version` is intentionally absent from `gpu-operator-ocp/values.yaml` and a `--set gpuoperatorocp:driver.version=...` override is silently ignored.

Override keys for each component are listed in `recipes/registry.yaml` under `valueOverrideKeys`. CR values keys mirror the upstream Helm chart values for consistency across services — the same knobs appear in OCP CR values as in EKS/AKS/GKE Helm values, even though the underlying mechanism differs (ClusterPolicy CR fields vs. Helm chart values).

### Intent-Specific Overlays

The OCP overlay supports multiple intents (e.g., `training`, `inference`). Intent-specific overlays inherit from the base OCP overlay and apply additional values overrides or components relevant to the workload:

```bash
aicr recipe --service ocp --accelerator h100 --intent training --os rhel
aicr recipe --service ocp --accelerator h100 --intent inference --os rhel
```

Training overlays typically enable features like MIG Manager and GDRCopy for multi-node training workloads. Inference overlays use the base CR values.

The available intents and platforms follow the same overlay inheritance pattern used by other services. Refer to `recipes/overlays/ocp-*.yaml` for the current set of supported combinations.

### Raw Manifest Output

Users who prefer plain Kubernetes manifests over Helm-based deployment can run `helm template` on any emitted folder to produce static YAML with all values resolved:

```bash
helm template <release> ./<bundle-folder> -n <namespace> > manifests.yaml
```

This works outside of AICR with a standard Helm installation and produces manifests suitable for `oc apply -f`.

## Deployer Support

Both phases emit `KindLocalHelm` folders, which all existing deployers handle:

| Deployer | Phase 1 (OLM) | Readiness Gate | Phase 2 (CR) |
|----------|---------------|----------------|--------------|
| Helm | `helm upgrade --install` via `install.sh` | `--wait --timeout` (readiness Job) | `helm upgrade --install` |
| Argo CD | `Application` CR, sync-wave N | `Application` CR, sync-wave N+1 | `Application` CR, sync-wave N+2 |

**Argo CD example:**

```bash
aicr bundle -r recipe.yaml --deployer argocd --readiness-hooks -o ./ocp-bundle
```

Argo CD bundles emit `Application` CRs with sync-wave annotations that enforce the OLM → readiness → CR ordering within Argo CD's sync mechanism.

**Without readiness hooks:**

```bash
aicr bundle -r recipe.yaml -o ./ocp-bundle
```

Without `--readiness-hooks`, OLM and CR charts still deploy but there is no gate between them. The CR may be applied before the operator is ready, which can cause transient errors until the operator catches up.

## Values Structure

All OCP components follow a consistent values structure. Understanding this pattern makes it straightforward to customize any operator, whether currently supported or added in the future.

### OLM Values (Phase 1)

Every `*-ocp-olm` component uses the same values shape to control the OLM Subscription:

```yaml
namespace: <operator-namespace>
subscription:
  name: <subscription-name>
  channel: <olm-channel>
  source: <catalog-source>                 # e.g., certified-operators, redhat-operators
  sourceNamespace: openshift-marketplace
  installPlanApproval: Automatic           # or Manual
operatorGroup:
  name: <operator-group-name>
  targetNamespaces:                        # empty list = AllNamespaces install mode
    - <operator-namespace>
```

### CR Values (Phase 2)

Every `*-ocp` component carries values that define the operator's Custom Resource spec. The structure varies per operator but mirrors the upstream Helm chart values for cross-service consistency:

```yaml
name: <cr-instance-name>
# Operator-specific configuration — flat keys (no `spec.` prefix); the
# template maps them into the CR `spec`. Keys match the upstream Helm chart
# values where applicable, e.g. an override is `--set gpuoperatorocp:driver.rdma.enabled=false`.
driver:
  rdma:
    enabled: true
```

The actual values files live in `recipes/components/<component>/values.yaml`, with optional service/intent-specific overrides named `values-<service>[-<intent>].yaml` (e.g., `values-eks-training.yaml`). Overlays reference these via `valuesFile` in `componentRefs`. The `gpu-operator-ocp` component currently ships only a base `values.yaml`; the `ocp-training` overlay carries no component value overrides.

## Overlay Structure

OCP overlays follow the standard base-plus-overlay inheritance pattern used by all AICR services:

```text
base.yaml → ocp.yaml → ocp-<intent>.yaml → ocp-<intent>-<platform>.yaml
```

The **base OCP overlay** (`ocp.yaml`) declares the OLM/CR component pairs for all operators supported on the platform and disables base components that are either replaced by OLM equivalents or not applicable to OpenShift (e.g., components managed natively by OCP or not yet supported).

**Intent overlays** (e.g., `ocp-training.yaml`) inherit from the base and apply workload-specific values overrides to operator CRs.



This hierarchy mirrors the overlay structure of other services (EKS, GKE, AKS) and supports the same composition mechanisms — base inheritance, intent specialization, and platform extensions.
