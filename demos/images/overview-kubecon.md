**Title (prompt reference only, not rendered in the image):** AICR Recipes at a Glance
**Style:** Clean flat-design infographic, 2:1 landscape (it sits under a slide header, so wider than 16:9), three-column content area, minimal text, bold iconography, consistent card panels throughout
**Colors:** NVIDIA Green (#76B900) primary, Slate (#1A1A1A) for the environments and footer bars, White card bodies, Cool Grey (#6B7280) secondary text, Soft Blue (#3B82F6) for component chip borders

---

**Overall Layout**
Three horizontal bands stacked top to bottom. No header bar and no logo: the slide that hosts this image supplies the title.
1. Content area — three equal columns with generous whitespace between them
2. Environments row — dark slate, full width, compact
3. Footer tagline — dark slate, full width

---

**Content Area — Left Column: Recipe Composition**
Column header in NVIDIA Green: "How a Recipe Is Built"

A vertical layer-cake stack of five thin rectangular layers, each slightly narrower than the one below, creating a pyramid silhouette. Each layer is a different shade from dark slate (bottom) to NVIDIA Green (top), with a short white label centered inside:
- Bottom layer (widest, dark slate): "Base defaults"
- Layer 2 (dark grey): "Service  ·  EKS / GKE / AKS / OKE / LKE"
- Layer 3 (medium grey): "Accelerator  ·  H100 / GB200 / GB300"
- Layer 4 (grey-green): "OS  ·  Ubuntu / COS / Oracle Linux"
- Top layer (narrowest, NVIDIA Green): "Intent  ·  Training / Inference"

A small icon of interlocking puzzle pieces floats to the right of the stack with a label: "+ Platform  ·  Kubeflow / Slurm / Kueue / Dynamo / NIM" in grey italic

Below the stack, a thick downward NVIDIA Green arrow pointing to a bundle box icon labeled "GitOps Bundle or OCI Image" with four small deployer chip badges in a row beneath it: "Helm", "Helmfile", "Argo CD", "Flux"

Below the column, a short grey caption: "Same recipe · any deployer"

---

**Content Area — Center Column: Component Ecosystem**
Column header in NVIDIA Green: "What Goes Into a Recipe"

Four rows of component chip badges (rounded rectangle, white background, soft blue border, dark text). Chips are grouped by a thin colored left-edge bar:

Row 1 — Blue left edge (label "Node & Platform"):
"Nodewright" · "Network Operator" · "NFD" · "cert-manager" · "Node Problem Detector"

Row 2 — NVIDIA Green left edge (label "GPU Stack"):
"GPU Operator" · "DRA Driver" · "DRA Node Labeler" · "NVSentinel"

Row 3 — Purple left edge (label "Observability & Storage"):
"kube-prometheus-stack" · "Prometheus Adapter" · "Ephemeral Storage Metrics" · "EBS CSI"

Row 4 — Amber left edge (label "Workload Platforms"):
"Kubeflow Trainer" · "Slinky (Slurm)" · "Kueue" · "Dynamo" · "NIM Operator" · "KAI Scheduler" · "Grove" · "Topograph"

Each chip row has a small version-lock padlock icon to its far right in grey, suggesting pinned versions.

Below the chips, a short NVIDIA Green callout pill: "Versions pinned · SBOM generated"

Short grey caption below: "Curated, version-locked, validated together"

---

**Content Area — Right Column: Value Delivered**
Column header in NVIDIA Green: "What You Get"

Six compact cards stacked vertically, each with identical structure: icon left, short title bold, one-line description in grey. No colored edge bars on the cards.

Card 1 — icon: speedometer gauge
**Optimized** — Tuned for hardware × OS × workload × service

Card 2 — icon: circular arrows (reproducibility)
**Reproducible** — Same inputs → identical, version-pinned, checksummed artifacts

Card 3 — icon: checklist with magnifying glass
**Validated** — Constraint, deployment, performance, and conformance checks

Card 4 — icon: chain link with shield
**Trusted** — SLSA provenance (Build L3 images) · Sigstore attestations · SBOM

Card 5 — icon: magnifying glass over a document
**Auditable** — query, diff, upgrade-check, signed evidence

Card 6 — icon: plug connecting to pipeline
**GitOps-native** — Drop into your existing Helm, Argo CD, or Flux workflow

---

**Environments Row**
Light grey bar (#D1D5DB), full width. Left-aligned dark slate label: "Supported Environments · values with a shipped recipe". Then five equal-width groups in a single horizontal line, separated by thin vertical dividers. Each group has: a small dark grey label above, pill badges on one line below, and "and more…" in grey italic on a second line below the badges where noted. No group wraps onto a third line — size badges small enough to guarantee single-line fit.
- Services: badges "EKS · GKE · AKS · OKE · LKE" then "+ OpenShift, RKE2, BCM, k0s…" below
- GPUs: badges "H100 · GB200 · GB300 · B200" then "and more…" below
- OS: badges "Ubuntu · COS · Oracle Linux" (no second line)
- Intent: badges "Training · Inference" (no second line)
- Platform: badges "Kubeflow · Slurm · Kueue · Dynamo · NIM" (no second line)

---

**Footer Tagline**
Dark slate, centered white text: "You bring the cluster and your deployment tooling — AICR generates the configuration your tools deploy."

---

**Design Notes**
- No code blocks, no CLI commands, no ASCII art — all concepts expressed as icons, badges, and labels of five words or fewer
- No header bar, title, or logo inside the image: the hosting slide carries "AICR recipes at a glance" and the conference branding
- The three-column layout has a clear left-to-right reading: inputs (how it's built) → ingredients (what goes in) → outputs (what you get)
- Layer-cake in the left column is the visual anchor of the whole piece; use smooth color graduation from dark slate at the base to NVIDIA Green at the top
- Component chips in the center column are the densest element; keep chip text small and rows tightly spaced — this is the "ingredient list" feel. Chip names must match the list above exactly; do not invent components
- The six value cards in the right column are the payoff; trust and auditability sit beside the others as equals, not as the headline
- Environments row lists only values that have at least one shipped recipe overlay (`recipes/overlays/`); values the CLI accepts but no recipe provides (RHEL, Amazon Linux, Talos, Run:ai) are deliberately omitted
- Environments row uses pill badges not icons; keep them small so the row reads as a compact reference strip, not a feature callout
- Minimum text size: every label must stay legible when the whole image is projected at 1600 px wide; prefer fewer, larger labels over more, smaller ones
- Generous whitespace between columns; do not let chips or cards touch the column boundaries
- Overall tone: authoritative, not salesy — the visual density of the component list and the environment matrix conveys breadth without needing large text claims
