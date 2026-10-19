**Title (prompt reference only, not rendered in the image):** AI Cluster Runtime: Validated Kubernetes for GPU Infrastructure
**Style:** Modern technical diagram, isometric icons, terminal overlays, left-to-right flow, 2:1 landscape (it sits under a slide header, so wider than 16:9)
**Colors:** NVIDIA Green (#76B900), Slate Grey (#1A1A1A), White

---

**Problem** (left, standalone panel)
Visual: From-behind view of a frustrated gender-ambiguous DevOps engineer at a laptop surrounded by scattered YAML files labeled OS, K8s, GPU, CDI, DRA
Terminal shows a red "Error: version mismatch" popup
Caption: "Config complexity: manual errors, drift, version mismatches"

---

**AI Cluster Runtime Workflow** (4 connected steps with flowing arrows, in a 2 × 2 grid)

**Step 1 - RECIPE**
Icon: Mixing bowl with ingredients
Command: `aicr recipe --service eks --accelerator h100 --intent training --os ubuntu --platform kubeflow --output recipe.yaml`
Visual: Environment criteria (service, accelerator, OS, platform) + an Intent toggle (Training / Inference) → a glowing recipe.yaml
Callouts (three short pills): "Driver 580.173.02", "GPU Operator v26.7.1", "DRA enabled"
Caption: "Generate hardware-specific optimizations for workload intent"

**Step 2 - BUNDLE**
Icon: Shipping box with a GitOps logo
Command: `aicr bundle --recipe recipe.yaml --deployer argocd --repo <gitops-repo-url> --output ./bundle --attest`
Visual: The recipe transforms into deployment-ready artifacts:

```shell
bundle/
├── checksums.txt
├── recipe.yaml
├── 001-cert-manager/
├── 002-gpu-operator/
│   ├── values.yaml
│   └── application.yaml
└── ...
```

Caption: "Create GitOps-ready artifacts with Sigstore attestations"

**Step 3 - DEPLOY**
Icon: Argo CD logo
Visual: the Step 2 bundle committed to a Git repository, then Argo CD syncing it into the cluster. A bundle is generated for one deployer; a small side note lists the others (Helm, Helmfile, Flux) as `--deployer` values that regenerate the bundle, not as consumers of the same `./bundle`
Caption: "Use the open source CD tooling you already run"

**Step 4 - VALIDATE**
Icon: Inspection magnifying glass with a check-mark shield
Command: `aicr validate --recipe recipe.yaml --phase all --output report.json`
Visual: Recipe compared against a fresh snapshot, with green check marks for deployment, performance, and conformance
Caption: "Verify the recipe was correctly reconciled in your cluster"

---

**Outcome** (right, standalone panel)
Visual: Clean Kubernetes cluster
Benefits (icon + two to four words each):
- Deterministic: same input, same output
- Secure: SLSA provenance, SBOM, Sigstore attestations
- Conformant: CNCF AI Conformance verified
- Optimized: tuned for hardware + OS + K8s + workload

---

**Design Notes:**
- Do not include the word "Section" in panel titles; use the titles above
- Flow: Problem (standalone) → 4-step workflow chain → Outcome
- Step order is fixed and must be numbered in the image exactly as above: 1 Recipe (top-left), 2 Bundle (top-right), 3 Deploy (bottom-left), 4 Validate (bottom-right). Validate is the last step; it never precedes Bundle or Deploy
- No header bar, title text, or logo inside the image: the hosting slide carries the title and the conference branding
- `--repo` is the Git repository the bundle will be committed to; Argo CD Applications embed it, and it must be set before `--attest` signs the inventory
- Exactly one command per step, rendered in a terminal overlay; copy the commands above character for character and do not invent flags
- Terminal overlays: light text on the dark panel with strong contrast; dim grey on navy is not acceptable
- Minimum text size: every caption and command must stay legible when the whole image is projected at 1600 px wide. Prefer fewer, larger labels; if a command does not fit on two lines at that size, drop the Step 1 `--os` and `--platform` flags before shrinking the font
- Footer: dark background, white text: "Recipe → Bundle → Deploy → Validate. One recipe, any deployer."
