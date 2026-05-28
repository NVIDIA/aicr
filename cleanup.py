#!/usr/bin/env python3
"""
wipe-cluster.py — remove Run:ai and NVIDIA AI stack from the current kube context.

Phases (each phase fans out in parallel; phases run sequentially because
finalizers depend on operators being alive when their CRs are deleted):

  1. Pre-flight: capture context, get user confirmation.
  2. Delete CRs of known finalizer-heavy CRDs while their operators still run.
  3. Uninstall matching Helm releases (parallel).
  4. Delete non-helm manifest installs (training-operator, mpi-operator).
  5. Delete matching namespaces (parallel, --wait=false).
  6. Strip finalizers from stuck namespaces and orphaned CRs.
  7. Delete matching CRDs (parallel).

Run with --dry-run first.
"""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass

NAMESPACE_PATTERNS = [
    "gpu-operator",
    "fake-gpu-operator",
    "knative-operator",
    "knative-serving",
    "knative-eventing",
    "kubeflow",
    "lws-system",
    "monitoring",
    "mpi-operator",
    "nim-operator",
    "nemo-operator",
    "network-operator",
    "nvidia-network-operator",
    "node-feature-discovery",
    "cert-manager",
    "runai",
    "runai-backend",
    "kai-scheduler",
    "kueue-system",
]

HELM_RELEASE_PATTERNS = [
    "fake-gpu-operator",
    "gpu-operator",
    "knative-operator",
    "kube-prometheus-stack",
    "prometheus-operator-crds",
    "lws",
    "nim-operator",
    "k8s-nim-operator",
    "nemo-operator",
    "network-operator",
    "nvidia-network-operator",
    "nfd",
    "node-feature-discovery",
    "nvsentinel",
    "cert-manager",
    "kai-scheduler",
    "runai-backend",
    "runai-cluster",
    "kueue",
]

CRD_PATTERNS = [
    ".nvidia.com",
    ".knative.dev",
    ".kubeflow.org",
    "leaderworkerset.x-k8s.io",
    ".run.ai",
    "engine.run.ai",
    "kai.scheduler",
    "scheduling.run.ai",
    ".kueue.x-k8s.io",
    "node-feature-discovery",
    "nodefeature",
    "nodefeaturerule",
    "cert-manager.io",
    "acme.cert-manager.io",
    "monitoring.coreos.com",
]

FINALIZER_HEAVY_CRD_KINDS = [
    "clusterpolicies.nvidia.com",
    "nvidiadrivers.nvidia.com",
    "knativeservings.operator.knative.dev",
    "knativeeventings.operator.knative.dev",
    "runaiconfigs.run.ai",
    "trainingruntimes.kubeflow.org",
    "issuers.cert-manager.io",
    "clusterissuers.cert-manager.io",
    "certificates.cert-manager.io",
    "certificaterequests.cert-manager.io",
]

NON_HELM_MANIFEST_DEPLOYMENTS = [
    ("kubeflow", "deployment/training-operator"),
    ("mpi-operator", "deployment/mpi-operator"),
]

KUBECTL = "kubectl"
HELM = "helm"


@dataclass
class StepResult:
    name: str
    ok: bool
    output: str


def run(cmd: list[str], check: bool = False, timeout: int = 120) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        check=check,
        timeout=timeout,
    )


def fan_out(label: str, tasks: list[tuple[str, list[str]]], dry_run: bool, max_workers: int = 16) -> list[StepResult]:
    if not tasks:
        print(f"  ({label}: nothing to do)")
        return []
    print(f"  {label}: {len(tasks)} task(s) in parallel")
    if dry_run:
        for name, cmd in tasks:
            print(f"    [dry-run] {name}: {' '.join(cmd)}")
        return []
    results: list[StepResult] = []
    with ThreadPoolExecutor(max_workers=max_workers) as ex:
        futures = {ex.submit(run, cmd): name for name, cmd in tasks}
        for fut in as_completed(futures):
            name = futures[fut]
            try:
                cp = fut.result()
                ok = cp.returncode == 0
                out = (cp.stdout + cp.stderr).strip()
                results.append(StepResult(name, ok, out))
                tag = "ok " if ok else "ERR"
                first_line = out.splitlines()[0] if out else ""
                print(f"    [{tag}] {name}  {first_line[:140]}")
            except Exception as e:
                results.append(StepResult(name, False, str(e)))
                print(f"    [ERR] {name}  {e}")
    return results


def current_context() -> str:
    cp = run([KUBECTL, "config", "current-context"])
    return cp.stdout.strip() if cp.returncode == 0 else "(unknown)"


def list_namespaces() -> list[str]:
    cp = run([KUBECTL, "get", "ns", "-o", "name"])
    if cp.returncode != 0:
        return []
    return [line.split("/", 1)[1] for line in cp.stdout.strip().splitlines() if "/" in line]


def list_helm_releases() -> list[tuple[str, str]]:
    cp = run([HELM, "list", "-A", "-o", "json"])
    if cp.returncode != 0:
        return []
    try:
        return [(r["name"], r["namespace"]) for r in json.loads(cp.stdout)]
    except json.JSONDecodeError:
        return []


def list_crds() -> list[str]:
    cp = run([KUBECTL, "get", "crd", "-o", "name"])
    if cp.returncode != 0:
        return []
    return [line.split("/", 1)[1] for line in cp.stdout.strip().splitlines() if "/" in line]


def matches_any(name: str, patterns: list[str]) -> bool:
    return any(p in name for p in patterns)


def phase_delete_crs(dry_run: bool) -> None:
    print("\n[2/7] Deleting CRs of finalizer-heavy CRDs (before their operators die)")
    crds = list_crds()
    tasks: list[tuple[str, list[str]]] = []
    for kind in FINALIZER_HEAVY_CRD_KINDS:
        if kind not in crds:
            continue
        tasks.append((
            f"delete-all {kind}",
            [KUBECTL, "delete", kind, "--all", "--all-namespaces",
             "--ignore-not-found", "--wait=false", "--timeout=30s"],
        ))
    fan_out("delete CRs", tasks, dry_run)


def phase_uninstall_helm(dry_run: bool) -> None:
    print("\n[3/7] Uninstalling Helm releases")
    releases = list_helm_releases()
    tasks = [
        (
            f"{name} (ns={ns})",
            [HELM, "uninstall", name, "-n", ns, "--ignore-not-found", "--wait", "--timeout=120s"],
        )
        for name, ns in releases
        if matches_any(name, HELM_RELEASE_PATTERNS)
    ]
    fan_out("helm uninstalls", tasks, dry_run)


def phase_delete_manifest_installs(dry_run: bool) -> None:
    print("\n[4/7] Deleting non-helm manifest installs")
    tasks = [
        (
            f"{ns}/{res}",
            [KUBECTL, "delete", "-n", ns, res, "--ignore-not-found", "--wait=false", "--timeout=30s"],
        )
        for ns, res in NON_HELM_MANIFEST_DEPLOYMENTS
    ]
    fan_out("manifest deletes", tasks, dry_run)


def phase_delete_namespaces(dry_run: bool) -> list[str]:
    print("\n[5/7] Deleting namespaces (background; will strip finalizers next)")
    existing = list_namespaces()
    targets = [ns for ns in existing if matches_any(ns, NAMESPACE_PATTERNS)]
    tasks = [
        (
            ns,
            [KUBECTL, "delete", "ns", ns, "--ignore-not-found", "--wait=false", "--timeout=30s"],
        )
        for ns in targets
    ]
    fan_out("namespace deletes", tasks, dry_run)
    return targets


def strip_finalizers_ns(ns: str) -> StepResult:
    patch = '{"metadata":{"finalizers":[]},"spec":{"finalizers":[]}}'
    cp = run([KUBECTL, "get", "ns", ns, "-o", "name"])
    if cp.returncode != 0:
        return StepResult(ns, True, "already gone")
    cp = run([
        KUBECTL, "patch", "ns", ns,
        "--type=merge", "-p", patch,
        "--subresource=finalize",
    ])
    if cp.returncode == 0:
        return StepResult(ns, True, "finalizers cleared")
    cp = run([KUBECTL, "patch", "ns", ns, "--type=merge", "-p", patch])
    return StepResult(ns, cp.returncode == 0, cp.stderr.strip())


def phase_strip_finalizers(dry_run: bool, target_namespaces: list[str]) -> None:
    print("\n[6/7] Waiting briefly, then stripping finalizers from anything stuck")
    if dry_run:
        print("  [dry-run] (would wait 20s then strip finalizers from stuck objects)")
        return
    time.sleep(20)

    existing_crds = set(list_crds())
    for kind in FINALIZER_HEAVY_CRD_KINDS:
        if kind not in existing_crds:
            continue
        cp = run([KUBECTL, "get", kind, "--all-namespaces", "-o",
                  "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}"])
        if cp.returncode != 0 or not cp.stdout.strip():
            continue
        tasks: list[tuple[str, list[str]]] = []
        for line in cp.stdout.strip().splitlines():
            ns, _, name = line.partition("/")
            if not name:
                ns, name = "", ns
            tasks.append((
                f"finalizer-strip {kind} {ns}/{name}",
                [KUBECTL, "patch", kind, name,
                 *(["-n", ns] if ns else []),
                 "--type=merge", "-p", '{"metadata":{"finalizers":null}}'],
            ))
        fan_out(f"strip {kind} finalizers", tasks, False)

    stuck = [ns for ns in target_namespaces if ns in list_namespaces()]
    if not stuck:
        print("  (no stuck namespaces)")
        return
    print(f"  stuck namespaces: {stuck}")
    with ThreadPoolExecutor(max_workers=8) as ex:
        for r in ex.map(strip_finalizers_ns, stuck):
            tag = "ok " if r.ok else "ERR"
            print(f"    [{tag}] {r.name}  {r.output}")


def phase_delete_crds(dry_run: bool) -> None:
    print("\n[7/7] Deleting matching CRDs")
    crds = list_crds()
    targets = [c for c in crds if matches_any(c, CRD_PATTERNS)]
    tasks = [
        (
            c,
            [KUBECTL, "delete", "crd", c, "--ignore-not-found", "--wait=false", "--timeout=30s"],
        )
        for c in targets
    ]
    fan_out("CRD deletes", tasks, dry_run)


def confirm(ctx: str, force: bool) -> None:
    print(f"\nCurrent kube context: {ctx}")
    print("This will delete Helm releases, namespaces, and CRDs matching:")
    print(f"  helm releases: {HELM_RELEASE_PATTERNS}")
    print(f"  namespaces:    {NAMESPACE_PATTERNS}")
    print(f"  CRD substrs:   {CRD_PATTERNS}")
    if force:
        return
    resp = input("\nProceed? Type 'y': ").strip()
    if resp != 'y':
        print("Aborting.")
        sys.exit(2)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--dry-run", action="store_true", help="show actions without executing")
    ap.add_argument("--yes", action="store_true", help="skip confirmation prompt")
    args = ap.parse_args()

    for tool in (KUBECTL, HELM):
        if shutil.which(tool) is None:
            print(f"missing required tool: {tool}", file=sys.stderr)
            return 1

    ctx = current_context()
    print(f"[1/7] kube context: {ctx}")
    confirm(ctx, args.yes or args.dry_run)

    phase_delete_crs(args.dry_run)
    phase_uninstall_helm(args.dry_run)
    phase_delete_manifest_installs(args.dry_run)
    target_ns = phase_delete_namespaces(args.dry_run)
    phase_strip_finalizers(args.dry_run, target_ns)
    phase_delete_crds(args.dry_run)

    print("\nDone. Final state:")
    run_cp = run([KUBECTL, "get", "ns"])
    print(run_cp.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())