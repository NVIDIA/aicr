# UAT Day/Night Cycle and Reservation Broker

**AICR's real-hardware UAT runs on a small set of reserved GPU pools that must be time-shared.** The day/night broker (issue #1274) arbitrates that scarce capacity so contending runs *queue* instead of racing the hardware, driven entirely by a checked-in registry. This page explains the operating model, how to request a run, how queuing behaves, and how to add a reservation.

## The day/night cycle

Each reserved GPU pool follows a daily cycle, with every phase acquiring the *same* per-reservation lease so CI and human use never overlap on one reservation:

- **Night — the nightly batch.** On a cron, `uat-nightly-batch.yaml` runs the [version matrix](#the-version-matrix) per reservation — `main` plus the previous N stable releases — each cell a full provision → CUJ → evidence → publish → teardown. This is the `lifecycle=nightly` mode: provision-and-destroy under a run-scoped cluster name.
- **Morning — handoff.** Once the batch drains a reservation, the [daytime human-access deployment](#daytime-human-access-deployment) is stood up on it with `lifecycle=daytime-up`: provision, deploy the stack, and **hold** (no teardown) under a stable, reservation-tagged cluster name. The `uat-daytime.yaml` scheduler fires this on a morning cron for every reservation in the daytime rotation. DC2 owns the provision-and-hold mechanic; DC8 (`uat-daytime.yaml`) owns *which* flavor lands on *which* cloud and how access is shared.
- **Day — human use.** The daytime cluster is used outside CI — humans reach it [out-of-band](#daytime-human-access-deployment), never through the CI path.
- **Evening — teardown.** `uat-daytime.yaml` fires `lifecycle=daytime-down` on an evening cron to tear the daytime cluster down and release the reservation **before** the next night batch.

The phases are independently scheduled (cron edges), not chained: the per-reservation lease — plus a [pre-batch guard](#pre-batch-guard) — keeps them from overlapping, so a crashed or overrunning phase never orphans the reservation. A hosted GitHub Actions job is capped at the runner's timeout (hours, not a whole working day), so a single lease-holding run cannot span the day; the lease only needs to cover the brief transition windows, and the steady-state daytime cluster's existence is tracked by its stable, reservation-tagged name rather than a continuously held run.

> What ships today is the **night side** (the nightly batch), the **lease + dispatch surface** every phase builds on, DC2's **per-intent selection**, **daytime provision-and-hold / teardown mechanics**, and **pre-batch guard**, and DC8's **day side** — the `uat-daytime.yaml` scheduler that stands up one human-facing deployment per cloud each morning and tears it down each evening. The served-inference *workload* an `intent=inference` cluster runs (`phase_serve`) is still owned by DC3.

## Requesting a UAT run

All UAT runs go through one entry point, `uat-run.yaml` — the shared dispatch surface that owns the reservation lease. To request a run, dispatch it with a reservation name from the registry:

```bash
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main -f reservation=aws-h100
```

`uat-run.yaml` resolves the reservation row, then invokes the cloud-appropriate reusable pipeline (`uat-aws.yaml` or `uat-gcp.yaml`). A typo'd reservation name fails fast in the resolve step (the `uat-broker` helper exits *not found*). For manual debugging, `skip_tests` and `skip_delete` inputs are available.

Two further inputs shape the run (both default to the nightly-batch behavior, so the cron needs neither):

```bash
# Inference intent, nightly provision→CUJ→teardown
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main \
  -f reservation=aws-h100 -f intent=inference

# Morning handoff: stand up the daytime cluster and hold it
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main \
  -f reservation=aws-h100 -f lifecycle=daytime-up

# Evening teardown of the held daytime cluster
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main \
  -f reservation=aws-h100 -f lifecycle=daytime-down
```

The nightly batch and the daytime handoff/teardown call this *same* surface, so every run for a reservation contends on one lease.

## Selecting the intent

The `intent` input (`training` — the default — or `inference`) selects both the recipe criteria and the per-intent test config the pipeline consumes: `tests/uat/<cloud>/tests/h100-<intent>-config.yaml`. The two configs are siblings with the same `AICRConfig` shape; they differ only in `spec.recipe.criteria.intent`/`platform` (`training`/`kubeflow` vs `inference`/`dynamo`) and the stable evidence push prefix. Both intents provision from the *same* `cluster-config.yaml` — the GPU pool count comes from the reservation row and the system/CPU pools stay per-run dynamic (GCP autoscales; AWS is fixed at `desired: 3`), so nothing about the cluster shape is hardcoded per intent.

The nightly CUJ steps are intent-aware: the training TrainJob step runs only for `intent=training`. The served-inference workload (`phase_serve`) is owned by DC3 and out of scope here, so an `intent=inference` run stands up and validates the inference stack (Dynamo platform, KAI scheduler, DRA driver) and signs evidence, but does not yet exercise a served endpoint.

An unrecognized `intent` (or a missing sibling config) fails closed in the pipeline's `Validate inputs` step before any provisioning.

## Cluster lifecycles

The `lifecycle` input selects one of three cluster lifecycles, all sharing the reservation lease:

| Lifecycle | Cluster name | Provisions | Deploys | CUJ | Teardown at job end |
|-----------|--------------|-----------|---------|-----|---------------------|
| `nightly` (default) | `aicr-uat-<run_id>` (AWS) / `aicr-<run_id>` (GCP) — run-scoped | yes | yes | yes (prep→install→validate→train/verify) | yes (unless `skip_delete`) |
| `daytime-up` | `aicr-uat-day-<reservation>` (AWS) / `aicr-day-<reservation>` (GCP) — **stable** | yes | yes (prep→install) | no | **no — holds** |
| `daytime-down` | same stable name | no | no | no | yes (tears down the held cluster) |

The nightly per-run name isolates concurrent history (OCI tags, Terraform state) per run. The daytime name is **stable and reservation-tagged** so the evening `daytime-down` teardown and the nightly pre-batch guard can find the held cluster without tracking a run id. `skip_delete` is a nightly-only debugging escape and is ignored by the daytime lifecycles.

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

Two cron edges (UTC), plus a manual `workflow_dispatch` with an `action: up | down` input:

| Edge | Cron (UTC) | Action | Lifecycle dispatched |
|------|-----------|--------|----------------------|
| Morning handoff | `0 15 * * *` | `up` | `daytime-up` (provision + deploy + hold) |
| Evening teardown | `0 2 * * *` | `down` | `daytime-down` (tear down + release) |

The evening teardown runs ~2h before the nightly batch opens (`0 4 * * *`), leaving margin for a ~10–15 min destroy. A manual run to stand up or tear down the whole rotation by hand:

```bash
gh workflow run uat-daytime.yaml --repo NVIDIA/aicr --ref main -f action=up    # morning handoff
gh workflow run uat-daytime.yaml --repo NVIDIA/aicr --ref main -f action=down  # evening teardown
```

Different reservations run in parallel (independent hardware); a daytime run that finds its reservation still busy (an overrunning batch) *queues* on the lease rather than racing.

### If the evening teardown is missed

The teardown is not the only safety net. If a `daytime-down` is skipped or fails and the daytime cluster is still up when the nightly batch opens, DC2's [pre-batch guard](#pre-batch-guard) **blocks** the batch (fail-closed) rather than racing the held deployment. Recover by tearing the daytime cluster down — `gh workflow run uat-daytime.yaml -f action=down`, or a single `uat-run.yaml … -f lifecycle=daytime-down` for one reservation — then re-run the batch.

### Reaching the daytime cluster

Access is **out-of-band by design**: nothing here routes a kubeconfig or endpoint URL through the CI path, the evidence bundle, or the dashboard. Instead, the cluster's stable name is public knowledge and access is gated by **cloud IAM** on the daytime cluster — so an authorized operator mints their own kubeconfig directly and no credential ever transits CI:

```bash
# AWS — training cluster (aicr-uat-day-aws-h100)
aws eks update-kubeconfig --region us-east-1 --name aicr-uat-day-aws-h100

# GCP — inference cluster (aicr-day-gcp-h100)
gcloud container clusters get-credentials aicr-day-gcp-h100 --region <region>
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

Once DC3's `phase_serve` lands, the served workload is deployed automatically as part of `daytime-up`; until then it is a one-command manual apply on the held cluster.

## Pre-batch guard

A missed evening teardown must surface as a **blocked batch, never as silent contention** with the still-running daytime deployment. Before it provisions, every `nightly` run asserts that no daytime cluster (by the stable `aicr-uat-day-<reservation>` / `aicr-day-<reservation>` name) is still up on the target reservation. The check runs *after* the run has acquired the reservation lease and authenticated to the cloud, and *before* Bringup — so it fails fast rather than racing. It fails **closed**: only a definitive "cluster does not exist" (AWS `ResourceNotFoundException`, GCP `code=404`) clears the run to proceed; a throttle or auth error blocks the batch rather than being read as "clear."

If the guard trips, tear the daytime cluster down with `lifecycle=daytime-down` (which releases the reservation), then re-run the batch.

## Capacity assertions and the GCP posture

**AWS — post-lease assertion.** `uat-aws.yaml` asserts the EC2 capacity reservation is provisioned large enough for the GPU pool's desired count. Because the reservation lease is now the contention gate, this is **not** a race-and-fail pre-flight: it checks the reservation's `TotalInstanceCount` (its fixed provisioned size), not the momentary `AvailableInstanceCount`. A genuinely undersized/exhausted reservation still fails; transient contention (another run's not-yet-released nodes) no longer does, because the lease already guaranteed we are the only run consuming the reservation.

**GCP — actuator-time failure (decided posture).** `uat-gcp.yaml` has **no** pre-flight capacity/quota assertion, and DC2 deliberately did **not** add one. GCP relies on the GKE actuator failing at provision time if the reservation is exhausted. With the reservation lease serializing contending runs, a provision-time failure means a genuinely undersized/exhausted reservation, not a race — so a symmetric gcloud reservation check would add a second cloud API surface without changing the outcome. This is a recorded decision, not an oversight; there is intentionally no capacity step in the GCP pipeline.

## How queuing works (the reservation lease)

The lease is a GitHub Actions concurrency group keyed by reservation name — `uat-<reservation>` (for example `uat-aws-h100`) — declared on `uat-run.yaml` with `cancel-in-progress: false`. Two runs that target the *same* reservation serialize: the second waits until the first (including its teardown) finishes. Two runs that target *different* reservations share no group and run in parallel, because they are independent hardware.

This replaces the previous behavior, where a second run hitting a busy AWS reservation hard-failed on the capacity check. Now it queues.

**The one-in-progress-plus-one-pending limit.** GitHub concurrency holds at most one in-progress run plus one pending run per group. If a *third* run is queued for a reservation that already has one in-progress and one pending, GitHub cancels the older pending run and the newest takes its place. At launch this is acceptable: there are two reservations, each contended by at most the nightly cron plus an occasional ad-hoc dispatch. A run cancelled this way is *superseded*, not failed. So that a dropped request is never silent, the `uat-superseded-notice.yaml` observer watches for it: triggered on `workflow_run: completed` for `UAT Run`, it classifies a cancelled run that never started a job as a supersede (versus a genuine mid-run cancel) and emits a job-summary entry plus a `::warning`. (The nightly controller reconciles the same signal synchronously for the cells it dispatches; a DC6 regression guard, #1279, will exercise the observer.) If deeper queuing is ever needed (many requesters per reservation), the escalation path is the *Deferred* standing broker service — a pull-based queue rather than GitHub concurrency — recorded in the epic (#1264).

## The version matrix

The nightly batch runs a **cross-version regression** per reservation: `main` (built from source at tip) plus the previous **N** stable releases, so an older stable `aicr` is re-checked against today's cluster. `uat-broker schedule` orders the cells `main`-first, then releases in descending semver order; the controller runs them **sequentially** on the reservation (each cell dispatched through `uat-run.yaml`, so they share the lease) and **time-boxes** the batch — once the deadline passes it stops dispatching, so the in-flight cell finishes and the remaining (oldest) releases are dropped, guaranteeing `main` and the freshest releases always land.

**Release cells install released artifacts, not source.** A `main` cell builds the `aicr` binary + validator/agent images from the checked-out tree. A release cell (`aicr_version=vX.Y.Z`) instead downloads the released `aicr` binary at that tag; the released binary self-resolves its own version's validator images (`…/aicr-validators/<phase>:vX.Y.Z`) and snapshot agent (`ghcr.io/nvidia/aicr:vX.Y.Z`), so no images are built for release cells. Each run's summary records its `aicr_version` (`main` or the tag).

**Release cells verify what they install.** The `install-aicr-release` composite action does two checks before a downloaded binary is used, and **fails closed** on either: (1) *integrity* — the archive matches its `aicr_checksums.txt` entry; and (2) *provenance* — `cosign verify-blob-attestation` validates the SLSA Build Provenance v1 attestation goreleaser ships inside the archive (`aicr-attestation.sigstore.json`). The verifier does not trust *any* NVIDIA release signer: it derives the certificate-identity regexp from the requested `aicr_version`, so **only the attestation for that exact release tag** is accepted (`on-tag.yaml@refs/tags/<that-version>`, issuer `token.actions.githubusercontent.com`) — an attestation for a different tag is rejected. The attestation's subject is the binary's own digest, so this also binds authenticity to the exact bytes that run — not to the same-release checksums manifest. A release whose binary is unattested, or whose attestation does not verify, aborts the cell rather than running an unverified `aicr`.

**Tunables** — workflow inputs on `uat-nightly-batch.yaml` (these are the scheduled-run defaults):

- `previous_n` — stable releases below `main` to run per reservation (default `2`; `0` = `main` only).
- `deadline_offset_hours` — hours after batch start to stop dispatching new cells (default `5`). The controller job watches each cell sequentially and GitHub caps a hosted job at 6h, so this stays below that ceiling (and the job's own `timeout-minutes`) to keep the graceful drop-oldest reachable rather than being killed mid-cell.

To test a single released version by hand: `gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main -f reservation=aws-h100 -f aicr_version=v1.2.3`. (`--ref main` dispatches the nightly-path revision of the workflow, not your feature branch's.)

## Adding a reservation

Reservations are data, not code. To onboard a new reserved pool, add a row to `infra/uat/reservations.yaml`:

```yaml
- name: aws-b200          # the lease key; becomes concurrency group uat-aws-b200
  cloud: aws              # aws | gcp — selects which pipeline (EKS vs GKE) provisions
  reservation-id: cr-...  # the cloud capacity-reservation id (GCP uses the full path)
  accelerator: b200
  gpu-count: 8
  cluster-config-path: tests/uat/aws/cluster-config-b200.yaml
  test-config-dir: tests/uat/aws/tests
```

No broker, workflow, or Go change is needed — the nightly batch enumerates rows from the registry, and `uat-run.yaml` resolves them. The unit of sequencing is the *reservation*, so a new GPU type in an existing cloud simply runs in parallel with the others on its own lease. (Provisioning is per *cloud*: the same `uat-aws.yaml` pipeline provisions any AWS accelerator from the row's `cluster-config-path`; you do not add a per-accelerator workflow.)

The values in this file are identifiers, **not secrets** — a reservation-id grants no access on its own; access to the reserved capacity is governed by cloud IAM/ACLs bound to the CI federation identity (see `infra/uat-aws-account/` and `infra/uat-gcp-account/`). They are safe to commit.

## Roadmap

What ships now is the lease, the data-driven dispatch surface, the time-boxed nightly version matrix (`main` + previous-N stable releases, release cells installing released artifacts), superseded-run surfacing (the controller flags a dropped cell inline; the `uat-superseded-notice.yaml` observer catches ad-hoc dropped runs), per-intent selection, the daytime provision-and-hold / teardown / pre-batch-guard mechanics, and the DC8 [daytime human-access scheduler](#daytime-human-access-deployment) (`uat-daytime.yaml`) — one held deployment per cloud each working day, torn down before the batch, with out-of-band access. Still to come:

- **Served-inference CUJ (DC3).** The `phase_serve` step an `intent=inference` run — nightly *and* the daytime inference cluster — will exercise on the deployed inference stack, deploying the served `DynamoGraphDeployment` automatically instead of the current manual apply.
- **Both flavors per cloud during the day.** Blocked on capacity — one reservation cannot hold both a daytime cluster and the nightly batch at once. Pulls once more infra lands.
