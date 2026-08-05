# UAT Day/Night Cycle and Reservation Broker

**AICR's real-hardware UAT runs on a small set of reserved GPU pools that must be time-shared.** The day/night broker (issue #1274) arbitrates that scarce capacity so contending runs *queue* instead of racing the hardware, driven entirely by a checked-in registry. This page explains the operating model, how to request a run, how queuing behaves, and how to add a reservation.

## The day/night cycle

Each reserved GPU pool follows a daily cycle, with every phase acquiring the *same* per-reservation lease so CI and human use never overlap on one reservation:

- **Night — the nightly batch.** On a cron, `uat-nightly-batch.yaml` runs the [version matrix](#the-version-matrix) per reservation — `main` plus the previous N stable releases — each cell a full provision → CUJ → evidence → publish → teardown (for `intent=inference` the CUJ serve step is wired but not executed pending #1644 — those cells run provision → install → validate/conformance → verify → teardown). This is the `lifecycle=nightly` mode: provision-and-destroy under a run-scoped cluster name.
- **On demand — handoff.** The [daytime human-access deployment](#daytime-human-access-deployment) is stood up with `lifecycle=daytime-up`: provision, deploy the stack, and **hold on success** (tearing down on failure or cancellation) under an ephemeral, `(slug, slot)`-scoped cluster name (`aicr-uat-day-<slug>-<slot>-<run-id>`, [ADR-017](../design/017-uat-cluster-name-convention.md)) — unique per provision, so a re-provision never reuses the name the previous teardown just deleted (which collides with cloud deletion tombstones and stale terraform-state locks). This is **on demand — there is no morning cron**: an operator dispatches it when they want a cluster (see [Requesting a daytime cluster](#requesting-a-daytime-cluster)). DC2 owns the provision-and-hold mechanic; DC8 (`uat-daytime.yaml`) owns *which* flavor lands on *which* cloud and how access is shared.
- **Day — human use.** The daytime cluster is used outside CI — humans reach it [out-of-band](#daytime-human-access-deployment), never through the CI path.
- **Evening — teardown.** `uat-daytime.yaml` fires `lifecycle=daytime-down` on an evening cron — the **only** scheduled daytime edge — to tear the daytime cluster down and release the reservation **before** the next night batch. It is an unconditional safety net: it runs whether or not anyone stood a cluster up, so a manually-provisioned daytime cluster left running is reclaimed without anyone remembering to ask. It is a teardown *attempt*, not a guarantee — if the destroy itself fails, the [pre-batch guard](#pre-batch-guard) blocks the nightly batch rather than letting it race the still-held cluster.

The phases are independently scheduled (cron edges), not chained: the per-reservation lease — plus a [pre-batch guard](#pre-batch-guard) — keeps them from overlapping, so a crashed or overrunning phase never orphans the reservation. A hosted GitHub Actions job is capped at the runner's timeout (hours, not a whole working day), so a single lease-holding run cannot span the day; the lease only needs to cover the brief transition windows, and the steady-state daytime cluster's existence is tracked by its `(slug, slot)`-scoped name **prefix** (`aicr-uat-day-<slug>-<slot>-*`, discovered by a list-and-match scan) rather than a continuously held run.

> What ships today is the **night side** (the nightly batch), the **lease + dispatch surface** every phase builds on, DC2's **per-intent selection**, **daytime provision-and-hold / teardown mechanics**, and **pre-batch guard**, DC8's **day side** — the `uat-daytime.yaml` scheduler that stands up one human-facing deployment per cloud **on demand** and tears it down on an evening safety-net cron, and DC3's **served-inference CUJ** — the `phase_serve` step of an `intent=inference` run (deploy a `DynamoGraphDeployment`, hit its OpenAI-compatible endpoint, assert a completion); the `phase_serve` runner source ships and is intent-selected, but the workflow step is currently disabled in both cloud pipelines pending #1644, so automated runs validate the inference platform without executing the serving CUJ.

## Requesting a UAT run

All UAT runs go through one entry point, `uat-run.yaml` — the shared dispatch surface that owns the reservation lease. To request a run, dispatch it with a reservation name from the registry:

```bash
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main -f reservation=aws-h100
```

`uat-run.yaml` resolves the reservation row, then invokes the cloud-appropriate reusable pipeline (`uat-aws.yaml`, `uat-gcp.yaml`, `uat-azure.yaml`, or — for the `service: kind` real-silicon lane — `uat-kind.yaml`). A typo'd reservation name fails fast in the resolve step (the `uat-broker` helper exits *not found*). For manual debugging, `skip_tests` and `skip_delete` inputs are available.

The **nvkind lane** (`cloud: kind`, DC5 #1278) is a full sibling of the cloud lanes — same `uat-run` dispatch, same reservation lease, same nightly batch, same phase-by-phase runner (`tests/uat/kind/run`, sharing `tests/uat/lib/collect-debug.sh`), and the same signed-evidence emit → verify → ingest. It differs only in provisioning: instead of a `github.com/mchmarny/cluster` actuator it stands up a **single-node, single-GPU nvkind cluster on a self-hosted GPU runner** (`.github/actions/gpu-cluster-setup`) and tears it down with `.github/actions/gpu-test-cleanup` — no cloud credentials, no capacity reservation (the runner *is* the lease). Validator/agent images resolve to the runner-local `ko.local` registry for `main` cells; release cells install the released `aicr` and pull released images. Scope is honestly H100 ×1, single-GPU.

Two further inputs shape the run (both default to the nightly-batch behavior, so the cron needs neither):

```bash
# Inference intent, nightly provision→validate→teardown (serve CUJ wired, disabled pending #1644)
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main \
  -f reservation=aws-h100 -f intent=inference

# On demand: stand up the daytime cluster and hold it (single reservation)
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main \
  -f reservation=aws-h100 -f lifecycle=daytime-up

# Evening teardown of the held daytime cluster
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main \
  -f reservation=aws-h100 -f lifecycle=daytime-down
```

The nightly batch and the daytime handoff/teardown call this *same* surface, so every run for a reservation contends on one lease.

## Selecting the intent

The `intent` input (`training` — the default — or `inference`) selects both the recipe criteria and the per-intent test config the pipeline consumes: `tests/uat/<cloud>/tests/h100-<intent>-config.yaml`. The two configs are siblings with the same `AICRConfig` shape; they differ only in `spec.recipe.criteria.intent`/`platform` (`training`/`kubeflow` vs `inference`/`dynamo`) and the stable evidence push prefix. Both intents provision from the *same* `cluster-config.yaml` — the GPU pool count comes from the reservation row and the system/CPU pools stay per-run dynamic (GCP autoscales; AWS is fixed at `desired: 3`), so nothing about the cluster shape is hardcoded per intent.

The CUJ phase is intent-selected — exactly one of `phase_train` / `phase_serve` is chosen, mirroring the runner's intent-aware `run all` (`phase_serve`'s workflow step is currently disabled pending #1644 — selection is wired, execution is not):

- **`intent=training` → `phase_train`.** Submits a Kubeflow `TrainJob`, waits for completion, captures logs (`demos/cuj1-training.md`).
- **`intent=inference` → `phase_serve`** (DC3, #1276). Deploys a Dynamo `DynamoGraphDeployment` (KAI queue + Frontend/decode-Worker graph — the worker requests its GPU as a scalar `nvidia.com/gpu` limit, matching the device-plugin production default from the #1327 flip; the runner source is converted and ready, but the workflow step remains disabled pending #1644, so nightly runs do not exercise it yet) onto the already-validated inference stack, waits for the pods to become ready, port-forwards the frontend, issues a sample OpenAI-compatible `/v1/chat/completions` request, and asserts a non-empty completion — the inference counterpart of `phase_train`, at CUJ1 parity (`demos/cuj2-inference.md`). It **fails closed** (non-zero exit, captured pod logs/events under `serve-logs/`) on a non-ready deployment or an invalid completion, mirroring `phase_train`'s `Failed=True` handling. The served workload's node scheduling and model are overridable via `SERVE_*` env vars; the defaults track `demos/workloads/inference/vllm-agg.yaml`.

In both cases the signed evidence bundle is emitted by the earlier conformance step (which validates the full deployed stack); for training the CUJ step then exercises the deployment, while for inference the serve CUJ is wired but not executed pending #1644 (evidence still covers the deployed stack). `phase_conformance` also cross-checks that the recipe's declared `platform` matches the deployed component set — the platform operator's workload CRD (`dynamographdeployments.nvidia.com` for `dynamo`, `trainjobs.trainer.kubeflow.org` for `kubeflow`) must be installed — because the emitted bundle's TestGrid tab coordinate is derived from the author-declared platform and is otherwise cluster-unverifiable (the fingerprint does not capture the platform dimension).

An unrecognized `intent` (or a missing sibling config) fails closed in the pipeline's `Validate inputs` step before any provisioning.

### Nightly intent cadence (both intents, all three clouds)

The single nightly cron (`uat-nightly-batch.yaml`, `0 4 * * *`) runs **both intents on every nightly-enrolled reservation**, so training *and* inference are exercised nightly on AWS, GCP, and Azure (see the table below). (Note: an inference cell currently provisions and validates the inference platform; the `phase_serve` serving-CUJ step itself remains disabled in both cloud workflows pending #1644, so nightly runs do not yet execute the serving request path.) The set of intents per reservation is data — the `nightly-intents` list in `infra/uat/reservations.yaml` (absent defaults to `[training]`; an explicit empty list `[]` opts the reservation out of the nightly batch entirely — bring-up mode, manual dispatch only):

| Reservation | Cloud | `nightly-intents` | Nightly CUJs |
|-------------|-------|-------------------|--------------|
| `aws-h100` | AWS | `[training, inference]` | `phase_train` + `phase_serve` (serve step disabled pending #1644) |
| `gcp-h100` | GCP | `[training, inference]` | `phase_train` + `phase_serve` (serve step disabled pending #1644) |
| `azure-h100` | Azure | `[training, inference]` | `phase_train` + `phase_serve` (serve step disabled pending #1644); inference gated to `>= v0.18.0` via `nightly-intent-min-versions` (see **Cost / tuning** below) |
| `kind-h100` | kind (nvkind) | `[training, inference]` | training → `phase_train`; inference runs **no `phase_serve`** — its evidence comes from the `--phase all` conformance step (vLLM is excluded from UAT, as on the cloud lanes; #1644). Single-GPU; both intents gated to `>= v0.18.0` via `nightly-intent-min-versions` (the lane + os-agnostic coordinate fix #1851 postdate v0.17.0), so only `main` runs nvkind nightly until v0.18.0 ships |

**How it stays contention-free — serialize, don't add a second cron.** The intents are folded into the existing [version matrix](#the-version-matrix) as extra cells rather than a second scheduled job. The controller's drive loop is **version outer / intent inner**: for each version it dispatches one intent's full provision→CUJ→teardown cell (inference cells currently run provision→validate→teardown; the serve CUJ is disabled pending #1644), waits for it (`gh run watch`), then dispatches the next — all through the *same* per-reservation lease. So the intents serialize naturally, and because `main` runs every intent before any release cell, a time-box drop only ever sheds the oldest *release* cells (never `main`'s inference). This is the deliberate DC3 cadence decision: **never schedule two daily crons against one reservation** — the lease is a single-slot queue (one in-progress + one pending), so a second cron plus an occasional human dispatch on the same reservation is a routine three-contender case whose loser is silently [superseded](#how-queuing-works-the-reservation-lease). One cron dispatching serialized cells sidesteps that entirely.

**Cost / tuning.** Listing both intents roughly **doubles a reservation's nightly cell count** (each version now runs two full cluster lifecycles). If the batch [time-box](#the-version-matrix) is exceeded the oldest cells are dropped first, so `main`+freshest always land; tune `previous_n` (fewer release versions) or `deadline_offset_hours` to fit the window. A released version that predates a platform (e.g. `dynamo`) fails its inference cell's recipe resolution as a genuine regression signal — drop `previous_n` if that coverage is premature. Changing which intents a reservation runs is a registry edit — no workflow change; the `uatbroker` committed-registry test pins the launch set.

**Gating an intent to a minimum release — `nightly-intent-min-versions`.** When an intent only became *supported* on a reservation at a particular release — a fix or platform that older releases lack — running it on the pre-support releases produces a permanently-red cell, not a regression signal. Express the floor per intent in the registry row:

```yaml
- name: azure-h100
  nightly-intents: [training, inference]
  nightly-intent-min-versions:
    inference: v0.18.0   # first release that carries the AKS perf fix (#1767)
```

Semantics: **`main` is never gated** (it is built from source and carries the newest fixes, so it always runs every listed intent); a **release** cell drops any intent whose minimum version is newer than the tag (semver; a tag `>=` the minimum runs). The gate lives in the schedule (`uat-broker schedule` attaches each cell's eligible `intents`), so the controller simply never dispatches a gated `(version × intent)` — no per-version workflow logic. Pointing the floor at a **not-yet-tagged** release is intentional and self-resolving: until that release ships, the intent runs on **`main` only** (green, continuous coverage of the fix), and the release enrolls automatically once it exists. `Validate` rejects a floor for an intent the row does not run, or a non-semver value. Bump the floor if the real first-fixed tag differs — an over-low floor surfaces as a visible red (safe), an over-high floor silently skips a good release (bump down).

## Cluster lifecycles

The `lifecycle` input selects one of three cluster lifecycles, all sharing the reservation lease:

| Lifecycle | Cluster name | Provisions | Deploys | CUJ | Teardown at job end |
|-----------|--------------|-----------|---------|-----|---------------------|
| `nightly` (default) | `aicr-uat-<run_id>` — run-scoped | yes | yes | yes (prep→install→validate→train\|serve→verify; the serve step is disabled pending #1644) | yes (unless `skip_delete`) |
| `daytime-up` | `aicr-uat-day-<slug>-<slot>-<run_id>` — **ephemeral** | yes | yes (prep→install) | no | **holds on success; tears down on failure/cancellation** |
| `daytime-down` | *discovered* by the `aicr-uat-day-<slug>-<slot>-` prefix | no | no | no | yes (tears down the discovered cluster) |

Names use the **same `aicr-uat-` scheme on all three platforms** (matching the committed `id: aicr-uat` in each `cluster-config.yaml`), so a name is platform-independent. The nightly per-run name isolates concurrent history (OCI tags, Terraform state) per run. The daytime name is **ephemeral** — a stable, `(slug, slot)`-scoped prefix with the up-run's id appended ([ADR-017](../design/017-uat-cluster-name-convention.md)) — so a re-provision never reuses the name the previous teardown just deleted (cloud deletion tombstones and stale terraform-state locks make same-name reuse flaky). The `slug` is a short 2–4 char registry field (account-stable and unique across reservations, where the reservation name is only cloud-unique), and `slot` is a runtime input (default `0`, slot-ready for a future multi-cluster-per-day end state); together they keep the daytime name inside GKE's 40-char cap. Because the name is no longer reconstructable from the reservation/slug alone, the evening `daytime-down` and the pre-batch guard find the held cluster by **scanning the prefix** (`list-clusters`/`az aks list`/`clusters list` + match) — a name-free, list-and-match discovery. (The nightly per-run name needs no discovery: it is derived deterministically from the run id and torn down by the same run.) During the rename the scans carry a second, **legacy** leg matching the old `aicr-uat-day-<reservation>-` prefix so an in-flight old-named daytime cluster is still discovered and torn down; it is a transitional shim removed once none remain. The actuator derives the cluster name, resource group, and terraform-state key all from `.deployment.id`, so recovering the name recovers everything teardown needs. `skip_delete` is a nightly-only debugging escape and is ignored by the daytime lifecycles.

## Daytime human-access deployment

The **day side** of the cycle (issue #1281, DC8) stands up **one long-lived, human-facing deployment per cloud** for the working day — a place to submit jobs, hit a served endpoint, and demo, **outside CI**. It is *not* a UAT cell: it emits **no evidence bundle** and produces **no TestGrid column**, and access is distributed [out-of-band](#reaching-the-daytime-cluster). The scarce reservation time is split between this human use and the nightly [version matrix](#the-version-matrix); the two never overlap on one reservation because both route through the same lease.

### The cloud→flavor split

Which cloud hosts which flavor is **data, not code**: the `daytime-intent` column of each row in `infra/uat/reservations.yaml`. A row with `daytime-intent: training` or `daytime-intent: inference` joins the daytime rotation; an empty/absent value keeps the reservation nightly-batch-only. The launch default splits the two flavors across the two clouds:

| Reservation | Cloud | `daytime-intent` | Daytime deployment |
|-------------|-------|------------------|--------------------|
| `aws-h100` | AWS | `training` | training stack (Kubeflow `TrainJob`s) |
| `gcp-h100` | GCP | `inference` | inference stack (Dynamo, OpenAI-compatible endpoint) |

Re-splitting (or adding a daytime reservation) is a registry edit — no workflow change. Only **one** reservation per cloud may carry a `daytime-intent` today: a single reservation cannot host both a held daytime cluster and the nightly batch at once, so *both* flavors on one cloud during the day is out of scope until more capacity lands. The `uatbroker` committed-registry test enforces the one-per-cloud invariant and the launch split.

### The scheduler (`uat-daytime.yaml`)

`uat-daytime.yaml` is a thin scheduler over the `daytime-up` / `daytime-down` mechanics — it owns no lifecycle logic. It enumerates the rotation (`uat-broker reservations --daytime` → a JSON `{reservation, intent}` matrix) and, once per reservation, dispatches the shared `uat-run.yaml` with the reservation's intent and the edge's lifecycle, then watches the dispatched run to completion so a failed handoff/teardown surfaces on the scheduler run. Because it goes through `uat-run.yaml`, each daytime run takes the **same per-reservation lease** as the nightly batch.

`daytime-up` is **manual only** — there is no morning cron. Only the evening teardown is scheduled, as an unconditional safety net. So there is **one** scheduled edge, plus a manual `workflow_dispatch` with an `action: up | down` input:

| Edge | Trigger | Action | Lifecycle dispatched |
|------|---------|--------|----------------------|
| Handoff (stand up) | **manual dispatch** (`action=up`) | `up` | `daytime-up` (provision + deploy; holds on success, tears down on failure/cancellation) |
| Evening teardown | cron `0 2 * * *` (UTC) **or** manual (`action=down`) | `down` | `daytime-down` (tear down + release) |

The evening teardown runs ~2h before the nightly batch opens (`0 4 * * *`), leaving margin for a ~10–15 min destroy, and fires **whether or not** a cluster was stood up — so a manually-held cluster is reclaimed without anyone remembering to ask. If the destroy fails, the [pre-batch guard](#pre-batch-guard) blocks the batch.

#### Requesting a daytime cluster

Stand up (or tear down) the **whole daytime rotation** — every reservation carrying a `daytime-intent` — by dispatching the scheduler:

```bash
gh workflow run uat-daytime.yaml --repo NVIDIA/aicr --ref main -f action=up    # stand up + hold
gh workflow run uat-daytime.yaml --repo NVIDIA/aicr --ref main -f action=down  # tear down + release
```

To stand up (or tear down) a **single reservation** without touching the rest of the rotation, dispatch `uat-run.yaml` directly:

```bash
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main \
  -f reservation=aws-h100 -f lifecycle=daytime-up      # or -f lifecycle=daytime-down
```

You never *have* to tear a cluster down by hand — the evening safety-net cron will — but doing so frees the reservation (and its GPU capacity) sooner. Different reservations run in parallel (independent hardware); a daytime run that finds its reservation still busy (an overrunning batch) *queues* on the lease rather than racing.

### If the evening teardown is missed

The teardown is not the only safety net. If a `daytime-down` is skipped or fails and the daytime cluster is still up when the nightly batch opens, DC2's [pre-batch guard](#pre-batch-guard) **blocks** the batch (fail-closed) rather than racing the held deployment. Recover by tearing the daytime cluster down — `gh workflow run uat-daytime.yaml -f action=down`, or a single `uat-run.yaml … -f lifecycle=daytime-down` for one reservation — then re-run the batch.

### The orphan janitor (backstop teardown)

`uat-janitor.yaml` (hourly, all three clouds) is the last-resort backstop for whatever the in-run teardown misses — a runner hard-killed mid-cancel, a daytime hold whose evening `daytime-down` never fired, an abandoned `skip_delete` run, or a Bringup false-failure that stranded a healthy cluster. It reconciles every `aicr-uat-<run_id>` / `aicr-uat-day-<reservation>-<run_id>` deployment against its owning GitHub run and reaps only the ones whose run is finished and past an age floor, via the same actuator `destroy` the in-run teardown uses. It is **dry-run by default**: scheduled runs only report; an actual reap requires a deliberate `workflow_dispatch` with `enforce=true`. See `.github/scripts/uat-janitor.sh` for the full safety model.

**Retention limit for `skip_delete`.** A cluster kept alive with `skip_delete=true` (manual debugging) **becomes eligible for enforced reclamation ~24h after its run finishes** — there is no API signal that distinguishes an intentional hold from a genuine orphan, so the age floor is the only lever. (While the janitor stays dry-run-first, eligibility only becomes a *reclaim* on an `enforce=true` dispatch — or automatically once scheduled enforcement is turned on.) Treat a `skip_delete` cluster as a one-working-day loan; if you need it longer, re-provision.

### Reaching the daytime cluster

Access is **out-of-band by design**: nothing here routes a kubeconfig or endpoint URL through the CI path, the evidence bundle, or the dashboard. Access is gated by **cloud IAM** on the daytime cluster — so an authorized operator mints their own kubeconfig directly and no credential ever transits CI. Because the daytime cluster is now ephemerally named, first **discover** it by its `(slug, slot)`-scoped prefix (with the legacy `aicr-uat-day-<reservation>-` prefix as a fallback during the ADR-017 migration window, matching the dual-prefix scan the workflows carry), then bind to the discovered name:

```bash
# Expect exactly one match — zero means no cluster is held (nothing to reach),
# and more than one means a leak the pre-batch guard should have caught.
one() { [ "$(printf '%s' "$1" | grep -c .)" -eq 1 ] || { echo "expected exactly one daytime cluster, got: ${1:-<none>}" >&2; return 1; }; }

# AWS — training cluster: new (slug, slot) prefix aicr-uat-day-ah1-0-, plus the
# legacy aicr-uat-day-aws-h100- for the life of the ADR-017 migration shim (drop
# the legacy alternative once no old-named daytime clusters remain).
name=$(aws eks list-clusters --region us-east-1 --query "clusters[]" --output text \
  | tr '\t' '\n' | grep -E '^aicr-uat-day-(ah1-0|aws-h100)-')
one "$name" && aws eks update-kubeconfig --region us-east-1 --name "$name"

# GCP — inference cluster: new prefix aicr-uat-day-gh1-0-, plus legacy
# aicr-uat-day-gcp-h100- during the migration shim.
name=$(gcloud container clusters list --project eidosx --format='value(name)' \
  | grep -E '^aicr-uat-day-(gh1-0|gcp-h100)-')
one "$name" && gcloud container clusters get-credentials "$name" --region us-central1 --project eidosx
```

**Training (AWS).** Submit Kubeflow `TrainJob`s against the held cluster — the same CUJ the nightly `intent=training` run exercises (see `demos/cuj1-training.md`).

**Inference (GCP).** The `daytime-up` run deploys the Dynamo inference *platform* (dynamo-platform + KAI scheduler + DRA driver). Apply a `DynamoGraphDeployment` served workload — reuse an existing serve asset such as `demos/workloads/inference/vllm-agg.yaml`; DC8 does **not** invent a serving stack — then reach its OpenAI-compatible endpoint by port-forwarding the frontend:

```bash
kubectl port-forward -n dynamo-system svc/vllm-agg-frontend 8000:8000 &
curl http://localhost:8000/v1/models
curl http://localhost:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"<model>","messages":[{"role":"user","content":"hello"}]}'
```

On the held daytime cluster this served workload is a one-command manual apply (above): `daytime-up` deploys the inference *platform* and holds, so a human drives the serve step by hand. The automated `phase_serve` (DC3) belongs to the **nightly** `intent=inference` CUJ, not `daytime-up`, which by design stops after install to hand the cluster off — though its workflow step is currently disabled pending #1644, so nightly inference cells validate the platform without executing the serve step.

## Pre-batch guard

A missed evening teardown must surface as a **blocked run, never as silent contention** with the still-running daytime deployment. Before it provisions, every `nightly` **and** `daytime-up` run asserts that no daytime cluster is still up on the target reservation — `daytime-up` guards too, because with ephemeral names a leaked prior cluster would otherwise let a second daytime cluster stack on a reservation that can host only one. Detection is a **prefix scan** (`aicr-uat-day-<slug>-<slot>-*`, the same scheme on every platform, plus a transitional legacy `aicr-uat-day-<reservation>-*` leg during the ADR-017 rename): the held cluster's exact name is no longer reconstructable, so the guard lists clusters and matches the `(slug, slot)`-scoped prefix. The check runs *after* the run has acquired the reservation lease and authenticated to the cloud, and *before* Bringup — so it fails fast rather than racing. It fails **closed**: a `list-clusters` throttle or auth error blocks the run rather than being read as "no daytime cluster, clear to proceed." If it trips, the error names the held cluster(s) and the exact `lifecycle=daytime-down` reclaim command.

If the guard trips, tear the daytime cluster down with `lifecycle=daytime-down` (which releases the reservation), then re-run the batch.

## Capacity assertions and the GCP posture

**AWS — post-lease assertion.** `uat-aws.yaml` asserts the EC2 capacity reservation is provisioned large enough for the GPU pool's desired count. Because the reservation lease is now the contention gate, this is **not** a race-and-fail pre-flight: it checks the reservation's `TotalInstanceCount` (its fixed provisioned size), not the momentary `AvailableInstanceCount`. A genuinely undersized/exhausted reservation still fails; transient contention (another run's not-yet-released nodes) no longer does, because the lease already guaranteed we are the only run consuming the reservation.

**GCP — actuator-time failure (decided posture).** `uat-gcp.yaml` has **no** pre-flight capacity/quota assertion, and DC2 deliberately did **not** add one. GCP relies on the GKE actuator failing at provision time if the reservation is exhausted. With the reservation lease serializing contending runs, a provision-time failure means a genuinely undersized/exhausted reservation, not a race — so a symmetric gcloud reservation check would add a second cloud API surface without changing the outcome. This is a recorded decision, not an oversight; there is intentionally no capacity step in the GCP pipeline.

**Azure — quota-backed, GCP posture.** Azure capacity is **subscription quota** (westus `NDSH100v5`), not a reservation object — the registry row carries no `reservation-id` — and `uat-azure.yaml` follows the GCP posture: no pre-flight capacity assertion; the AKS actuator fails loudly at provision time if the quota is exhausted (with the lease serializing contenders, that failure means genuinely exhausted quota, not a race). Auth differs in mechanism, not model: `azure/login` exchanges the GitHub OIDC token against an Entra federated credential and writes the az CLI context to `~/.azure`, which is mounted into the AKS actuator container; because a federated az session cannot self-refresh, each long phase re-runs `azure/login` so no phase runs on an expired token.

## How queuing works (the reservation lease)

The lease is a GitHub Actions concurrency group keyed by reservation name — `uat-<reservation>` (for example `uat-aws-h100`) — declared on `uat-run.yaml` with `cancel-in-progress: false`. Two runs that target the *same* reservation serialize: the second waits until the first (including its teardown) finishes. Two runs that target *different* reservations share no group and run in parallel, because they are independent hardware.

This replaces the previous behavior, where a second run hitting a busy AWS reservation hard-failed on the capacity check. Now it queues.

**The one-in-progress-plus-one-pending limit.** GitHub concurrency holds at most one in-progress run plus one pending run per group. If a *third* run is queued for a reservation that already has one in-progress and one pending, GitHub cancels the older pending run and the newest takes its place. At launch this is acceptable: there are three reservations, each contended by at most the nightly cron plus an occasional ad-hoc dispatch. A run cancelled this way is *superseded*, not failed. So that a dropped request is never silent, the `uat-superseded-notice.yaml` observer watches for it: triggered on `workflow_run: completed` for `UAT Run`, it classifies a cancelled run that never started a job as a supersede (versus a genuine mid-run cancel) and emits a job-summary entry plus a `::warning`. (The nightly controller reconciles the same signal synchronously for the cells it dispatches; a DC6 regression guard, #1279, will exercise the observer.) If deeper queuing is ever needed (many requesters per reservation), the escalation path is the *Deferred* standing broker service — a pull-based queue rather than GitHub concurrency — recorded in the epic (#1264).

## The version matrix

The nightly batch runs a **cross-version regression** per reservation: `main` (built from source at tip) plus the previous **N** stable releases, so an older stable `aicr` is re-checked against today's cluster. `uat-broker schedule` orders the cells `main`-first, then releases in descending semver order; the controller runs them **sequentially** on the reservation (each cell dispatched through `uat-run.yaml`, so they share the lease) and **time-boxes** the batch — once the deadline passes it stops dispatching, so the in-flight cell finishes and the remaining (oldest) releases are dropped, guaranteeing `main` and the freshest releases always land.

**Release cells install released artifacts, not source.** A `main` cell builds the `aicr` binary + validator/agent images from the checked-out tree. A release cell (`aicr_version=vX.Y.Z`) instead downloads the released `aicr` binary at that tag; the released binary self-resolves its own version's validator images (`…/aicr-validators/<phase>:vX.Y.Z`) and snapshot agent (`ghcr.io/nvidia/aicr:vX.Y.Z`), so no images are built for release cells. Each run's summary records its `aicr_version` (`main` or the tag).

**Release cells verify what they install.** The `install-aicr-release` composite action does two checks before a downloaded binary is used, and **fails closed** on either: (1) *integrity* — the archive matches its `aicr_checksums.txt` entry; and (2) *provenance* — `cosign verify-blob-attestation` validates the SLSA Build Provenance v1 attestation goreleaser ships inside the archive (`aicr-attestation.sigstore.json`). The verifier does not trust *any* NVIDIA release signer: it derives the certificate-identity regexp from the requested `aicr_version`, so **only the attestation for that exact release tag** is accepted (`on-tag.yaml@refs/tags/<that-version>`, issuer `token.actions.githubusercontent.com`) — an attestation for a different tag is rejected. The attestation's subject is the binary's own digest, so this also binds authenticity to the exact bytes that run — not to the same-release checksums manifest. A release whose binary is unattested, or whose attestation does not verify, aborts the cell rather than running an unverified `aicr`.

**Tunables** — workflow inputs on `uat-nightly-batch.yaml` (these are the scheduled-run defaults):

- `previous_n` — stable releases below `main` to run per reservation (default `2`; `0` = `main` only).
- `deadline_offset_hours` — hours after batch start to stop dispatching new cells (default `5`). This is a **secondary** cap: the controller also enforces a **budget-aware** cutoff derived from the drive job's own `timeout-minutes`, stopping dispatch once fewer than `max_cell_minutes` remain so the last cell always finishes before GitHub kills the job. The effective cutoff is the earlier of the two, so `deadline_offset_hours` no longer needs hand-tuning against the job timeout to keep the graceful drop-oldest reachable.
- `max_cell_minutes` — wall-clock a single dispatched cell may need to complete (default `150`). Sets the drive job's dispatch reserve: a new cell is dispatched only if at least this many minutes remain before the job's `timeout-minutes` (a small setup slack is also held back), so an overrun sheds the oldest remaining cell gracefully instead of hard-failing the leg mid-cell. Keep it at or above the realistic worst-case cell duration.

To test a single released version by hand: `gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main -f reservation=aws-h100 -f aicr_version=v1.2.3`. (`--ref main` dispatches the nightly-path revision of the workflow, not your feature branch's.)

## Adding a reservation

Reservations are data, not code. To onboard a new reserved pool, add a row to `infra/uat/reservations.yaml`:

```yaml
- name: aws-b200          # the lease key; becomes concurrency group uat-aws-b200
  slug: ab2               # 2-4 char (^[a-z][a-z0-9]{1,3}$), UNIQUE across rows — the daytime cluster name's discovery key (ADR-017)
  cloud: aws              # aws | gcp | azure — selects which pipeline (EKS / GKE / AKS) provisions
  reservation-id: cr-...  # the cloud capacity-reservation id (GCP uses the full path); OMIT for quota-backed capacity (azure)
  accelerator: b200
  gpu-count: 8
  cluster-config-path: tests/uat/aws/cluster-config-b200.yaml
  test-config-dir: tests/uat/aws/tests
```

No broker, workflow, or Go change is needed — the nightly batch enumerates rows from the registry, and `uat-run.yaml` resolves them. The unit of sequencing is the *reservation*, so a new GPU type in an existing cloud simply runs in parallel with the others on its own lease. (Provisioning is per *cloud*: the same `uat-aws.yaml` pipeline provisions any AWS accelerator from the row's `cluster-config-path`; you do not add a per-accelerator workflow.)

Onboarding a new *cloud* (rather than a new pool in an existing cloud) is a code change on top of the row: a `run-<cloud>` job in `uat-run.yaml`, a `uat-<cloud>.yaml` pipeline, and account federation under `infra/uat-<cloud>-account/`. During bring-up, set `nightly-intents: []` (explicit empty list — absent defaults to `[training]`) so the reservation is manually dispatchable via `uat-run.yaml` but skipped by the nightly batch; flip it once the pipeline has green runs.

The values in this file are identifiers, **not secrets** — a reservation-id grants no access on its own; access to the reserved capacity is governed by cloud IAM/ACLs bound to the CI federation identity (see `infra/uat-aws-account/`, `infra/uat-gcp-account/`, and `infra/uat-azure-account/`). They are safe to commit.

## Roadmap

What ships now is the lease, the data-driven dispatch surface, the time-boxed nightly version matrix (`main` + previous-N stable releases, release cells installing released artifacts), superseded-run surfacing (the controller flags a dropped cell inline; the `uat-superseded-notice.yaml` observer catches ad-hoc dropped runs), per-intent selection, the DC3 [served-inference CUJ](#selecting-the-intent) runner (`phase_serve` — deploys a `DynamoGraphDeployment` and asserts a served completion; both training and inference cells run nightly on both clouds, serialized as extra version-matrix cells under the one cron, but the serve step itself is disabled pending #1644), the daytime provision-and-hold / teardown / pre-batch-guard mechanics, and the DC8 [daytime human-access scheduler](#daytime-human-access-deployment) (`uat-daytime.yaml`) — one held deployment per cloud each working day, torn down before the batch, with out-of-band access. Still to come:

- **Both flavors per cloud during the day.** Blocked on capacity — one reservation cannot hold both a daytime cluster and the nightly batch at once. Pulls once more infra lands.
