# tools/talos-test

Spin up a local Talos Linux cluster (Docker provisioner) for testing the
AICR snapshot agent's Talos OS support. The chainsaw test that exercises
this cluster lives at `tests/chainsaw/snapshot/deploy-agent-talos/`.

## Prerequisites

- Docker (or compatible) running locally
- `kubectl`
- `chainsaw`
- `curl` (used by the registry-reachability preflight in `up.sh`)
- A `localhost:5001` image registry (the same one `make dev-env` brings up)
- `talosctl`

### Installing `talosctl`

macOS (Homebrew):

    brew install siderolabs/tap/talosctl

Linux (upstream installer):

    curl -sL https://talos.dev/install | sh

Verify:

    talosctl version --client --short

## Quickstart

1. Build and push the agent image to the local registry:

       make image IMAGE_REGISTRY=localhost:5001 IMAGE_TAG=local

   This produces an image at `localhost:5001/aicr:local` that the
   snapshot agent Job will pull.

2. Build the host `aicr` binary (the chainsaw test invokes it):

       make build

3. Bring up the Talos cluster:

       make talos-dev-env

4. Run the chainsaw test:

       make talos-snapshot-test

5. Tear down when done:

       make talos-dev-env-clean

## Customization

Override defaults via environment variables before invoking the make
targets:

| Variable | Default | Purpose |
|----------|---------|---------|
| `TALOS_CLUSTER_NAME` | `aicr-talos` | `talosctl cluster create --name` |
| `TALOS_VERSION` | `v1.9.0` | Talos version (`ghcr.io/siderolabs/talos:$TALOS_VERSION`) |
| `TALOS_KUBECONFIG` | `${HOME}/.kube/aicr-talos` | Where the cluster's kubeconfig is written |
| `TALOS_REGISTRY_MIRROR_HOST` | auto (Darwin: `host.docker.internal`; Linux: `172.17.0.1`) | Hostname the Talos node containers use to reach the host's `localhost:5001` registry |

## Side effects

`make talos-dev-env` makes three changes outside the Talos containers
you should be aware of before scheduling other workloads on this
cluster — and before relying on `talosctl` for any other cluster:

- **`default` namespace is relabeled to `pod-security.kubernetes.io/enforce=privileged`**
  (also `audit=privileged` and `warn=privileged`). Talos enforces
  `restricted` cluster-wide by default; the snapshot agent's privileged
  pod cannot be scheduled there without this relabel. The label persists
  for the lifetime of the cluster — anything else you `kubectl apply` to
  `default` afterward also runs at the privileged baseline. For
  unrelated workloads, prefer creating a separate namespace whose PSS
  enforcement label you control.
- **`br_netfilter` is loaded on the host VM** via a one-shot privileged
  Alpine container so flannel CNI works. The module persists for the
  life of the host runtime VM (Docker Desktop / Podman Machine), not
  just this cluster. No-op on Linux hosts where the module is normally
  already loaded.
- **Your default `talosctl` config (`~/.talos/config`) is updated**
  with this cluster's apid endpoint and node IP via
  `talosctl config endpoint` / `talosctl config node`. Subsequent
  interactive `talosctl` commands without `--endpoints` / `--nodes`
  will target this cluster — and after `make talos-dev-env-clean`
  they will target a destroyed cluster (host port no longer
  listens). To avoid affecting your default config, point
  `talosctl` at a temporary file before running the script:
  `TALOSCONFIG=$(mktemp -t talosctl.XXXXXX) make talos-dev-env`.
  After teardown, `talosctl config contexts` will show the stale
  context; remove it with `talosctl config remove-context aicr-talos`
  or by deleting `~/.talos/config` entirely if you don't use other
  Talos clusters.

`make talos-dev-env-clean` destroys the Talos containers but does not
revert any of the three changes above. Restart the host runtime VM to
drop `br_netfilter`; recreate the namespace to drop the PSS labels;
remove the talosctl context as documented above.

## Troubleshooting

**`talosctl: command not found`**
Install `talosctl` (see Installing `talosctl` above).

**`localhost:5001 registry not reachable`**
The cluster spinup expects the same registry the kind-based dev cluster uses.
Start it with `make dev-env` (or run a `registry:2` container on
port 5001 manually).

**Image pull fails inside the Talos node with a TLS error**
The registry mirror uses plain HTTP. If your Docker engine requires
HTTPS for the mirror host, add `localhost:5001` (or the bridge IP) to
`insecure-registries` in your Docker daemon configuration. The Talos
machine config already points the mirror at `http://...`, so the
failure is host-side, not Talos-side.

**`localhost:5001/aicr:local` not found inside the cluster**
You haven't pushed the image yet. Run Quickstart step 1.

## Why a separate cluster?

Talos has no systemd D-Bus and no `/etc/os-release` on the host
filesystem, so the snapshot agent's privileged pod cannot use the
hostPath mounts the kind-based dev cluster uses. PR #714 added a Talos collector
backend (`pkg/collector/talos/`) and gated those hostPath mounts on
`OS=talos` (`pkg/k8s/agent/job.go`). This tooling lets a developer
exercise that path locally against a real Talos node, not a kind node.
