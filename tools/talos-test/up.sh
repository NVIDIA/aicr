#!/usr/bin/env bash
# Spin up a local Talos cluster (Docker provisioner) for snapshot testing.
# See tools/talos-test/README.md for prerequisites and customization.
#
# Env vars (all optional):
#   TALOS_CLUSTER_NAME           default: aicr-talos
#   TALOS_VERSION                default: v1.9.0
#   TALOS_REGISTRY_MIRROR_HOST   default: host.docker.internal (Darwin) or 172.17.0.1 (Linux)
#   KUBECONFIG_OUT               default: ${HOME}/.kube/aicr-talos

set -euo pipefail

CLUSTER_NAME="${TALOS_CLUSTER_NAME:-aicr-talos}"
TALOS_VERSION="${TALOS_VERSION:-v1.9.0}"
KUBECONFIG_OUT="${KUBECONFIG_OUT:-${HOME}/.kube/aicr-talos}"

case "$(uname -s)" in
    Darwin) MIRROR_HOST_DEFAULT="host.docker.internal" ;;
    Linux)  MIRROR_HOST_DEFAULT="172.17.0.1" ;;
    *)      MIRROR_HOST_DEFAULT="host.docker.internal" ;;
esac
MIRROR_HOST="${TALOS_REGISTRY_MIRROR_HOST:-${MIRROR_HOST_DEFAULT}}"

err() { printf 'error: %s\n' "$*" >&2; exit 1; }

# 1. Preflight: required tools on PATH. curl is included because the
# registry-reachability check below uses it; without this guard, a host
# without curl would fall through to the misleading
# "registry not reachable" branch.
for tool in talosctl docker kubectl chainsaw curl; do
    command -v "$tool" >/dev/null 2>&1 \
        || err "$tool not found on PATH; see tools/talos-test/README.md"
done

# 2. Registry check: localhost:5001 reachable from the host.
if ! curl -sf "http://localhost:5001/v2/" >/dev/null 2>&1; then
    err "localhost:5001 registry not reachable; start it via 'make dev-env' or run a registry container manually (see tools/talos-test/README.md)"
fi

# 2a. Kernel module preflight: Talos's default Flannel CNI requires
# br_netfilter for VXLAN bridging. The Linux VM that backs the
# Docker-compatible runtime on macOS (Docker Desktop's LinuxKit VM or
# Podman Machine's Fedora CoreOS VM) ships the module but does not
# auto-load it; load it once here via a short privileged sidecar.
# /lib/modules from the VM is bind-mounted so modprobe can find the
# module map. This is a no-op on Linux hosts where the module is
# normally already loaded. State persists for the life of the
# host VM (i.e. until the runtime restarts).
echo "Ensuring br_netfilter is loaded in the host VM kernel..."
if ! docker run --rm --privileged --net=host \
        -v /lib/modules:/lib/modules:ro \
        alpine:3.21 sh -c 'modprobe br_netfilter && lsmod | grep -q br_netfilter' \
        >/dev/null 2>&1; then
    err "could not load br_netfilter; flannel CNI will fail. Try running 'docker run --rm --privileged --net=host -v /lib/modules:/lib/modules:ro alpine:3.21 modprobe br_netfilter' manually, then 'lsmod | grep br_netfilter' to verify."
fi

# 3. Cluster create. Newer talosctl (>= 1.x) splits provisioners into
# subcommands; the docker subcommand fixes controlplanes=1 and accepts
# registry config only via --config-patch (no --registry-mirror flag).
PATCH_FILE="$(mktemp -t talos-config-patch.XXXXXX.yaml)"
trap 'rm -f "${PATCH_FILE}"' EXIT
# Route localhost:5001/* pulls from inside the Talos node back to the
# host's localhost:5001 registry.
cat > "${PATCH_FILE}" <<EOF
machine:
  registries:
    mirrors:
      "localhost:5001":
        endpoints:
          - "http://${MIRROR_HOST}:5001"
EOF

echo "Creating Talos cluster '${CLUSTER_NAME}' (Talos ${TALOS_VERSION}, registry mirror -> ${MIRROR_HOST}:5001)..."
talosctl cluster create docker \
    --name "${CLUSTER_NAME}" \
    --workers 1 \
    --image "ghcr.io/siderolabs/talos:${TALOS_VERSION}" \
    --config-patch "@${PATCH_FILE}"

# Derive control-plane addressing from Docker:
#  - CP_NODE_IP    : the in-network IP, used as --nodes (Talos identity)
#  - APID_ENDPOINT : the host-side NAT'd apid port, used as --endpoints
# On macOS the in-network 10.5.0.0/24 subnet is not host-routable, so
# host:port mappings are required to reach apid (50000) and the K8s
# API (6443) from the developer's machine.
CP_CONTAINER="${CLUSTER_NAME}-controlplane-1"
CP_NODE_IP="$(docker inspect "${CP_CONTAINER}" \
    --format "{{ (index .NetworkSettings.Networks \"${CLUSTER_NAME}\").IPAddress }}" 2>/dev/null)"
[ -n "${CP_NODE_IP}" ] || err "could not resolve control-plane node IP for ${CP_CONTAINER}"
APID_HOST_PORT="$(docker port "${CP_CONTAINER}" 50000/tcp 2>/dev/null | head -1 | awk -F: '{print $NF}')"
[ -n "${APID_HOST_PORT}" ] || err "could not resolve apid host port for ${CP_CONTAINER}"
APID_ENDPOINT="127.0.0.1:${APID_HOST_PORT}"

# Persist endpoint/node defaults so subsequent talosctl commands (and the
# user's interactive sessions) don't need --endpoints / --nodes.
talosctl config endpoint "${APID_ENDPOINT}"
talosctl config node "${CP_NODE_IP}"

# 4. Talos health: stream readiness phases (etcd, apid, kubelet, control plane,
# all machines, all k8s nodes ready). Replaces the old --wait flag and
# surfaces specific Talos-level failures instead of timing out blindly.
# CoreDNS pulls registry.k8s.io/coredns on first run, which can be slow on
# residential connections; the rest of the checks complete in ~30s.
echo "Waiting for Talos cluster to report healthy (apid ${APID_ENDPOINT}, node ${CP_NODE_IP})..."
if ! talosctl --endpoints "${APID_ENDPOINT}" --nodes "${CP_NODE_IP}" \
        health --wait-timeout 8m; then
    echo "" >&2
    echo "talosctl health failed. Diagnostic commands you can run now:" >&2
    echo "  talosctl --endpoints ${APID_ENDPOINT} --nodes ${CP_NODE_IP} services" >&2
    echo "  talosctl --endpoints ${APID_ENDPOINT} --nodes ${CP_NODE_IP} logs controller-runtime --tail 100" >&2
    echo "  talosctl --endpoints ${APID_ENDPOINT} --nodes ${CP_NODE_IP} logs etcd --tail 100" >&2
    echo "  docker logs ${CP_CONTAINER} --tail 100" >&2
    echo "  docker logs ${CLUSTER_NAME}-worker-1 --tail 100" >&2
    err "Talos cluster did not reach healthy state"
fi

# 5. Kubeconfig: write to a dedicated path so we don't clobber the user's main kubeconfig.
mkdir -p "$(dirname "${KUBECONFIG_OUT}")"
talosctl --endpoints "${APID_ENDPOINT}" --nodes "${CP_NODE_IP}" \
    kubeconfig --merge=false --force "${KUBECONFIG_OUT}"

# 6. Rewrite the kubeconfig server URL to the host-NAT'd K8s API port.
# talosctl writes the in-network URL (e.g. https://10.5.0.2:6443), which on
# macOS Docker Desktop is not host-routable; the apiserver is reachable
# only via the docker port mapping. The default Talos apiserver cert
# does not necessarily include 127.0.0.1 in its SAN list, so we also flip
# insecure-skip-tls-verify on the cluster entry. This is local-dev-only.
K8S_API_HOST_PORT="$(docker port "${CP_CONTAINER}" 6443/tcp 2>/dev/null | head -1 | awk -F: '{print $NF}')"
[ -n "${K8S_API_HOST_PORT}" ] || err "could not resolve K8s API host port for ${CP_CONTAINER}"
CLUSTER_KEY="$(kubectl --kubeconfig "${KUBECONFIG_OUT}" config get-clusters --no-headers | head -1)"
[ -n "${CLUSTER_KEY}" ] || err "could not read cluster name from ${KUBECONFIG_OUT}"
kubectl --kubeconfig "${KUBECONFIG_OUT}" config set-cluster "${CLUSTER_KEY}" \
    --server="https://127.0.0.1:${K8S_API_HOST_PORT}" \
    --insecure-skip-tls-verify=true >/dev/null

# 7. Sanity: every node Ready within 120s.
kubectl --kubeconfig "${KUBECONFIG_OUT}" wait \
    --for=condition=Ready node --all --timeout=120s

# 8. Relax Pod Security Standards on the 'default' namespace so the
# privileged snapshot agent can be scheduled there. Talos's default
# config enforces 'restricted' cluster-wide, but the snapshot agent
# requires privileged + hostPID + hostNetwork (the OS=talos pod-shape
# branch in pkg/k8s/agent/job.go lives inside applyPrivilegedSettings,
# so this matches the posture of real GPU clusters that exercise the
# same code path).
kubectl --kubeconfig "${KUBECONFIG_OUT}" label namespace default \
    pod-security.kubernetes.io/enforce=privileged \
    pod-security.kubernetes.io/audit=privileged \
    pod-security.kubernetes.io/warn=privileged \
    --overwrite >/dev/null

cat <<EOF

Talos cluster ready.

  export KUBECONFIG=${KUBECONFIG_OUT}
  kubectl get nodes

Tear down: make talos-dev-env-clean
EOF
