**Title:** AI Cluster Runtime: Asymmetric Metadata Rule Engine
**Style:** Technical architecture diagram, graph-theory inspired, two equal-width panels side-by-side, layered depth visualization
**Colors:** NVIDIA Green (#76B900), Slate Grey (#1A1A1A), White, Orange (#FF8C00 for warnings/highlights)

---

**Section 1: Overlay Inheritance Graph** (Left Panel)
Visual: Directed acyclic graph (DAG) showing overlay inheritance relationships
Each node represents an overlay file with its criteria and inheritance

Inheritance hierarchy (top to bottom):

```
                    ┌─────────────────────────────────────┐
                    │           base.yaml                 │
                    │  criteria: { }  (empty = wildcard)  │
                    │  base: (none)                       │
                    │  Specificity: 0 fields              │
                    └─────────────────────────────────────┘
                                     │
            ┌────────────────────────┼────────────────────────┐
            │                        │                        │
            ▼                        ▼                        ▼
┌───────────────────────┐ ┌───────────────────────┐ ┌───────────────────────┐
│      eks.yaml         │ │      gke.yaml         │ │      aks.yaml         │
│  criteria:            │ │  criteria:            │ │  criteria:            │
│    service: eks       │ │    service: gke       │ │    service: aks       │
│  Specificity: 1 field │ │  Specificity: 1 field │ │  Specificity: 1 field │
└───────────────────────┘ └───────────────────────┘ └───────────────────────┘
            │
            ▼
┌─────────────────────────────────────┐
│        eks-training.yaml            │
│  criteria:                          │
│    service: eks                     │
│    intent: training                 │
│  base: eks  (explicit inheritance)  │
│  Specificity: 2 fields             │
└─────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────────────────┐
│      gb200-eks-ubuntu-training.yaml             │
│  criteria:                                      │
│    service: eks                                 │
│    accelerator: gb200                           │
│    os: ubuntu                                   │
│    intent: training                             │
│  base: eks-training (→ eks → base)              │
│  Specificity: 4 fields (most specific)          │
└─────────────────────────────────────────────────┘
```

Callouts:
- Node size/prominence increases with specificity
- Edges labeled with "base:" to show explicit inheritance
- base.yaml at top (root), most specific overlays at bottom (leaves)

Caption: "Overlay composition: inheritance chains + criteria specificity"

---

**Section 2: Key Principles**

**Inheritance Chain Resolution:**
When overlay "gb200-eks-ubuntu-training" is selected:
1. Resolve chain: gb200-eks-ubuntu-training → eks-training → eks → base
2. Apply in order: base → eks → eks-training → gb200-eks-ubuntu-training
3. Each level overrides/augments previous (deep merge)
Benefit: Eliminates duplication, common settings inherited automatically

**Asymmetric Matching:**
- Empty field in criteria = WILDCARD (matches any value)
- Empty field in query = NO MATCH (field not provided)
- Criteria fields: service, accelerator, os, intent, nodes
- Overlays SELECT queries, queries don't select overlays

Caption: "overlay.criteria.IsMatch(query) is not equal to query.IsMatch(overlay.criteria)"

---

**Section 3: Query Evaluation Cascade** (Right Panel)
Visual: 5-step vertical flow showing query processing with inheritance

**Step 1 - MATCH**
Command: `aicr recipe --service eks --accelerator gb200 --os ubuntu --intent training`
Visual: User query evaluated against each overlay's criteria

| Overlay | Match Result |
|---|---|
| base.yaml (empty criteria) | MATCH |
| eks.yaml (service: eks) | MATCH |
| gke.yaml (service: gke) | SKIP - criteria mismatch |
| eks-training.yaml (service+intent) | MATCH |
| h100-eks-ubuntu-training.yaml (accelerator: h100) | SKIP - accelerator mismatch |
| gb200-eks-ubuntu-training.yaml (all 4 fields) | MATCH |

Caption: "Find all overlays whose criteria match the query"

**Step 2 - SORT**
Visual: Matched overlays sorted by specificity (least to most)
1. base.yaml (0 fields)
2. eks.yaml (1 field)
3. eks-training.yaml (2 fields)
4. gb200-eks-ubuntu-training.yaml (4 fields)
Caption: "Fewer populated criteria = lower specificity = applied first"

**Step 3 - INHERIT**
Visual: Inheritance chain resolved and deep-merged for each overlay
Chain: base → eks → eks-training → gb200-eks-ubuntu-training
Caption: "Resolve inheritance chains and deep merge values"

**Step 4 - VALIDATE** (when snapshot provided)
Visual: Constraints evaluated against snapshot measurements

| Constraint | Expected | Actual | Result |
|---|---|---|---|
| K8s.server.version >= 1.28 | >=1.28 | 1.31 | PASS |
| OS.release.ID == ubuntu | ubuntu | ubuntu | PASS |
| GPU.driver.version >= 550.0 | >=550.0 | 570.86 | PASS |
| GPU.device.mig_enabled == true | true | false | WARN |

Caption: "Evaluate constraints against snapshot (warnings added to recipe metadata)"

**Step 5 - OUTPUT**
Visual: Final recipe YAML with all accumulated results
Sections: apiVersion, kind, metadata (appliedOverlays, constraintWarnings), criteria, componentRefs, deploymentOrder, constraints
Caption: "Complete recipe with all inherited and overlay-specific values"

---

**Section 4: Outcome**
Visual: Formula reference box

```
MATCH(overlay, query) = for all field f in overlay.criteria:
                          (f is empty) or (f == query[f])

INHERITANCE_CHAIN(overlay) = [overlay] + INHERITANCE_CHAIN(overlay.base)

FINAL_RECIPE = MERGE( for o in sorted(matched_overlays, by=specificity):
                        RESOLVE_CHAIN(o) )

where MERGE = deep merge (child values override parent for shared keys)
```

Caption: "Deterministic recipe resolution from overlay metadata"

---

**Design Notes:**
- Do not include "Section" in section titles, just use the title itself
- Flow: Left panel (static structure) + Right panel (dynamic behavior)
- Header: Dark bg, "AI Cluster Runtime" bold NVIDIA Green
- Footer: Dark bg, white text
- NVIDIA Green for active/matching/passing elements
- Orange for warnings (constraint issues)
- Grey for skipped/non-matching elements
- Clear visual hierarchy with numbered steps in the evaluation cascade
