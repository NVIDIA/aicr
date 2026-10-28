# kubectl aicr

The `kubectl aicr` plugin is how approvers inspect, approve and revoke
rollouts, and how anyone can check a plan's bundle against the AICR CLI. It
talks to the API server only; it never calls the operator.

## Install

Each AICR [GitHub release](https://github.com/NVIDIA/aicr/releases)
carries `kubectl-aicr-<os>-<arch>` for Linux and macOS on amd64 and arm64:

```shell
gh release download "v${VERSION}" --repo NVIDIA/aicr --pattern kubectl-aicr-linux-amd64
gh attestation verify kubectl-aicr-linux-amd64 --repo NVIDIA/aicr
install -m 0755 kubectl-aicr-linux-amd64 /usr/local/bin/kubectl-aicr
```

`kubectl` finds any `kubectl-aicr` on the `PATH`. Use the plugin from the same
release as the operator.

## Usage

```text
kubectl aicr COMMAND ROLLOUT [flags]
kubectl aicr reproduce [PLAN] [flags]
kubectl aicr help
```

Flags may come before or after the name. Every command accepts every flag:

| Flag | Default | Used by |
| --- | --- | --- |
| `--kubeconfig PATH` | `$KUBECONFIG`, then `~/.kube/config` | All |
| `--context NAME` | The current context | All |
| `--operator-namespace NAME` | `aicr-system` | `describe`, `reproduce`: where assessment reports and plans live |
| `--reason TEXT` | none; required | `approve`, `revoke`; at most 1024 characters |
| `--expires DURATION` | `2h` | `approve`: how long the approval stays valid; must be positive |
| `--dir PATH` | `aicr-reproduce` | `reproduce`: where the recipe is written |

Each command's API calls share a 30-second deadline. Output is plain text.

| Exit | Meaning |
| --- | --- |
| 0 | Success, or `help` |
| 1 | The command failed: kubeconfig, API error, admission denial or a refusal (below). The error is printed as `error: [CODE] message` |
| 2 | A usage error: unknown command or flag, a missing name or `--reason`, extra arguments |

## describe

```shell
kubectl aicr describe rollout-3f9a1c0d2b7e4a51-1
```

Prints what an approver decides on:

- the rollout, its runtime UID, and the transition (`<from> -> <to>`, or
  `none (initial install)`);
- its phase and outcome, deadline, approval (`required, pending`, `approved by
  … until …`, or the revocation), message, and the size and digest of its
  starting inventory;
- every step: release, operation (`Install`, `Upgrade`, `Verify`, `Remove`,
  `Adopt`), AICR's assessment, whether it changes CRDs, and its state (`in
  flight`, `Ready`, `Failed`, …);
- AICR's `upgrade-check` report from the operator namespace, verified against
  the digest the rollout records: each component's change, versions, verdict
  and notes. Without one it prints `Assessment: none`;
- under "Recorded steps", each transition record's precondition (`before:`)
  and steps. The operator performs only the apply; approving acknowledges the
  steps before it, and `acknowledge` the steps after it;
- each acknowledgement already given (`Acknowledged: <release> by <user>`).

## acknowledge

```shell
kubectl aicr acknowledge rollout-3f9a1c0d2b7e4a51-1
```

For a rollout waiting in `AwaitingAcknowledgement`: records, as you, that the
post-apply steps of the release it waits on are done (for NVSentinel, a
tripped circuit breaker reset and the CRD verified), and the rollout continues.
It refuses a rollout that is not waiting. Approvers only; the username is the
one the API server reports.

## approve

```shell
kubectl aicr approve rollout-3f9a1c0d2b7e4a51-1 --expires 2h --reason "Maintenance window"
```

Asks the API server who you are (a `SelfSubjectReview`), then sets the
rollout's `spec.approval` to your username, the expiry and the reason. Prints
`rollout <name> approved by <user> until <time>`.

It refuses a rollout that has ended, is already approved, or needs no
approval. An approval can be neither changed nor extended: if it expires, the
step in flight finishes, the rollout ends `Expired`, and a
[retry](operations.md#retrying) needs a new approval.

## revoke

```shell
kubectl aicr revoke rollout-3f9a1c0d2b7e4a51-1 --reason "Training job not checkpointed"
```

Records your username, the time and the reason as the revocation. The step in
flight finishes, no further step starts, and the rollout ends `Revoked`. It
refuses a rollout that is not approved, already revoked, or has ended; the
API server also refuses one in `Validating`.

## reproduce

```shell
kubectl aicr reproduce                      # the active plan, else the candidate
kubectl aicr reproduce "sha256:<digest>"    # or a plan ConfigMap name, aicr-plan-<16 hex>
```

Writes the plan's recipe to `<dir>/recipe.json` and prints the plan, the AICR
image it was rendered with, the operator's bundle reference, and three
commands to run yourself: a `docker run` of that AICR image rendering the
recipe with the plan's flags, an `oras pull` of the operator's bundle, and a
`diff -r` of the two. Zero diff means the operator deployed exactly what the
AICR CLI renders ([verify the bundle yourself](index.md#verify-the-bundle-yourself)).

It changes nothing in the cluster. It refuses a plan rebuilt from Applications
(it has no recipe), a plan that does not record its render flags (written
before v0.1.0-rc.3), and a recipe whose content does not match its recorded
digest.

## Permissions

| Command | Needs |
| --- | --- |
| `describe` | `get` on `runtimerollouts`; `get` on ConfigMaps in the operator namespace for the assessment report |
| `approve`, `revoke` | `get` and `patch` on `runtimerollouts`, and `update` on the virtual subresource `runtimerollouts/approve`; `create` on `selfsubjectreviews`, which Kubernetes grants every authenticated user |
| `reproduce` | `get` on `clusterruntimes` (without a PLAN argument) and on ConfigMaps in the operator namespace |

The chart's `aicr-operator-approver` ClusterRole and Role grant all of these
to the users and groups in its `approvers` value.
