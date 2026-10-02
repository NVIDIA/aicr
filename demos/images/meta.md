**Title:** AI Cluster Runtime: Recipe Optimization Complexity
**Style:** Technical data visualization, two equal-width panels side-by-side, metric cards + flow diagrams
**Colors:** NVIDIA Green (#76B900), Slate Grey (#1A1A1A), White, Orange (#FF8C00 for delta callouts)

---

**Section 1: Core Metrics** (Left Panel)
Visual: Vertical stack of 6 stat cards, each with a large bold number and muted subtitle

```
┌─────────────────────────────────────────────┐
│  49  Registered Components                  │
│  Helm and Kustomize charts in the registry  │
├─────────────────────────────────────────────┤
│ 134  Overlay Files                          │
│  Specialization overlays across all         │
│  criteria combinations                      │
├─────────────────────────────────────────────┤
│  45  values.yaml Files                      │
│  Files named exactly values.yaml            │
│  under recipes/components/                  │
├─────────────────────────────────────────────┤
│  5  Criteria Dimensions                     │
│  service, accelerator, intent, os, platform │
├─────────────────────────────────────────────┤
│  23  Max Components per Recipe              │
│  Largest stock recipe                       │
│  (OCP + Inference + NIM)                    │
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
│   (base+wild)│                │              │                   │              │
│ 12 components│                │ +2 components│                   │ gpu-operator  │
│              │                │ aws-ebs-csi  │                   │ overrides:    │
│              │                │ aws-efa      │                   │ CDI           │
└──────────────┘                │              │                   └──────────────┘
                                └──────────────┘                          │
                                                                          ▼
┌──────────────┐  +PLATFORM=    ┌──────────────┐  +ACCELERATOR=    ┌──────────────┐
│   RESOLVED   │◀──────────────│  + UBUNTU    │◀────────────────│  + H100      │
│   RECIPE     │   KUBEFLOW    │              │   H100           │              │
│              │                │ OS kernel    │                   │ +nodewright- │
│ 16 unique    │  +kubeflow-   │ constraint   │                   │ customizations │
│ components   │  trainer      │ >= 6.8       │                   │ behavior     │
│              │               │              │                   │ mutations    │
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
│  16 components        │       │  19 components        │
│                       │       │                       │
│  Unique:              │       │  Unique:              │
│    kubeflow-trainer   │       │    grove              │
│                       │       │    dynamo-platform    │
│  GPU Operator:        │       │    agentgateway-crds  │
│    CDI=true           │       │    agentgateway       │
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
Visual: Horizontal bar chart, 3 bars for the same workload across services
(criteria: `--accelerator h100 --intent training`; GKE additionally needs `--os cos`)

| Service  | Components | Service-Specific Additions / Omissions        |
|----------|------------|-----------------------------------------------|
| EKS      | 15         | aws-efa, aws-ebs-csi-driver, nodewright-customizations |
| GKE/COS  | 15         | gcp-driver-installer, gke-nccl-tcpxo, nodewright-customizations, COS GPU overrides |
| Kind     | 13         | network-operator; no cloud CSI/EFA            |

Caption: "Same intent, different service = different component sets and values"

---

**Design Notes:**
- Do not include "Section" in section titles, just use the title itself
- Flow: Left panel (static metrics) + Right panel (dynamic comparisons)
- Header: Dark bg, "AI Cluster Runtime" bold NVIDIA Green
- Footer: Dark bg, white text
- NVIDIA Green for active/matching/passing elements
- Orange for delta callouts (+N components)
- Grey for muted subtitles and secondary text
- Component names in monospace font
- Numbers large and bold in stat cards
- The product name is "AICR" (AI Cluster Runtime), not "Eidos"
