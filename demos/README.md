# Demos

Runbooks for testing and demonstrating AICR end-to-end workflows on live clusters.

## Available Demos

| Demo | Description |
|------|-------------|
| [cuj1-eks.md](cuj1-eks.md) | CUJ1 - EKS cluster setup |
| [cuj1-gke.md](cuj1-gke.md) | CUJ1 - GKE cluster setup |
| [cuj2.md](cuj2.md) | CUJ2 - EKS inference with Dynamo |
| [cuj2-demo.md](cuj2-demo.md) | CUJ2 - Annotated demo walkthrough (training vs inference) |
| [cuj2-eks.md](cuj2-eks.md) | CUJ2 - EKS variant |
| [cuj2-gke.md](cuj2-gke.md) | CUJ2 - GKE variant |
| [e2e.md](e2e.md) | End-to-end CLI demo |
| [valid.md](valid.md) | Validation demo |
| [data.md](data.md) | External data directory demo |
| [ext.md](ext.md) | Extension demo |
| [query.md](query.md) | Querying hydrated recipes with dot-path selectors |
| [attestation.md](attestation.md) | Bundle attestation demo |
| [evidence.md](evidence.md) | Recipe evidence demo (validate emit + verify) |
| [s3c.md](s3c.md) | Supply chain security demo |

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
