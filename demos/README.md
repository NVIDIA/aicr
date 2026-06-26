# Demos

Runbooks for testing and demonstrating AICR end-to-end workflows on live clusters.

## Available Demos

| Demo | Description |
|------|-------------|
| [cuj1-training.md](cuj1-training.md) | CUJ1 (training) - EKS + GKE end-to-end, plus a config-driven GKE + signed-evidence variant |
| [cuj1-slinky-slurm.md](cuj1-slinky-slurm.md) | CUJ1 - Slinky Slurm on EKS / GKE / Kind (recipe → bundle → validate → `srun`) |
| [cuj2-inference.md](cuj2-inference.md) | CUJ2 (inference) - EKS + GKE end-to-end with the Dynamo platform |
| [cuj2-demo.md](cuj2-demo.md) | CUJ2 (inference) - Annotated slide-style demo walkthrough (training vs inference) |
| [recipe-data-architecture.md](recipe-data-architecture.md) | Recipe metadata system: inheritance, criteria matching, deployment order, runtime external data |
| [validation-acceptance.md](validation-acceptance.md) | Validation acceptance runbook (snapshot/recipe/bundle/validate across all phases) |
| [end-to-end-cli.md](end-to-end-cli.md) | End-to-end CLI demo (includes the runtime external-data `--data` flow) |
| [query.md](query.md) | Querying hydrated recipes with dot-path selectors |
| [evidence.md](evidence.md) | Recipe evidence demo (validate emit + verify) |
| [evidence-demo-slides.html](evidence-demo-slides.html) | Recipe evidence demo — slide deck |
| [evidence-demo.sh](evidence-demo.sh) | Interactive split-leg evidence walkthrough (validate on VPN → publish off VPN → verify) |
| [provenance.md](provenance.md) | Binary & image SLSA L3 provenance demo (verify, SBOM, Rekor, in-cluster enforcement) |
| [provenance-demo-slides.html](provenance-demo-slides.html) | Build provenance — slide deck |
| [provenance-demo.sh](provenance-demo.sh) | Interactive consumer-side verification walkthrough |
| [bundle-attestation.md](bundle-attestation.md) | Bundle attestation demo (`aicr bundle --attest` + `aicr verify` trust levels) |
| [bundle-attestation-demo-slides.html](bundle-attestation-demo-slides.html) | Bundle attestation — slide deck |
| [bundle-attestation-demo.sh](bundle-attestation-demo.sh) | Interactive bundle sign + verify + tamper walkthrough |
| [examples/CUJ2-Test-Report.md](examples/CUJ2-Test-Report.md) | Dated historical capture (2026-03-13) of a CUJ2 inference run — example test report, not a runbook |

## Recording Test Runs

Use the `script` command to capture a terminal session for sharing or archival:

```shell
script session.log
# ... run demo steps ...
exit  # stops recording
```

The raw log contains terminal escape codes from your shell prompt. Extract key events with:

```shell
cat session.log \
  | sed 's/\x1b\[[0-9;]*[a-zA-Z]//g' \
  | sed 's/\x1b\][^\x07\x1b]*[\x07]//g' \
  | sed 's/\x1b\][^\x1b]*\x1b\\//g' \
  | sed 's/\x1b[()][A-Z0-9]//g' \
  | sed 's/\x1b\[[?][0-9;]*[a-zA-Z]//g' \
  | sed 's/\x0d//g; s/\x07//g; s/\x08//g; s/\x0f//g' \
  | grep -E '^\[cli\]|^Installing |^Deploying |^Deployment |^Error|^Script '
```

This strips ANSI escape codes and filters to AICR log lines, deploy script progress, and errors.

### Writing a Test Report

From the cleaned output, create a markdown report covering:

1. **Environment** - AICR version, cluster type, node counts, OS
2. **Steps executed** - commands and key output for each step
3. **Validation results** - table of phases, pass/fail counts, per-validator status
4. **Workload verification** - pod status, API response
5. **Issues found** - any failures, workarounds, or bugs discovered

See [examples/CUJ2-Test-Report.md](examples/CUJ2-Test-Report.md) for an example.
