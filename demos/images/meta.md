**Title:** AI Cluster Runtime: Recipe Optimization Complexity
**Style:** Technical data visualization, two equal-width panels side-by-side, metric cards + flow diagrams
**Colors:** NVIDIA Green (#76B900), Slate Grey (#1A1A1A), White, Orange (#FF8C00 for delta callouts)

---

**Section 1: Core Metrics** (Left Panel)
Visual: Vertical stack of 6 stat cards, each with a large bold number and muted subtitle

```
┌─────────────────────────────────────────────┐
│  18  Registered Components                  │
│  Helm and Kustomize charts in the registry  │
├─────────────────────────────────────────────┤
│  23  Overlay Layers                         │
│  Specialization overlays across all         │
│  criteria combinations                      │
├─────────────────────────────────────────────┤
│  427  Config Values                         │
│  Total leaf configuration values across     │
│  all component value files                  │
├─────────────────────────────────────────────┤
│  5  Criteria Dimensions                     │
│  service, accelerator, intent, os, platform │
├─────────────────────────────────────────────┤
│  15  Max Components per Recipe              │
│  Most specialized inference recipe          │
│  (H100 + EKS + Dynamo)                     │
├─────────────────────────────────────────────┤
│  6  Overlay Chain Depth                     │
│  base > service > intent > accelerator      │
│  > os > platform                            │
└─────────────────────────────────────────────┘
```

Caption: "Small input criteria, high-cardinality output configurations"

---

**Section 2: Specificity Ladder** (Right Panel, Top)
Visual: Left-to-right horizontal pipeline, boxes connected by labeled arrows

```
┌──────────────┐  +SERVICE=EKS  ┌──────────────┐  +INTENT=TRAINING  ┌──────────────┐
│   GENERIC    │───────────────▶│   + EKS      │──────────────────▶│  + TRAINING  │
│   (Base)     │                │              │                   │              │
│ 9 components │                │ +2 components│                   │ gpu-operator  │
│ 427 values   │                │ aws-ebs-csi  │                   │ overrides:    │
│              │                │ aws-efa      │                   │ CDI, gdrcopy  │
└──────────────┘                │ +70 values   │                   └──────────────┘
                                └──────────────┘                          │
                                                                          ▼
┌──────────────┐  +PLATFORM=    ┌──────────────┐  +ACCELERATOR=    ┌──────────────┐
│   RESOLVED   │◀──────────────│  + UBUNTU    │◀────────────────│  + H100      │
│   RECIPE     │   KUBEFLOW    │              │   H100           │              │
│              │                │ OS kernel    │                   │ +skyhook-    │
│ 12 unique    │  +kubeflow-   │ constraint   │                   │ customizatns │
│ components   │  trainer      │ >= 6.8       │                   │ behavior     │
│              │  +36 values   │              │                   │ mutations    │
└──────────────┘                └──────────────┘                   └──────────────┘
```

Caption: "Each criteria dimension adds, overrides, or mutates configuration"

---

**Section 3: Training vs Inference** (Right Panel, Middle)
Visual: Single input forking into two divergent paths

```
                    ┌─────────────────────┐
                    │  H100 + EKS + Ubuntu │
                    └─────────┬───────────┘
                              │
              ┌───────────────┴───────────────┐
              ▼                               ▼
┌───────────────────────┐       ┌───────────────────────┐
│  TRAINING (Kubeflow)  │       │  INFERENCE (Dynamo)   │
│                       │       │                       │
│  12 components        │       │  15 components        │
│                       │       │                       │
│  Unique:              │       │  Unique:              │
│    kubeflow-trainer   │       │    dynamo-crds        │
│                       │       │    dynamo-platform    │
│  GPU Operator:        │       │    kgateway-crds      │
│    CDI=true           │       │    kgateway           │
│    gdrcopy=true       │       │                       │
│                       │       │  DRA driver:          │
│                       │       │    gpuResources=true  │
└───────────────────────┘       └───────────────────────┘
```

Callouts:
- Switching intent: +4 components gained, -1 lost
- Completely different deployment stacks from a single criteria change

Caption: "intent=training vs intent=inference produces divergent component graphs"

---

**Section 4: Cross-Service Comparison** (Right Panel, Bottom)
Visual: Horizontal bar chart, 3 bars for same generic workload across services

| Service  | Components | Service-Specific Additions            |
|----------|------------|---------------------------------------|
| EKS      | 14         | aws-efa, aws-ebs-csi-driver           |
| GKE/COS  | 10         | COS-specific GPU Operator overrides   |
| Kind     | 16         | network-operator, DRA driver, local   |

Caption: "Same intent, different service = different component sets and values"

---

**Design Notes:**
- Do not include "Section" in section titles, just use the title itself
- Flow: Left panel (static metrics) + Right panel (dynamic comparisons)
- Header: Dark bg, "AI Cluster Runtime" bold NVIDIA Green
- Footer: Dark bg, white text
- NVIDIA Green for active/matching/passing elements
- Orange for delta callouts (+N components, +N values)
- Grey for muted subtitles and secondary text
- Component names in monospace font
- Numbers large and bold in stat cards
- The product name is "AICR" (AI Cluster Runtime), not "Eidos"
