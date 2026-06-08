**Title:** Recipes You Can Trust
**Style:** Clean flat-design infographic, 16:9 landscape, consistent card panels, minimal text, bold iconography
**Colors:** NVIDIA Green (#76B900) for success/verified, Slate (#1A1A1A) backgrounds, White text, Amber (#F5A623) for warnings, Soft Red (#E53935) for failures

---

**Overall Layout**
Three horizontal bands stacked top to bottom:
1. Header bar (dark slate, full width)
2. Content area: one Problem card on the left, three Trust Layer cards center-right (equal width, side by side)
3. Outcome bar (dark slate, full width)

---

**Header Bar**
Dark slate background. Left-aligned: bold NVIDIA Green text "Recipes You Can Trust". Below it, smaller white text: "SLSA  ·  Sigstore  ·  Rekor". Right side: NVIDIA logo mark in white.

---

**Content Area — left third: Problem Card**
Light grey background card with a thin red border.
Top label in red: "The Problem"
Center: simple icon of a Kubernetes cluster (hexagon with wheel) with a large amber question mark overlaid.
Below the icon, three short lines in dark text, each preceded by a red X icon:
- "Who built this?"
- "Was it tampered with?"
- "Validated on real hardware?"
No code, no commands — just the three questions and icons.

---

**Content Area — right two-thirds: Three Trust Layer Cards**
Three equal-width cards side by side, connected by right-pointing NVIDIA Green arrows between them. Each card has a dark slate header and white body.

**Card 1 — Binary Provenance**
Header (dark slate): white label "1 · Binary Provenance"
Body (white):
- Large center icon: NVIDIA CI badge (shield with NVIDIA logo) with a small Sigstore flame overlaid in the bottom-right corner
- Below icon: two short green pill badges stacked — "SLSA Provenance v1" and "SPDX SBOM"
- Below badges: small padlock icon + short text "Signed by NVIDIA CI · Logged in Rekor"
- Bottom caption in grey italic: "Every release binary and image is signed"

**Card 2 — Bundle Attestation**
Header (dark slate): white label "2 · Bundle Attestation"
Body (white):
- Large center icon: a shipping box with a green Sigstore seal stamp on its face
- Below icon: a horizontal trust-level bar with four segments left to right — "unknown" (red), "unverified" (amber), "attested" (yellow-green), "verified" (NVIDIA green) — the "verified" segment is highlighted larger
- Below bar: small shield-check icon + short text "Checksums pinned · Tamper = instant failure"
- Bottom caption in grey italic: "Every deployment artifact is signed and verifiable"

**Card 3 — Recipe Evidence**
Header (dark slate): white label "3 · Recipe Evidence"
Body (white):
- Large center icon: a cluster node (Kubernetes wheel) on the left connected by a short arrow to an OCI cylinder in the center, connected by a short arrow to a maintainer figure (person silhouette) on the right — all three small and evenly spaced
- Below icon: small green checkmark icon + short text "Validated on real GPU hardware · OCI-signed"
- A tiny pointer-file chip in NVIDIA green: "pointer.yaml → sha256:f0c1…"
- Bottom caption in grey italic: "Proof of validation — verifiable offline by any maintainer"

---

**Outcome Bar**
Dark slate background, full width.
Left side: three interlocking chain link icons labeled "Binary", "Bundle", "Evidence" in white — chain rendered in NVIDIA Green.
Center: large white right-pointing arrow.
Right side: Kubernetes cluster icon in NVIDIA Green with a green shield-check badge overlaid.
Far right: three short white lines each with a green checkmark:
- "Origin verified"
- "Integrity guaranteed"
- "Validation proven"
Short white footer text centered below: "From NVIDIA CI to your cluster — cryptographically traceable, tamper-evident, independently verifiable"

---

**Design Notes**
- No code blocks, no commands, no ASCII art — all technical detail expressed as icons, badges, and short labels (five words or fewer per label)
- Each Trust Layer card uses identical structure: dark header → center icon → badge/indicator → short caption — visual consistency is the priority
- Arrows between the three Trust Layer cards are thick NVIDIA Green with a subtle glow to emphasize the chain metaphor
- Trust-level bar in Card 2 is the single most detailed element; keep all other cards simpler
- Problem card intentionally uses muted colors (grey, red) to contrast with the vibrant Trust Layer cards
- Chain links in the Outcome bar should be visually bold — this is the payoff moment
- Maintain generous whitespace inside each card; do not crowd elements
