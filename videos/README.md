# AICR videos

The 60-second series covers cluster discovery, GPU stack deployment, validation, and workload operations. The release examples use AICR v1.3.0. Operator preview and unreleased examples are labeled.

This is our first pass at the series, and some details may be inaccurate. Corrections and suggestions are welcome on [GitHub Issues](https://github.com/NVIDIA/aicr/issues).

Follow the [local rebuild guide](./REBUILD.md) to render a video by episode number or review the series after a release. Each episode links to its editable source.

## Opening episodes

- [**01 · AICR Overview**](episodes/01.json) — Follow the GPU chip mascot from cluster configuration to a recipe, deployment bundle, and validated GPU workload. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/README.md#how-it-works)
- [**02 · Recipes You Can Trust**](episodes/02.json) — Trace a bundle creator to the AICR release, see a one-character change fail verification, and examine signed results from real clusters. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/artifact-verification.md)

## Core workflow

- [**03 · What’s running in my GPU cluster?**](episodes/03.json) — Replace an incomplete handover with a reusable snapshot that records the GPU, node software, and installed components. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/agent-deployment.md)
- [**04 · How AICR picks a recipe**](episodes/04.json) — Change the intent from training to inference and see a distinct resolved recipe for the same hardware. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/data-flow.md#recipe-input-options)
- [**05 · One query, two clusters**](episodes/05.json) — Hold the query selector fixed and see it return different kernel-module settings for two real recipe targets. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/demos/query.md#driver-config-differs-by-accelerator)
- [**06 · One recipe, seven deployment formats**](episodes/06.json) — Keep one resolved recipe while generating deployer-specific artifacts and handing Argo CD the format it expects. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/bundling.md#choose-a-deployer)
- [**07 · Will this cluster run my recipe?**](episodes/07.json) — Watch an illustrative kernel prerequisite fail before validator Jobs run, then read a phase-specific result after deployment. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/validation.md#when-to-validate)
- [**08 · What does the AICR Operator do?**](episodes/08.json) — Trace `ClusterRuntime` intent through a pinned AICR release, an ordered Argo CD rollout, and deployment evidence. **Operator preview.** [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/operator/index.md)
- [**09 · Put AICR inside your Go app**](episodes/09.json) — Follow one typed platform request through `ResolveRecipe` and `MakeBundle` to deployer artifacts returned by a Go app. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/go-library.md#quick-start)
- [**10 · What changed since last week?**](episodes/10.json) — Use a saved baseline and fresh snapshot to identify one changed kernel reading and flag drift in CI. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/cli-reference.md#aicr-diff)
- [**11 · Can I make this upgrade in one step?**](episodes/11.json) — Compare the deployed stack with a proposed recipe and use a blocked transition to decide what to assess separately. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/upgrading.md)
- [**12 · From cluster checks to a CNCF submission**](episodes/12.json) — Run version-matched conformance checks and review the generated package for a specific cluster submission. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/conformance/cncf/index.md)

## Recipes and deployment

- [**13 · Build a recipe in layers**](episodes/13.json) — Trace a setting through recipe overlays and a reusable mixin, then use the resolved inheritance chain to identify its source. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/recipe-development.md#multi-level-inheritance)
- [**14 · Who owns the GPU driver?**](episodes/14.json) — Choose a GKE GPU stack profile by identifying who provides the driver and who advertises whole GPUs. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/gke-gpu-setup.md)
- [**15 · Use your own catalog**](episodes/15.json) — Validate a private catalog and carry one custom runtime component from an overlay into the generated bundle. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/data-extension.md)
- [**16 · Bring your own release artifacts**](episodes/16.json) — Bind an exact chart and image release into the selected recipe, then inspect its recorded artifact provenance. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/artifact-inventory.md)
- [**17 · One bundle, two owners**](episodes/17.json) — Split a Helm deployment into two ordered handoffs while retaining a single root for bundle verification. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/bundling.md#split-a-bundle-by-class)
- [**18 · Supply secrets at install time**](episodes/18.json) — Show a required bootstrap-token slot moving into an install-time values file while the generated bundle stays free of the token. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/bundling.md#required-and-install-time-secret-inputs)
- [**19 · Deploy behind an air gap**](episodes/19.json) — Build a reachable package for an isolated cluster by discovering, mirroring, and vendoring the recipe’s actual dependencies. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/air-gap-mirror.md)

## Validation, evidence, and trust

- [**20 · Are the GPUs using the fast link?**](episodes/20.json) — See a completed training benchmark fail its fabric check when NCCL falls back to sockets. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/validation.md#training-performance-validation)
- [**21 · How fast is the first token?**](episodes/21.json) — Read startup latency and steady-state throughput as separate checks against recipe thresholds. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/validation.md#inference-performance-validation)
- [**22 · Who tested this recipe?**](episodes/22.json) — Distinguish simulation, hardware testing, and independent evidence for one AICR version. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/evidence-dashboard.md)
- [**23 · Publish your own run**](episodes/23.json) — Move validation evidence from a cluster network to a signer and give reviewers an independently verifiable pointer. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/contributor/evidence-publishing.md)
- [**24 · Turn trust into a deployment gate**](episodes/24.json) — Enforce verified trust in CI and watch a valid but lower-trust bundle stop before deployment. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/artifact-verification.md#minimum-trust-level-enforcement)
- [**25 · What’s inside the release?**](episodes/25.json) — Follow one image index into an architecture-specific manifest to find the right signed records. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/supply-chain-verification.md#unified-metadata-retrieval)
- [**26 · Bring your own validation check**](episodes/26.json) — Register a containerized prerequisite check and catch a misspelled catalog name before any Job starts. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/validator-extension.md)

## Operator preview

- [**27 · Healthy at which revision?**](episodes/27.json) — See a dependent release wait until the expected digest, current values, fresh reconciliation, and health line up. **Operator preview.** [Check readiness at the current revision](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/operator/argocd.md#readiness-at-the-current-revision)
- [**28 · Who approves a GPU-stack change?**](episodes/28.json) — Decide which transitions need a second person and see an approved rollout stall if its starting state changes. **Operator preview.** [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/operator/operations.md#approving-a-rollout)
- [**29 · Pause, suspend, or revoke?**](episodes/29.json) — Choose between holding new Operator steps, stopping automated Argo CD sync, and ending the rollout. **Operator preview.** [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/operator/operations.md#pausing-versus-suspending-the-engine)
- [**30 · Roll back by plan digest**](episodes/30.json) — Select a retained plan by its digest, then assess and approve the transition back to it. **Operator preview.** [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/operator/operations.md#rolling-back-by-pinning)
- [**31 · Prove what rolled out**](episodes/31.json) — Compare the Operator’s bundle with a local render and link the rollout to its validation evidence. **Operator preview.** [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/operator/cli-reference.md#reproduce)
- [**32 · Keep the stack when the Operator goes away**](episodes/32.json) — See how retaining releases differs from approved removal, including when explicit adoption is needed. **Operator preview.** [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/operator/lifecycle.md#uninstall)
- [**33 · Where do the pinned charts live?**](episodes/33.json) — Follow one chart from the publisher through the runtime registry to a digest-pinned Argo CD pull. **Operator preview.** [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/operator/index.md#choosing-a-registry)

## SDK, API, and automation

- [**34 · Serve recipes over HTTP**](episodes/34.json) — A selected EKS H100 target becomes a resolved recipe and deployment zip through the API. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/api-reference.md)
- [**35 · Run validation from Go**](episodes/35.json) — A Go portal shows completed checks and an interruption together, making a partial run useful. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/go-library.md#snapshotting-and-validation)
- [**36 · Two catalogs in one Go process**](episodes/36.json) — Two clients resolve the same target against their configured catalogs, with an immutable OCI digest for production. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/go-library.md#recipe-sources)
- [**37 · One checked-in config, two entry points**](episodes/37.json) — One reviewed `AICRConfig` change supplies matching criteria and catalog choices to both callers. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/go-library.md#using-a-committed-aicrconfig)
- [**38 · Put AICR in CI**](episodes/38.json) — CI deploys the exact verified bundle and checks the running stack against the recipe carried inside it. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/integrator/automation.md#pattern-2-canonical-snapshot-to-bundle-pipeline)
- [**39 · Ask a coding agent for a recipe**](episodes/39.json) — A plain-English GPU question becomes an auditable AICR command and a catalog-resolved value. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/demos/query.md#plain-english-queries-via-the-aicr-agent-skill)

## Workload stories

- [**40 · Launch distributed training**](episodes/40.json) — A two-node adaptation makes the job shape explicit and shows how to verify that workers land on separate GPU nodes. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/demos/cuj1-training.md)
- [**41 · Submit a Slurm job with Slinky**](episodes/41.json) — An EKS H100 cluster gains Slinky through an AICR recipe, and an `srun` smoke job confirms the assigned GPU is visible. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/demos/cuj1-slinky-slurm.md)
- [**42 · Run inference with AICR**](episodes/42.json) — Select Dynamo or NVIDIA NIM, resolve a platform-specific recipe and bundle, validate the installed stack, then test the chosen endpoint. [Dynamo walkthrough](https://github.com/NVIDIA/aicr/blob/v1.3.0/demos/cuj2-inference.md) · [NIM workload example](https://github.com/NVIDIA/aicr/blob/v1.3.0/demos/workloads/inference/nimservice-llama-3-2-1b.yaml)
- [**44 · What happens when a GPU fails?**](episodes/44.json) — Follow an NVSentinel event from stock monitoring to an explicitly configured quarantine policy for qualifying faults. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/user/component-catalog.md#nvsentinel-deployment-posture)
- [**45 · Where do the metrics go?**](episodes/45.json) — The episode separates NKTP’s observability export from the runtime Prometheus queries used by components such as the autoscaling adapter. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/docs/design/026-nktp-telemetry-collection.md)

## On main, after v1.3.0

- [**46 · Clean up after an interrupted validation run**](episodes/46.json) — Preview which run-scoped resources `aicr sweep` would remove and see the checks that precede deletion. **Unreleased in v1.3.0.** [Source](https://github.com/NVIDIA/aicr/blob/c8903a38a3f6d80be5be0516b010413c8a4df54e/docs/user/cli-reference.md#aicr-sweep)
- [**47 · DGX Spark training on NVKE**](episodes/47.json) — Verify CRI and Arm64 workload compatibility before deploying and validating the single-node GB10 preview on NVKE. **Unreleased in v1.3.0.** [Source](https://github.com/NVIDIA/aicr/pull/3942)

## Reference cuts

- [**Serve a model with Dynamo**](episodes/42-dynamo-reference.json) — Follow an inference request from the CPU frontend to a vLLM GPU worker and back. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/demos/cuj2-inference.md)
- [**Serve a model with NVIDIA NIM**](episodes/43-nim-reference.json) — Start from the installed NIM Operator, deploy a `NIMService`, and confirm the model appears at `/v1/models`. [Source](https://github.com/NVIDIA/aicr/blob/v1.3.0/demos/workloads/inference/nimservice-llama-3-2-1b.yaml)
