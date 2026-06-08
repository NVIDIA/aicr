**Title:** AICR Recipes: Optimized · Validated · Trusted
**Style:** Clean flat-design infographic, 16:9 landscape, three-column content area, minimal text, bold iconography, consistent card panels throughout
**Colors:** NVIDIA Green (#76B900) primary, Slate (#1A1A1A) card headers and background bars, White card bodies, Cool Grey (#6B7280) secondary text, Soft Blue (#3B82F6) for component chip borders

---

**Overall Layout**
Four horizontal bands stacked top to bottom:
1. Header bar — dark slate, full width
2. Content area — three equal columns with generous whitespace between them
3. Environments row — dark slate, full width, compact
4. Footer tagline — dark slate, full width

---

**Header Bar**
Dark slate background. Center-aligned bold white text: "AICR Recipes". Below it, three short NVIDIA Green pill badges side by side: "Optimized" · "Validated" · "Trusted". Right side: NVIDIA logo mark in white. Left side: small subtitle in grey italic: "GPU-accelerated Kubernetes, without the guesswork"

---

**Content Area — Left Column: Recipe Composition**
Column header in NVIDIA Green: "How a Recipe Is Built"

A vertical layer-cake stack of five thin rectangular layers, each slightly narrower than the one below, creating a pyramid silhouette. Each layer is a different shade from dark slate (bottom) to NVIDIA Green (top), with a short white label centered inside:
- Bottom layer (widest, dark slate): "Base defaults"
- Layer 2 (dark grey): "Cloud  ·  EKS / AKS / GKE"
- Layer 3 (medium grey): "Accelerator  ·  H100 / GB200"
- Layer 4 (grey-green): "OS  ·  Ubuntu / RHEL / Talos"
- Top layer (narrowest, NVIDIA Green): "Intent  ·  Training / Inference"

A small icon of interlocking puzzle pieces floats to the right of the stack with a label: "+ Mixins" in grey italic (platform fragments: Kubeflow, Dynamo, Slurm)

Below the stack, a thick downward NVIDIA Green arrow pointing to a bundle box icon labeled "GitOps Bundle or OCI Image" with four small deployer chip badges in a row beneath it: "Helm", "Argo CD", "Flux", "Helmfile"

Below the column, a short grey caption: "Same recipe · any deployer"

---

**Content Area — Center Column: Component Ecosystem**
Column header in NVIDIA Green: "What Goes Into a Recipe"

Two rows of component chip badges (rounded rectangle, white background, soft blue border, dark text). Chips are grouped by a thin colored left-edge bar:

Row 1 — Blue left edge (label "Node & Platform"):
"Nodewright" · "Network Operator" · "NFD" · "cert-manager"

Row 2 — NVIDIA Green left edge (label "GPU Stack"):
"GPU Operator" · "Device Plugin" · "Container Toolkit" · "DCGM" · "NVSentinel"

Row 3 — Purple left edge (label "Observability & Storage"):
"Prometheus" · "DCGM Exporter" · "AIStore"

Row 4 — Amber left edge (label "Workload Platforms"):
"Kubeflow" · "Dynamo" · "NIM Operator" · "Slinky" · "KAI Scheduler"

Each chip row has a small version-lock padlock icon to its far right in grey, suggesting pinned versions.

Below the chips, a short NVIDIA Green callout pill: "Version/digest pinned · SBOM generated"

Short grey caption below: "Curated, version-locked, validated together"

---

**Content Area — Right Column: Value Delivered**
Column header in NVIDIA Green: "What You Get"

Five compact cards stacked vertically, each with identical structure: dark slate left-edge accent bar, icon left, short title bold, one-line description in grey.

Card 1 — accent bar NVIDIA Green, icon: speedometer gauge
**Optimized** — Tuned for hardware × OS × workload × cloud

Card 2 — accent bar NVIDIA Green, icon: circular arrows (reproducibility)
**Reproducible** — Same inputs → identical deployments, every time

Card 3 — accent bar NVIDIA Green, icon: checklist with magnifying glass
**Validated** — Constraint, deployment, perf, and conformance checks

Card 4 — accent bar NVIDIA Green, icon: chain link with shield
**Trusted** — SLSA L3 provenance · Sigstore attestations · SBOM

Card 5 — accent bar NVIDIA Green, icon: plug connecting to pipeline
**GitOps-native** — Drop into your existing Helm, Argo CD, or Flux workflow

---

**Environments Row**
Light grey bar (#D1D5DB), full width. Left-aligned dark slate label: "Supported Environments". Then five equal-width groups in a single horizontal line, separated by thin vertical dividers. Each group has: a small dark grey label above, pill badges on one line below, and "and more…" in grey italic on a second line below the badges. No group wraps onto a third line — size badges small enough to guarantee single-line fit.
- Clouds: badges "AKS · BCM · EKS · GKE" then "and more…" below
- GPUs: badges "H100 · L40 · GB200" then "and more…" below
- OS: badges "Ubuntu · RHEL · Talos" then "and more…" below
- Intent: badges "Training · Inference" then "and more…" below
- Platform: badges "Kubeflow · NIM · Slurm" (all three on one line, no wrapping) then "and more…" below

---

**Footer Tagline**
Dark slate, centered white text: "You bring the cluster and your deployment tooling — AICR generates the configuration your tools deploy."

---

**Design Notes**
- No code blocks, no CLI commands, no ASCII art — all concepts expressed as icons, badges, and labels of five words or fewer
- The three-column layout has a clear left-to-right reading: inputs (how it's built) → ingredients (what goes in) → outputs (what you get)
- Layer-cake in the left column is the visual anchor of the whole piece; use smooth color graduation from dark slate at the base to NVIDIA Green at the top
- Component chips in the center column are the densest element; keep chip text small and rows tightly spaced — this is the "ingredient list" feel
- The five value cards in the right column are the payoff; trust is card 4 of 5, deliberately not the headline — it is one property among five equals
- Environments row uses pill badges not icons; keep them small so the row reads as a compact reference strip, not a feature callout
- Generous whitespace between columns; do not let chips or cards touch the column boundaries
- Overall tone: authoritative, not salesy — the visual density of the component list and the environment matrix conveys breadth without needing large text claims
