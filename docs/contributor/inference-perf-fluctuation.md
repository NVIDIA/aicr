# Inference-Perf Validation — TTFT Fluctuation & Worker-Stall Investigation

**Date:** 2026-06-04
**Author:** Yuan Chen (with Claude Code)
**Related:** NVIDIA/aicr #1192, #1193, #1194, #1196
**Status:** root cause characterized; mitigations shipped/in-PR; long-term fix proposed

---

## 1. Executive summary

The `inference-perf` validator intermittently **failed healthy GPU clusters** at
2048 concurrency — sometimes with a catastrophic time-to-first-token (TTFT) p99
of 9–45 s, sometimes just over the 1000 ms gate. Extensive testing across three
H100/RTX clusters showed:

- **No genuine deployment or hardware failure** was ever found. GPUs were healthy
  throughout; clusters delivered ~108–140k tok/s.
- The failures were the **validator/benchmark/gate mis-firing**: a fixed-but-real
  version-skew panic, a transient/stochastic worker stall, and a too-tight
  latency gate at the saturation knee producing **false negatives**.
- An early hypothesis (aiperf↔worker **CPU contention**) was **refuted by direct
  measurement** — the GPU node's CPU is never contended.

**Mitigations shipped/in-PR:** dynamo runtime image bump 0.9.0→1.0.2 (#1193, the
panic fix); reproducible gate — TTFT `<= 2000` ms + pinned AIPerf inputs (#1196);
centralized dynamo image reference + drift guard (#1194).
**Long-term:** Dynamo 1.2 load-aware least-loaded routing
(`DYN_ROUTER_MODE=least-loaded`, shipped for #1197), plus a sub-knee,
throughput-primary gate.

---

## 2. Symptom / problem statement

- On **EKS H100** (`p5.48xlarge`, 8× H100), the `inference-perf` check at
  2048 in-flight (256/GPU) **failed intermittently** on a cluster that had passed
  cleanly two days earlier (§9). TTFT p99 swung **664 ms → 45 s** run-to-run;
  throughput 25k–127k.
- User report: *"we used to get consistently good performance on EKS H100 until
  today."* Historical EKS H100 baseline: **108,790 tok/s / 688 ms**, on par with GKE.
- Two visible failure shapes:
  1. **Severe stall** — one worker's in-flight queue backs up to the
     `max_num_seqs` cap (1022) or partway (643) while its GPU sits at low util;
     TTFT 9–45 s.
  2. **Knee jitter** — balanced, full-throughput runs whose TTFT p99 lands just
     over the 1000 ms gate (1358 / 1670 ms) → false negative.

---

## 3. Environment & setup

### Clusters

| Cluster | Platform | GPU node | GPUs | Region |
|---|---|---|---|---|
| EKS H100 | EKS | `p5.48xlarge` (192 vCPU) | 8× H100-SXM | us-east-1 |
| GKE H100 | GKE | `a3-megagpu-8g` (208 vCPU) | 8× H100-SXM | us-central1 |
| EKS RTX Pro 6000 | EKS | RTX PRO 6000 | 8 | us-west-2 |

Non-GPU nodes were small on both EKS H100 paths: EKS H100 system nodes =
`m7i.xlarge` (4 vCPU); GKE = `n2-standard-8` cpu-worker (8 vCPU) + `e2-standard-4`
system nodes (4 vCPU).

### Workload / gate

- Model **Qwen/Qwen3-8B** (BF16), PVC weights cache.
- **256 concurrency/GPU → 2048 in-flight** (the per-GPU "knee").
- Recipe gate (original): throughput `>= 50000`, TTFT p99 `<= 1000` (gb200 already 2000).
- Validator deployed a `DynamoGraphDeployment` (Dynamo frontend + 8 vLLM decode
  workers, 1 GPU/worker via DRA) + an **AIPerf** benchmark Job.

### Tooling built for the investigation

- Locally-built `performance` validator images pushed to each cluster's registry
  (ECR / GCP Artifact Registry), invoked via `AICR_VALIDATOR_IMAGE_REGISTRY` +
  `AICR_VALIDATOR_IMAGE_TAG` overrides. Tags used: `dynamo102` (1.0.2 bump),
  `dynamo102b` (+CPU req +aiperf nodeAffinity), `dynamo102c` (+CPU req only),
  `dynamo102d` (+CPU req +aiperf CPU req), `dynamo102e` (+`kv` routing).
- Live cluster-state watchers capturing per-worker `Running`/`Waiting`/KV-cache,
  per-GPU `nvidia-smi` (util/clocks/power/throttle/ECC via the gpu-operator
  driver pod), worker→GPU-UUID mapping, frontend serve/panic signals, AIPerf
  pod node, and **node CPU PSI + loadavg** (`/proc/pressure/cpu`, `/proc/loadavg`
  via the hostPID driver pod — `kubectl top`/metrics-server was unavailable).

### Operational caveat: a GPU driver restart requires a DRA plugin restart

Restarting the GPU driver pod (`nvidia-driver-daemonset-*` in `gpu-operator`) on a
node **also requires restarting the NVIDIA DRA kubelet-plugin** pod
(`nvidia-dra-driver-gpu-kubelet-plugin-*` in `nvidia-dra-driver`) on that same
node. The plugin caches device state from before the driver reload, so a
driver-only restart leaves it emitting invalid CDI specs — every GPU
`ResourceClaim` then fails kubelet `NodePrepareResources` with
`FailedPrepareDynamicResources: … invalid device, empty device edits`, the vLLM
decode workers hang in `ContainerCreating`, and the validator times out at its
phase deadline with no benchmark run. Restart order: **driver pod → DRA
kubelet-plugin → confirm node `nvidia.com/gpu` allocatable is restored → launch
workloads.**

---

## 4. Investigation — tests run, in order

### 4.1 Dynamo frontend panic (version skew) — a real, fixed bug

- **Symptom:** on EKS RTX Pro 6000, the frontend reached 1/1 but never served; logs showed
  `thread 'tokio-runtime-worker' panicked … Unfold must not be polled after it
  returned Poll::Ready(None)` (24×) and `KVStoreDiscovery … bucket missing for
  query=AllEndpoints` (97×) — worker discovery died, `/v1/chat/completions` hung.
- **Root cause:** upstream `futures-util 0.3.31` bug ([ai-dynamo/dynamo#7328]),
  fixed in `futures-util 0.3.32`, first shipped in **dynamo v1.0.0**, never
  backported to 0.9.x. AICR ran a **version skew**: the `dynamo-platform`
  operator chart was `1.0.2` but the runtime **image tags were still `0.9.0`**
  (hardcoded literals the chart bump never touched).
- **Fix & confirmation:** bump runtime images 0.9.0→1.0.2 (#1193). Rebuilt the
  validator, ran on EKS RTX Pro 6000 @2048: **73,993 tok/s / 537 ms PASS, zero panics**
  (vs 24 on 0.9.0). Verified `futures-util` pin per `Cargo.lock` at each tag.

### 4.2 The worker-stall pattern (EKS H100)

Instrumented runs captured the stall directly. Representative (run "r1",
`dynamo102`, round-robin, 2048):

```text
worker        Running  Waiting  GPU-util  clock     throttle  power
<stalled>      1022      151      ~1–27%   1980MHz   0x0       ~130W   ← backed up, GPU starved
the other 7    16–366     0       86–100%  1980MHz   0/pcap    520–700W
```

- The stalled worker's **GPU is healthy and *under*-driven** (full clock, no
  throttle, low util) yet holds a huge queue → bottleneck is **upstream of GPU
  compute**. Under **round-robin** (dynamo default), the router keeps feeding the
  slow worker its 1/8 share, so its queue backs up to the cap.
- **Stochastic & device-independent:** across runs the stalled worker was a
  *different* physical GPU (idx 2 then idx 6) → not a bad GPU.
- GPU health verified clean every time: full 1980 MHz clocks, power cap 700 W,
  **0 ECC / 0 row-remap / 0 XID**, no thermal/SW throttle (idle or under load).

### 4.3 Fix attempts (config variants, EKS H100, 2048, dynamo 1.0.2)

| Config | router | worker CPU req | aiperf node | result | finding |
|---|---|---|---|---|---|
| baseline `dynamo102` | round-robin | none | GPU node | r1 **9.2 s** FAIL (peak 1022); r2 1030 ms PASS | stall intermittent |
| `dynamo102b` +CPU req +aiperf nodeAffinity | round-robin | 16/32Gi | forced → 4-vCPU sys node | r3 50k / **1265 ms** FAIL | aiperf under-drives (sys node too small) — **backfired** |
| `dynamo102c` +CPU req only | round-robin | 16/32Gi | GPU node | r4 **122k / 741 ms** PASS | clean |
| `dynamo102d` +CPU req +aiperf CPU req | round-robin | 16/32Gi | GPU node (forced by req) | r5–r8: **1/4 pass** (708 P; 1358/1670/**9020** F, peak 643) | still intermittent |
| `dynamo102e` `kv` routing | **kv** | 16/32Gi | GPU node | k1 **13.9 s** FAIL (skew 303↔1); k2 114k/897 ms PASS | kv concentrates on low-diversity prompts — **worse** |

Key learnings from the variants:
- **Fix B (banish aiperf off the GPU node) backfired:** EKS H100's only non-GPU
  nodes are 4-vCPU; aiperf couldn't drive 2048 there (~446 in-flight) → 50k.
- **An aiperf CPU request forces co-location** on the GPU node (16 vCPU can't fit
  a 4-vCPU node) — deterministic placement, but co-located with the workers.
- **`kv` routing is worse** here: its prefix-affinity term concentrates AIPerf's
  ~94%-prefix-hit synthetic prompts onto one worker.

### 4.4 CPU-contention hypothesis — REFUTED by measurement

The stall looked like CPU starvation, so we measured node CPU during runs:

```text
Node CPU PSI (some avg10):  0.00 – 1.54   ≈ 0   (no CPU stall)
Node load1:                 3 – 24  of 192 vCPU  (<13%; ~170 idle vCPUs)
Per-worker CPU:             ~2 cores each
```

`some avg10 ≈ 0` means tasks were essentially never stalled waiting for CPU.
**There is no node CPU contention.** AIPerf *does* co-locate on the GPU node, but
co-location ≠ contention. The "aiperf steals worker CPU → stall" hypothesis is
**not supported by the data**, and worker CPU/memory requests are therefore *not*
justified as a stall fix.

### 4.5 Stall-reproduction hunt — the stall is transient

Re-ran the exact stalling config (round-robin, no CPU req, `dynamo102`) the next
morning, 6 times back-to-back with the CPU sampler:

| run | throughput | TTFT p99 | peak Running | result |
|---|---|---|---|---|
| stall1 | 127,036 | 722 ms | 256 | passed |
| stall2 | 118,376 | 939 ms | 256 | passed |
| stall3 | 108,739 | 973 ms | 256 | passed |
| stall4 | 122,189 | 765 ms | 256 | passed |
| stall5 | 116,678 | 664 ms | 256 | passed |
| stall6 | 114,380 | 943 ms | 256 | passed |

**6/6 passed, all balanced (peak 256 = fair share, no backup).** The severe stall
(r1 1022/9.2 s; r8 9.0 s) did **not** reproduce. It was happening the night before
and **cleared overnight** → transient/stochastic, not a stable property.

### 4.6 GKE deconfounding — the 1.0.2 bump is safe

GKE H100 with the **same** validator/config:

| config | throughput | TTFT p99 | result |
|---|---|---|---|
| GKE 0.9.0 | 108,164 | 586 ms | PASS |
| GKE 1.0.2 (g1) | 137,105 | 474 ms | PASS |
| GKE 1.0.2 (g2) | 140,641 | 227 ms | PASS |
| GKE 1.0.2 (g3) | 138,700 | 365 ms | PASS |

GKE 1.0.2 ≥ its own 0.9.0 baseline and 4/4 clean → **the 1.0.2 bump is not a
regression** (it's an improvement). The earlier worry that 1.0.2 caused the
fluctuation is cleared; the difference is environmental/gate-design, not the bump.
(Note: 4 GKE runs is a small sample — GKE was *never observed* to stall, but that
does not prove immunity.)

### 4.7 Same-window tweak vs. no-tweak, and a driver/DRA restart (2026-06-04)

A later afternoon window — when the fluctuation had returned — was used to
isolate whether the gate-hardening changes (relaxed TTFT + pinned AIPerf inputs)
themselves affect the fluctuation, by running **both** configs back-to-back on
EKS H100.

**No-tweak** (image `dynamo102`, original AIPerf script, recipe TTFT 1000) —
byte-identical to the 4.5 morning setup, same `diag-cpu` collector:

| run | throughput | TTFT p99 | peak | result |
|---|---|---|---|---|
| s1 | 123,043 | 729 ms | 256 | passed |
| s2 | 84,049 | **4,155 ms** | 256 | failed (degraded) |
| s3 | 122,151 | 935 ms | 256 | passed |

**Tweak** (image `combo` = 1.0.2 + pinned AIPerf inputs, recipe TTFT 2000):

| run | throughput | TTFT p99 | peak | result |
|---|---|---|---|---|
| t1 | 48,920 | **30,743 ms** | 291 | failed (severe stall) |
| t2 | 111,472 | 1,183 ms | 256 | passed |
| t3 | 92,865 | 1,056 ms | 257 | passed |
| t4 | 124,479 | 603 ms | 256 | passed |
| t5 | 112,637 | 910 ms | 296 | passed |
| t6 | 92,590 | **4,844 ms** | 256 | failed (degraded) |

**No-tweak 2/3, tweak 4/6 — an identical ≈ 1/3 fluctuation rate in the same
window.** This confirms the fluctuation is **environmental/time-varying, not
introduced by the tweaks**. The tweaks' value shows in the passing runs: t2
(1,183 ms) and t3 (1,056 ms) would *fail* the old `<= 1000 ms` gate but pass at
`<= 2000 ms` — exactly the knee-jitter false-negative the relaxed ceiling
removes. Neither config prevents the severe stall (t1 30.7 s, s2 4.2 s); those
exceed even 2,000 ms and fail correctly. The recurring degraded signature is
**≈ 84–93 k tok/s at balanced peak Running 256** — degradation *without* an
obvious single-worker queue backup (s2, t6, and combo-c3 all share it).

**Driver + DRA restart probe.** To test whether stale GPU-driver state drives the
stall, the GPU driver pod was restarted (see the operational caveat in §3 — it
required a DRA kubelet-plugin restart too; the first attempt, `t7`, timed out in
`ContainerCreating` until the DRA plugin was also restarted). After a clean
driver + DRA restart, the single confirming run **`t8` passed** (110,847 tok/s,
1,274 ms, peak 229). One pass is **not** proof the restart fixes anything — at a
≈ 1/3 stall rate a single run has ≈ 2/3 odds of passing regardless — so a small
post-restart batch was run to look for a durable effect: **`t9` failed**
(77,537 tok/s, **12,295 ms**, peak 256 — the same balanced degradation) on the
freshly-restarted node, after which the batch was stopped. Post-restart **1 pass
/ 1 fail** is indistinguishable from the background ≈ 1/3 rate, so **the driver +
DRA restart does not fix the stall** — evidence the stall is *not* GPU-driver
state, consistent with a routing / vLLM-scheduler origin (see §7).

### 4.8 Serve-readiness cold-start timeout — a separate RTX PRO 6000 false-negative

A distinct failure surfaced on EKS RTX PRO 6000 (and *only* there): the phase
failed with `[TIMEOUT] timed out waiting for inference endpoint to serve
requests` even though the DGD was `successful`, all 8 workers `1/1`, and the
frontend `1/1`. This is the **same outer symptom as the dynamo 0.9.0 frontend
discovery panic (#1192)** but a **different root cause** — and #1192's panic is
already fixed in 1.0.2 (futures-util `0.3.31` → `0.3.32`; upstream
[ai-dynamo/dynamo#7328](https://github.com/ai-dynamo/dynamo/issues/7328) /
[#7346](https://github.com/ai-dynamo/dynamo/pull/7346), shipped in dynamo v1.0).
Verified the running frontend was genuine 1.0.2 by image digest, with
`/v1/models` populated, 24 discovery instances, and **no `Unfold` panic / no
`bucket missing`** in its logs.

Live probing of the wedged endpoint isolated it:

| Probe | Result |
|---|---|
| `/health`, `/v1/models` | 200 — model + 24 endpoints registered (discovery healthy) |
| `/v1/chat/completions` (first ever) | **200 in ~42 s** for 8 tokens |
| `/v1/chat/completions` (warm) | fast |

So the model serves — the **first inference is just slow** (~42 s: CUDA-graph
capture / JIT kernel warm on a fresh worker). The readiness probe
(`waitForEndpointReady`) polled with the generic **30 s** `HTTPClientTimeout`,
which **cancelled that legitimate first request mid-warmup**; each retry
restarts it, so no poll ever succeeded and the 5-minute window expired —
**before AIPerf (which has its own warmup phase) ever started**. It is
intermittent because RTX's cold first-token straddles the 30 s line (morning
runs landed \<30 s and passed; afternoon landed ~42 s and failed); H100/GB200
cold-start stays under 30 s, so they never tripped it. Independent of the AIPerf
determinism flags (the same tweaked image both passed and failed across runs)
and of concurrency.

**Fix:** a dedicated **120 s** serve-readiness probe timeout
(`InferenceEndpointProbeTimeout`) — clears observed cold-start with margin while
still fitting several polls inside the 5-minute window; AIPerf's own warmup then
absorbs steady-state. Validated on EKS RTX PRO 6000: **74,833 tok/s / 459 ms /
PASS** with the fix, vs repeated serve-timeouts before. A debug escape hatch,
`AICR_INFERENCE_PERF_NO_CLEANUP=1`, was added to leave the namespace / DGD /
workers / frontend / AIPerf Job in place for exactly this kind of post-mortem.

---

## 5. Key findings & insights

> These are also recorded as the consolidated findings comment on the
> investigation issue:
> [#1192 findings comment](https://github.com/NVIDIA/aicr/issues/1192#issuecomment-4623706346)

1. **Outlier-driven.** TTFT-p99 swings come from a small subset of requests
   queuing behind a transiently-backed-up worker, not uniform slowdown.
   **Throughput is the stable, discriminating signal; tail latency is the noise.**
2. **AIPerf co-locates with workers, but it is NOT resource contention.** The
   CPU-only generator runs on the GPU node, yet node CPU is idle (PSI ≈ 0, ~170
   idle vCPUs). *Measure, don't infer* — this corrected an earlier wrong call.
3. **Routing strategy + workload-generation config materially change the result.**
   round-robin is capacity-blind (force-feeds a slow worker → backup); `kv`
   prefix-affinity concentrates on low-diversity synthetic prompts; non-pinned
   AIPerf inputs add RNG variance. The gate measures the *test setup* unless pinned.
4. **The validation is a baseline / conformance floor, not optimal/peak.** Gating
   at the saturation knee (2048) — where the p99 curve is steepest — guarantees
   volatility. Gate **sub-knee, throughput-primary, with a generous TTFT ceiling.**
5. **The severe stall was stochastic / transient** — not reproducible the next
   day. A single run is an unreliable verdict.
6. **GPU hardware was healthy throughout** (clocks, ECC, throttle, XID all clean)
   — the fluctuation is software/routing/methodology, not hardware.
7. **`nvidia-smi` "GPU utilization" is a duty-cycle metric, not compute
   saturation** — a worker can read 100% util while under-fed; cross-check **power
   draw + achieved throughput**.
8. **Distinct, compounding failure modes** — the dynamo 0.9.0 discovery panic
   (version skew) is *separate* from the routing/knee fluctuation. Don't conflate.
9. **Cross-cluster variance (GKE consistent, EKS marginal at 2048) is
   unexplained** — same H100 GPU, near-same host CPU (208 vs 192 vCPU). Could be
   sampling, NUMA/pinning, or env. **Honest unknown.**
10. **Guiding principle:** the verdict must be `f(deployment health)`, not
    `f(RNG / knee jitter / benchmark config)`.
11. **Readiness gates must tolerate cold-start first-token latency.** A fresh
    worker's first inference (CUDA-graph capture / JIT warm) took ~42 s on RTX
    PRO 6000; the 30 s per-request probe timeout cancelled it and failed a
    healthy deployment (§4.8). Same *outer* symptom as the #1192 discovery panic,
    *different* root cause — discovery was healthy. Fixed with a 120 s probe
    timeout (`InferenceEndpointProbeTimeout`).

---

## 6. Mitigations & workarounds applied

| Change | What | Status | Notes |
|---|---|---|---|
| **#1193** | Bump dynamo runtime image 0.9.0 → 1.0.2 (all 5 manifest pins) | PR ready | The real fix for the discovery panic; aligns runtime with the 1.0.2 operator. Confirmed on EKS RTX Pro 6000 (panic gone) & GKE (improved). |
| **#1196** | Reproducible gate: TTFT `<= 2000` across overlays + pinned AIPerf inputs (`--random-seed`, token stddev 0, `--num-dataset-entries`, `--extra-inputs temperature:0`) + methodology docs | PR draft | 2000 ms passes healthy 708–1670 ms runs, still catches 9–45 s stalls 5–20×. Pending one confirming run. |
| **#1194** | Centralize the dynamo runtime image reference (single source of truth + registry-override parity + tag-vs-operator-chart drift guard) | issue | Absorbs former #1159. Prevents the version skew that caused #1193. |
| Operational | Run at **1024 concurrency** (`AICR_INFERENCE_PERF_CONCURRENCY_PER_GPU=128`) | env-only workaround | Sub-knee → stable everywhere (292–533 ms observed), zero rebuild. |
| **#1196** | Serve-readiness probe timeout 30 s → **120 s** (`InferenceEndpointProbeTimeout`) | PR draft | Fixes the §4.8 RTX cold-start false-negative; 42 s first-token no longer cancelled. Validated RTX PRO 6000: 74,833 / 459 ms PASS. |
| **#1196** | `AICR_INFERENCE_PERF_NO_CLEANUP=1` debug env var | PR draft | Leaves namespace/DGD/workers/AIPerf in place for post-mortem of a failed run. |
| **Rejected** | Worker CPU/memory requests as a "stall fix" | not adopted | Refuted: no CPU contention measured. Would be an unsupported change. |
| **Rejected** | aiperf nodeAffinity off GPU nodes (Fix B) | not adopted | Backfired — EKS H100's non-GPU nodes (4 vCPU) are too small to drive the load. |
| **Rejected** | `kv` routing on 1.0.2 | not adopted | Worse — prefix-affinity concentrates synthetic load (13.9 s). |

---

## 7. Proposed long-term solution

1. **Reproducible, baseline-shaped gate (#1196):** throughput-primary pass
   condition at a **sub-knee operating point** (e.g. 1024) with a **generous TTFT
   ceiling** (≥2000 ms) as a guardrail; deterministic AIPerf inputs. The verdict
   then reflects deployment health, not RNG or knee jitter.
2. **Load-aware routing on Dynamo 1.2** — route by each worker's active
   in-flight load (`DYN_ROUTER_MODE=least-loaded`) instead of capacity-blind
   round-robin or KV-overlap routing, so a transiently-slow worker stops
   receiving its full share at the saturation knee. Bump operator chart **and**
   runtime images together to avoid re-introducing version skew. *(Implemented
   for #1197: the inference-perf workload now defaults to `least-loaded`. KV
   routing remains available but lets a backed-up worker keep accumulating
   KV-overlap-matched requests, which least-loaded avoids.)*
3. **Version-drift guard (#1194):** single source of truth for the dynamo runtime
   image (repo+tag) + a `make` check tying the tag to the operator chart version,
   so the 0.9.0/1.0.2 skew can't recur.
4. **(Optional) isolate the load generator** on an adequately-sized non-GPU node —
   only worth it where such a node exists (GKE/EKS H100 here do not; their non-GPU
   nodes are ≤8 vCPU). Otherwise co-location on the big GPU node is fine.

---

## 8. Open questions

- **Exact stall mechanism** (when it occurs) is not pinned down — node CPU is not
  contended, so the leading remaining hypothesis is an *intra-worker* single-thread
  / GIL bottleneck (invisible to node-level load/PSI), or a transient
  software/discovery hiccup. Not confirmed.
- **Why EKS H100 fluctuated and GKE didn't** — same H100, near-same host CPU.
  Unexplained; candidates are NUMA/CPU-pinning differences, background-pod load,
  or simply small-sample luck (only 4 GKE runs).
- **What triggered the severe stalls the night of 2026-06-04 and cleared them by
  morning** — transient, root trigger unidentified.

---

## 9. References

- NVIDIA/aicr **#1192** — investigation issue (false-negative timeout); consolidated findings comment: [issuecomment-4623706346](https://github.com/NVIDIA/aicr/issues/1192#issuecomment-4623706346)
- NVIDIA/aicr **#1193** — dynamo runtime image bump 0.9.0 → 1.0.2 (panic fix)
- NVIDIA/aicr **#1194** — centralize dynamo image reference (SSOT + registry parity + drift guard)
- NVIDIA/aicr **#1196** — reproducible inference-perf gate (TTFT ≤ 2000 + pinned AIPerf inputs)
- ai-dynamo/dynamo **#7328** — `Unfold` / `futures-util 0.3.31` frontend panic (fixed in v1.0.0)
