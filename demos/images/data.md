**Title:** AI Cluster Runtime: Recipe Data Pipeline
**Style:** Modern technical diagram, clean lines, left-to-right horizontal flow, three-stage pipeline with transformation arrows
**Colors:** NVIDIA Green (#76B900), Slate Grey (#1A1A1A), White

---

**Section 1: Metadata**
Visual: Stack of layered YAML document icons (5-7 semi-transparent overlapping sheets), representing numerous overlay files
Small icons/badges on sheets: GPU, Network, Driver, K8s
Text snippets visible: service: eks, accelerator: h100, intent: training
Caption: "Numerous YAML overlays defining component-specific optimizations"

---

**Section 2: Transformation** (2 connected transitions with flowing arrows)

**Transition 1 - Metadata to Recipe**
Visual: Funnel or merge arrow showing many-to-one transformation
Icons: Filter icon, merge branches icon
Caption: "Criteria matching and overlay merge"

**Transition 2 - Recipe to Bundle**
Visual: Expansion/explosion arrow showing one-to-many transformation
Icons: Gear/build icon, folder creation icon
Caption: "Materialization and artifact generation"

---

**Section 3: Recipe**
Visual: Single consolidated YAML document with structured sections visible
Visible structure sections: criteria:, componentRefs:, constraints:
Component list preview: gpu-operator, network-operator, cert-manager
Version badges: v25.3.3, v25.4.0
Caption: "Single YAML response with optimal configurations for runtime criteria"

---

**Section 4: Bundle**
Visual: File system tree structure showing multiple component folders

```shell
bundles/
├── gpu-operator/
│   ├── checksums.txt
│   ├── values.yaml
│   └── scripts/install.sh
├── network-operator/
│   ├── checksums.txt
│   └── argocd/application.yaml
└── cert-manager/
    └── ...
```

Folder icons with component names
Deployer badges: "Script", "Argo CD", "Flux"
Caption: "Per-component folders with deployer-specific artifacts"

---

**Section 5: Outcome**
Visual: Three small boxes summarizing the pipeline
Benefits (icons):
- Input: System criteria (service, GPU, OS, intent)
- Process: Match, Merge, Generate
- Output: Helm values, manifests, scripts, GitOps configs

Visual Flow Summary:

```shell
[Many Overlays] ──funnel──▶ [Single Recipe] ──expand──▶ [Multiple Bundles]
   (N files)                  (1 file)                  (N folders)
```

---

**Design Notes:**
- Flow: Metadata (many overlays) → Recipe (single file) → Bundle (many folders)
- Header: Dark bg, "AI Cluster Runtime" bold NVIDIA Green
- Footer: Dark bg, white text
- Emphasize the "single source of truth" nature of the Recipe stage
- Show data volume: many → one → many transformation
- Show checksum verification as a security/integrity feature
- Include subtle Kubernetes/cloud-native iconography
