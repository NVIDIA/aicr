**Title:** AI Cluster Runtime: Validated Kubernetes for GPU Infrastructure
**Style:** Modern technical diagram, isometric icons, terminal overlays, left-to-right flow
**Colors:** NVIDIA Green (#76B900), Slate Grey (#1A1A1A), White

---

**Section 1: Problem**
Visual: From-behind view of a frustrated gender-ambiguous DevOps engineer at laptop surrounded by scattered YAML files (OS, K8s, GPU, CDI, DRA)
Terminal shows red "Error: version mismatch" popup
Caption: "Config complexity: manual errors, drift, version mismatches"

---

**Section 2: AI Cluster Runtime Workflow** (4 connected steps with flowing arrows)

**Step 1 - RECIPE**  
Icon: Mixing bowl with ingredients
Command: `aicr recipe -a h100 -i training -p kubeflow -o recipe.yaml`
Visual: Cluster Snapshot + Intent toggle (Training/Inference) → glowing recipe.yaml
Callouts: Driver 580.82.07, Device Plugin v0.17.4, CDI Enabled
Caption: "Generate hardware-specific optimizations for workload intent"

**Step 2 - BUNDLE**
Icon: Shipping box with GitOps logo
Command: `aicr bundle -f recipe.yaml -d argocd -o ./bundles`
Visual: Recipe transforms into deployment-ready artifacts:

```shell
bundles/
├── app-of-apps.yaml
├── gpu-operator/
│ ├── values.yaml
│ ├── manifests/
│ └── argocd/application.yaml
└── network-operator/
└── ...
```

Caption: "Create GitOps-ready artifacts with Cosign attestations"

**Step 3 - DEPLOY**
Icon: Argo CD and Helm 
Visual: GitOps tooling syncing artifacts from Step 3 into the cluster
Caption: "Use existing OSS tooling to deploy to your cluster"

**Step 4 - VALIDATE**
Icon: Inspection magnifying glass with Checkmark shield
Command: `aicr validate -r recipe.yaml -p deployment -p conformance -p performance`
Visual: Recipe compared against snapshot with green checkmarks
Caption: "Verify recipe was correctly reconciled in your cluster"

---

**Section 3: Outcome**
Visual: Clean Kubernetes cluster
Benefits (icons):
- Deterministic: Same input = same output
- Secure: SLSA L3, SBOM, Cosign attestations artifacts  
- Conformant: CNCF AI Conformance verification 
- Optimized: Tuned for hardware + os + k8s + use-case combo

---

**Design Notes:**
- Do not include "Section" in section titles, just use the title itself
- Flow: Problem (standalone) → 4-step workflow chain → Outcome
- Header: Dark bg, "AI Cluster Runtime" bold NVIDIA Green
- Footer: Dark bg, white text
- Emphasize: Step outputs feed next step (recipe→bundle→deploy→validate)
- All artifacts are signed/attested
  