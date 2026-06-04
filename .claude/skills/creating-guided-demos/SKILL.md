---
name: creating-guided-demos
description: Scaffolds an interactive guided demo script (demos/*.sh), live or self-paced, with the Frame → Tell → Show → Close pattern. Triggers on "demo script", "guided walkthrough", "demos/*.sh", "live demo".
---

# Creating Guided Demos

## Overview

A guided demo is a **narrative, not a command dump** — and it serves two readers equally: a presenter driving it live for a room, and a lone user stepping through it to learn or reproduce the flow. Either way, at every step they know *why* they're here, *what's* about to happen, and *what just happened*.

Four beats: **Frame → Tell → Show → Close.** The script is the guide — it paces itself, explains each step, and streams the real tool output so what's watched is the actual thing working.

## When to use

- Building a `demos/*.sh` to present a CLI/workflow live **or** to let a user self-walk it (onboarding, learning, reproducing a result).
- Turning a bare sequence of commands into a self-paced, narrated walkthrough.
- **Not for:** non-interactive CI scripts, one-off scratch scripts, or pure reference docs.

## The four beats

1. **Frame** — the *why*, for whoever's following (a room, or one reader). One or two sentences: what's missing/broken and why it matters. No commands.
2. **Tell** — name the steps before running them, so they have a map. Flag what's slow, what needs the network/VPN, what to watch for.
3. **Show** — run the real commands one at a time, pausing before each, streaming their **full** output. Pre-stage the slow/fragile parts; keep a fallback for anything that can fail.
4. **Close** — one line on what just happened, plus pointers: docs/ADR, related demos, the source of truth.

## Mechanics

Copy [`skeleton.sh`](skeleton.sh). It gives you:

- **`banner` / `note` / `pause` / `run` helpers** — numbered sections, dim explanations, an Enter-gated pause per step, and a `run` that echoes then streams output verbatim. Plus an optional `run_expect_fail` for steps where a non-zero exit is the expected result (uses `|| rc=$?` so `set -euo pipefail` doesn't abort).
- **One env config block at the top** — every input is an env var; *required* ones fail fast (validate producer-only inputs inside the producer branch so the skip flag still works), the rest default. Each runner overrides only what differs.
- **Re-runnability** — `DEMO_NO_PAUSE=1` runs unattended; a skip flag (e.g. `SKIP_SETUP=1`) reuses pre-staged state. Skip contract: **guard the producer, assert the artifact exists (fail loudly), then run the fast steps either way.**

## Rules

- **Show real output.** Never `>/dev/null`, never fabricate. Watching the real tool *is* the point.
- **Don't fake success.** Never weaken a gate/threshold to force green — an honest failure is a valid beat.
- **Respect time.** Anything multi-minute (deploys, benchmarks, sign-in) is pre-staged; live steps are seconds.
- **Commit-ready by default.** No home paths, kubeconfig paths, or private refs; generic defaults. (A throwaway personal demo can relax this.)

## Worked example

`demos/evidence-demo.sh` — the split-leg recipe-evidence walkthrough: Frame (the reachability gap) → Tell (validate → publish → verify) → Show (each leg, streamed, pre-staged with fallbacks) → Close (ADR-007 + related demos). Reads the same whether presented live or run solo.

## Common mistakes

- Command dump, no Frame/Close → the runner never learns *why*, or what they saw.
- Slow producer step run live → blows the time budget; pre-stage it and add a skip flag.
- Hardcoded paths/criteria → not reusable; use env vars and fail fast on required ones.
- Faked/suppressed output or a weakened gate → destroys the trust the demo exists to build.
