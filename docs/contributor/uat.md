# UAT Day/Night Cycle and Reservation Broker

**AICR's real-hardware UAT runs on a small set of reserved GPU pools that must be time-shared.** The day/night broker (issue #1274) arbitrates that scarce capacity so contending runs *queue* instead of racing the hardware, driven entirely by a checked-in registry. This page explains the operating model, how to request a run, how queuing behaves, and how to add a reservation.

## The day/night cycle

Each reserved GPU pool follows a daily cycle, with every phase acquiring the *same* per-reservation lease so CI and human use never overlap on one reservation:

- **Night — the nightly batch.** On a cron, `uat-nightly-batch.yaml` runs one UAT pass per reservation (provision → CUJ → evidence → publish → teardown).
- **Morning — handoff.** Once the batch drains a reservation, the daytime human-access deployment is stood up on it (owned by DC2/DC8; not yet implemented).
- **Day — human use.** The daytime cluster is used outside CI.
- **Evening — teardown.** The daytime cluster is torn down before the next night batch.

The phases are independently scheduled (cron edges), not chained: the per-reservation lease — plus a pre-batch guard (DC2) — keeps them from overlapping, so a crashed or overrunning phase never orphans the reservation. A hosted GitHub Actions job is capped at the runner's timeout (hours, not a whole working day), so a single lease-holding run cannot span the day; the lease only needs to cover the brief transition windows, and the steady-state daytime cluster's existence is guarded by its lease-tag rather than a continuously held run.

> The morning handoff, the daytime deployment, and the evening teardown are owned by sibling work (DC2 and DC8) and are not yet wired up. What ships today is the **night side** (the nightly batch) plus the **lease + dispatch surface** every phase builds on.

## Requesting a UAT run

All UAT runs go through one entry point, `uat-run.yaml` — the shared dispatch surface that owns the reservation lease. To request a run, dispatch it with a reservation name from the registry:

```bash
gh workflow run uat-run.yaml --repo NVIDIA/aicr --ref main -f reservation=aws-h100
```

`uat-run.yaml` resolves the reservation row, then invokes the cloud-appropriate reusable pipeline (`uat-aws.yaml` or `uat-gcp.yaml`). A typo'd reservation name fails fast in the resolve step (the `uat-broker` helper exits *not found*). For manual debugging, `skip_tests` and `skip_delete` inputs are available.

The nightly batch and (later) the daytime handoff call this *same* surface, so every run for a reservation contends on one lease.

## How queuing works (the reservation lease)

The lease is a GitHub Actions concurrency group keyed by reservation name — `uat-<reservation>` (for example `uat-aws-h100`) — declared on `uat-run.yaml` with `cancel-in-progress: false`. Two runs that target the *same* reservation serialize: the second waits until the first (including its teardown) finishes. Two runs that target *different* reservations share no group and run in parallel, because they are independent hardware.

This replaces the previous behavior, where a second run hitting a busy AWS reservation hard-failed on the capacity check. Now it queues.

**The one-in-progress-plus-one-pending limit.** GitHub concurrency holds at most one in-progress run plus one pending run per group. If a *third* run is queued for a reservation that already has one in-progress and one pending, GitHub cancels the older pending run and the newest takes its place. At launch this is acceptable: there are two reservations, each contended by at most the nightly cron plus an occasional ad-hoc dispatch. A run cancelled this way is *superseded*, not failed. So that a dropped request is never silent, the `uat-superseded-notice.yaml` observer watches for it: triggered on `workflow_run: completed` for `UAT Run`, it classifies a cancelled run that never started a job as a supersede (versus a genuine mid-run cancel) and emits a job-summary entry plus a `::warning`. (The nightly controller reconciles the same signal synchronously for the cells it dispatches; a DC6 regression guard, #1279, will exercise the observer.) If deeper queuing is ever needed (many requesters per reservation), the escalation path is the *Deferred* standing broker service — a pull-based queue rather than GitHub concurrency — recorded in the epic (#1264).

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

What ships now is the lease, the data-driven dispatch surface, and a main-only nightly batch. Still to come:

- **Version matrix (DC1 follow-up / DC5).** The nightly batch will expand into a time-boxed, `main`-first, previous-N-stable-releases schedule (`uat-broker schedule`), each release row installing the released `aicr` images at that version.
- **Per-intent shaping + daytime deployment (DC2 / DC8).** Selecting the test config and recipe by `intent`, and the morning-handoff daytime human-access cluster.
