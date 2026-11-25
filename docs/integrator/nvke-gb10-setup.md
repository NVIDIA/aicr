# NVKE GB10 Setup

The `gb10-nvke-ubuntu-*` recipes target NVKE with DGX Spark nodes: one GB10
GPU per node, on DGX OS (Ubuntu 24.04, arm64) with the NVIDIA driver
pre-installed. Four Preview coordinates:

- `gb10-nvke-ubuntu-training`: two Sparks linked over ConnectX-7. The
  performance phase runs NCCL over that link.
- `gb10-nvke-ubuntu-training-kubeflow`: adds Kubeflow Trainer to the training
  leaf.
- `gb10-nvke-ubuntu-inference`: one Spark.
- `gb10-nvke-ubuntu-inference-dynamo`: adds the Dynamo platform to the
  inference leaf.

## Cluster prerequisites

- Kubernetes 1.34 or newer.
- Sparks that the VXNIP Cluster API provider brings into NVKE carry
  `providerID: vxnip://<machine-uid>`. `aicr snapshot` maps every `vxnip://`
  node to `service: nvke`, as it does NICo's `nico://` on Forge, so a VXNIP
  cluster that does not run NVKE must pass `--service` to `aicr recipe`.
- DGX OS with its NVIDIA driver. GPU Operator's driver is off; the recipes use
  the host driver.
- A CRI-enabled containerd. DGX OS ships a containerd config that disables
  CRI, so replace it with a CRI-enabled one before the Spark joins the cluster.
- Internet access from pods on the Sparks: the NCCL check's workers install
  `openssh-server` when they start. DGX OS ships Docker, which sets the
  iptables `FORWARD` policy to `DROP`. A CNI that routes pod traffic through
  the host, as OVN-Kubernetes does in local gateway mode, then reaches nothing
  off the node. Accept the CNI's host interface in Docker's `DOCKER-USER`
  chain, for example `iptables -I DOCKER-USER -i ovn-k8s-mp0 -j ACCEPT` and the
  same with `-o`, and persist those rules.

## Linking two Sparks

The training leaves deploy `network-operator` with only the RDMA shared device
plugin, and their NCCL check runs RoCE v2 over a direct link between the two
Sparks. NCCL uses the host's RoCE devices and addresses, so pods need no second
network. Prepare both Sparks before deploying:

1. **Cable.** Connect port 1 of each Spark's ConnectX-7 (the `np1` netdevs)
   with one QSFP112 DAC, and leave port 0 uncabled. The DGX Spark user guide
   lists Amphenol NJAAKK-N911 (0.4 m) and NJAAKK0006 (0.5 m), and Luxshare
   LMTQF022-SD-R (0.4 m). Seat each end with the pull tab facing up. A plug
   inserted upside down seats short, and the NIC reports no module: `ethtool`
   shows `Link detected: no (No cable)`.
2. **Keep the ConnectX-7 powered.** DGX OS powers the card off, and removes it
   from the PCI bus, when it finds no cable at boot. Boot with the cable
   attached. To keep the card on without a cable, remove
   `/etc/nvidia/cx7-hotplug-enabled` and reboot.
3. **Addresses.** Port 1 appears as two PCIe functions, each with its own
   netdev and RDMA device: `enp1s0f1np1` and `enP2p1s0f1np1`. Give each
   function an IPv4 address in its own /24, the same /24 for that function on
   both Sparks. RoCE v2 resolves its peer through these addresses.
4. **MTU 9000** on both functions.
5. **ARP.** Both functions share one wire, so a Spark can answer ARP for its
   other function's address, and RoCE v2 then sends to the wrong one. Set
   `arp_ignore=1` and `arp_announce=2` on both functions.
6. **Shared RDMA namespace mode.** Pods reach the host's RoCE devices and GIDs
   only while `rdma system` shows `netns shared`, the Linux default.

With NetworkManager, on the first Spark (use `.12` on the second):

```shell
for spec in enp1s0f1np1:10.20.1 enP2p1s0f1np1:10.20.2; do
  dev=${spec%%:*} net=${spec#*:}
  sudo nmcli connection delete "cx7-$dev" >/dev/null 2>&1 || true
  sudo nmcli connection add type ethernet ifname "$dev" con-name "cx7-$dev" \
    ipv4.method manual ipv4.addresses "$net.11/24" ipv4.never-default yes \
    ipv6.method link-local 802-3-ethernet.mtu 9000 connection.autoconnect-priority 50
  sudo nmcli connection up "cx7-$dev"
done
printf 'net.ipv4.conf.%s.arp_ignore = 1\nnet.ipv4.conf.%s.arp_announce = 2\n' \
  enp1s0f1np1 enp1s0f1np1 enP2p1s0f1np1 enP2p1s0f1np1 | sudo tee /etc/sysctl.d/60-cx7-arp.conf
sudo sysctl -p /etc/sysctl.d/60-cx7-arp.conf
```

`ibdev2netdev` should then show `rocep1s0f1` and `roceP2p1s0f1` as `Up`, and a
`ping -M do -s 8972` to the other Spark's address on each function should pass.

## Validating

On two Sparks, the performance phase runs `nccl-all-reduce-bw-net`: two
workers, one GB10 each, each requesting `rdma/hca_shared_devices_a`. Before it
creates the TrainJob, a preflight fails when that resource is not allocatable,
in the same positive count, on both Sparks. In each rank, a precheck pins NCCL
to the ACTIVE RoCE devices and their RoCE v2 IPv4 GID, and fails with the cause
when either is missing, or when only one RoCE device is ACTIVE, as when one of
the linked port's two PCIe functions is down. It uses every ACTIVE RoCE device,
so a second cabled port needs addresses in subnets the peer shares, or must stay
down.

The `>= 20` GB/s floor comes from a run on two Sparks with NGC PyTorch 25.11:
24.11 GB/s busbw at 16 GiB, with NCCL using both PCIe functions.

On one Spark, the training leaves cannot run the performance phase: the NCCL
check needs two GPU nodes. Validate with `--phase deployment --phase
conformance`, with the ConnectX-7 kept powered as in step 2 under
[Linking two Sparks](#linking-two-sparks). The RDMA shared device plugin
advertises `rdma/hca_shared_devices_a` without a cable, so `expected-resources`
finds the RDMA fabric. To confirm, check that `kubectl describe node` shows
`rdma/hca_shared_devices_a` under `Allocatable` with a non-zero count. If DGX OS
has removed the ConnectX-7, no node carries the ConnectX-7 label, so the
NicClusterPolicy never reports ready and `expected-resources` fails on the
missing RDMA fabric. In that case, build the bundle without `--readiness-hooks`,
and validate with `--skip-check expected-resources`.

## A control plane that sees a GPU

A control plane on a machine with its own NVIDIA GPU, or in a VM with a GPU
passed through, can see that GPU on its PCI bus. NFD then reports it, and GPU
Operator labels the control-plane node `nvidia.com/gpu.present=true`. Keep the
GPU components off that node. With an amd64 control plane, select the Sparks by
architecture:

```shell
CP_NODE="my-control-plane-node"  # replace with the control plane's node name
kubectl label node "${CP_NODE}" nvidia.com/gpu.deploy.operands=false
aicr bundle -r recipe.yaml -o bundle \
  --accelerated-node-selector kubernetes.io/arch=arm64 \
  --set-json 'nvsentinel:global.nodeSelector={"kubernetes.io/arch":"arm64"}'
```

With an arm64 control plane, label the Sparks instead and pass that label to
both selectors. NVSentinel's node agents follow `global.nodeSelector`, not
`--accelerated-node-selector`, and tolerate every taint, so they need their own
selector. The validators count only nodes with allocatable `nvidia.com/gpu`, so
such a control plane does not enter the NCCL check.

## Known gaps

- DGX Spark has no GPUDirect RDMA or GDRCopy, so NCCL stages RoCE traffic
  through host memory.
- One Spark cannot run the training leaves' performance phase.
