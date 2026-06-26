#!/usr/bin/env python3
"""Deterministic synthesizer for a large, realistic corroboration evidence tree.

Mirrors the prototype mock's approach (hash-driven, NO random / NO clock), so
re-running produces byte-identical output. Writes the Contract-3 GCS layout:

  <out>/evidence/results/<group>/<dashboard>/<tab>/<signer-id-hash>/<run-id>/
      meta.json
      ctrf/{deployment,performance,conformance}.json

Usage:
  python3 gen.py [OUT_DIR]      # default OUT_DIR: <tempdir>/corroborate-bigdemo

Then render + serve (paths printed at the end):
  go run ./tools/corroborate -in <OUT_DIR>/evidence -out <OUT_DIR>/site
  cd <OUT_DIR>/site && python3 -m http.server 8000   # open http://localhost:8000/

Stdlib only — no third-party deps. Lives under testdata/ so Go tooling ignores it.
"""
import hashlib, json, os, sys, tempfile, datetime

OUT_ROOT = os.path.abspath(sys.argv[1]) if len(sys.argv) > 1 else os.path.join(tempfile.gettempdir(), "corroborate-bigdemo")
OUT = os.path.join(OUT_ROOT, "evidence", "results")
BASE = datetime.datetime(2026, 6, 20, 3, 14, 7, tzinfo=datetime.timezone.utc)  # fixed, not now()
N_BUILDS = 8
AICR_VERSIONS = ["v1.0.0", "v0.16.1", "v0.16.0", "v0.15.2", "v0.15.0", "v0.14.1", "v0.14.0", "v0.13.1"]
K8S_POOL = ["1.33", "1.32", "1.31", "1.30"]


def H(*parts):
    return int(hashlib.sha256("|".join(map(str, parts)).encode()).hexdigest(), 16)


# --- signer roster (class + allowlisted are pre-derived by GP2 at ingest) ----
SIGNERS = {
    "nvidia":    dict(label="NVIDIA UAT",   cls="first-party", allow=True,
                      # matches the firstParty regex in testdata/allowlist.yaml (uat-(aws|gcp).yaml)
                      identity="https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
                      issuer="https://token.actions.githubusercontent.com"),
    "acme":      dict(label="Acme GPU",     cls="community",   allow=True,
                      identity="https://github.com/acme-gpu/aicr-attest/.github/workflows/attest.yaml@refs/heads/main",
                      issuer="https://token.actions.githubusercontent.com"),
    "hydra":     dict(label="Hydra ML",     cls="community",   allow=True,
                      identity="https://gitlab.com/hydra-ml/evidence", issuer="https://gitlab.com"),
    "coreriver": dict(label="CoreRiver",    cls="community",   allow=True,
                      identity="https://github.com/coreriver/attest/.github/workflows/a.yaml@refs/heads/main",
                      issuer="https://token.actions.githubusercontent.com"),
    "coreweave": dict(label="CoreWeave Lab", cls="partner",    allow=True,
                      identity="https://oidc.coreweave-lab.example/attest", issuer="https://oidc.coreweave-lab.example"),
    "bluefield": dict(label="BlueField",    cls="partner",     allow=True,
                      identity="https://oidc.bluefield.example/attest", issuer="https://oidc.bluefield.example"),
    "driveby":   dict(label="drive-by",     cls="community",   allow=False,  # verified-but-unknown => reported dot
                      identity="https://github.com/driveby/random/.github/workflows/x.yaml@refs/heads/main",
                      issuer="https://token.actions.githubusercontent.com"),
}
# participation rate per signer (deterministic per recipe)
PARTICIPATE = {"nvidia": 100, "acme": 72, "hydra": 46, "coreriver": 36,
               "coreweave": 30, "bluefield": 20, "driveby": 26}

# --- recipe coordinates (criteria -> the generator inverts the coordinate) ---
RECIPES = [
    ("h100", "eks", "ubuntu", "training", "kubeflow"),
    ("h100", "eks", "ubuntu", "training", "slurm"),
    ("h100", "eks", "ubuntu", "inference", "dynamo"),
    ("h100", "eks", "ubuntu", "inference", "nim"),
    ("h100", "gke", "cos", "training", "kubeflow"),
    ("h100", "gke", "cos", "training", "slurm"),
    ("h100", "gke", "cos", "inference", "dynamo"),
    ("h100", "aks", "ubuntu", "training", ""),
    ("h100", "aks", "ubuntu", "training", "kubeflow"),
    ("h100", "aks", "ubuntu", "inference", "dynamo"),
    ("gb200", "eks", "ubuntu", "training", ""),
    ("gb200", "eks", "ubuntu", "training", "kubeflow"),
    ("gb200", "eks", "ubuntu", "inference", "dynamo"),
    ("gb200", "oke", "ubuntu", "training", "kubeflow"),
    ("gb200", "oke", "ubuntu", "inference", "dynamo"),
    ("b200", "gke", "cos", "training", "kubeflow"),
    ("b200", "gke", "cos", "inference", "dynamo"),
    ("rtx-pro-6000", "eks", "ubuntu", "inference", "dynamo"),
    ("rtx-pro-6000", "eks", "ubuntu", "inference", "nim"),
    ("rtx-pro-6000", "lke", "ubuntu", "training", ""),
    ("rtx-pro-6000", "lke", "ubuntu", "inference", ""),
]

# --- check pools per phase (membership varies per recipe for real coverage) --
DEPLOY = ["operator-health", "driver-ready", "container-toolkit-ready", "device-plugin-ready",
          "gpu-feature-discovery", "dcgm-exporter-ready", "node-feature-discovery", "mig-manager-ready"]
PERF = ["nccl-all-reduce-bw-net", "nccl-all-reduce-bw-nvls", "gpu-burn", "hpl-linpack", "memory-bandwidth"]
CONF = ["gpu-direct-rdma", "mig-strategy-single", "k8s-version-constraint",
        "topology-aware-scheduling", "secure-boot-attestation"]

MIG_ACCEL = {"h100", "gb200", "b200"}
NVLINK_ACCEL = {"gb200", "b200"}


def recipe_name(accel, svc, osn, intent, plat):
    parts = [accel, svc, osn, intent] + ([plat] if plat else [])
    return "-".join(parts)


def coordinate(accel, svc, osn, intent, plat):
    tab = intent + ("-" + plat if plat else "")
    return svc, f"{accel}-{osn}", tab


def checks_for(accel, svc, osn, intent, plat):
    dep = [c for c in DEPLOY if c != "mig-manager-ready" or accel in MIG_ACCEL]
    perf = [c for c in PERF
            if (c != "nccl-all-reduce-bw-nvls" or accel in NVLINK_ACCEL)
            and (c != "hpl-linpack" or intent == "training")]
    conf = [c for c in CONF
            if (c != "gpu-direct-rdma" or svc in ("eks", "gke"))
            and (c != "mig-strategy-single" or accel in MIG_ACCEL)
            and (c != "secure-boot-attestation" or osn == "ubuntu")]
    return {"deployment": dep, "performance": perf, "conformance": conf}


def participants(rname):
    out = [s for s, rate in PARTICIPATE.items() if H("part", rname, s) % 100 < rate]
    if "nvidia" not in out:
        out.append("nvidia")  # first-party always runs
    return out


def latest_status(rname, check, parts):
    """Engineer the LATEST-build outcome per check to span all five states."""
    allow = [s for s in parts if SIGNERS[s]["allow"]]
    comm = [s for s in allow if s != "nvidia"]
    scen = H("scen", rname, check) % 100
    st = {s: "skipped" for s in parts}  # default: not run
    if "driveby" in parts and H("rep", rname, check) % 100 < 60:
        st["driveby"] = "passed" if H("repres", rname, check) % 100 < 70 else "failed"
    if scen < 55:                                   # CONFIRMED (or SINGLE if <2 allowlisted)
        for s in allow:
            st[s] = "passed"
    elif scen < 70:                                 # SINGLE: only nvidia runs it
        if "nvidia" in parts:
            st["nvidia"] = "passed"
    elif scen < 83:                                 # CONTESTED: nvidia pass, one community fails
        if "nvidia" in parts:
            st["nvidia"] = "passed"
        if comm:
            st[comm[0]] = "failed"
            for s in comm[1:]:
                st[s] = "passed"
    elif scen < 91:                                 # FAILING: all allowlisted fail
        for s in allow:
            st[s] = "failed"
    else:                                           # UNTESTED: everyone skips (coverage gap)
        pass
    return st


def build_status(rname, check, parts, build, latest):
    """Older builds vary around the latest to create history + flakiness."""
    if build == 0:
        return latest
    st = {}
    for s in parts:
        base = latest[s]
        flip = H("flip", rname, check, s, build) % 100
        if base in ("passed", "failed") and flip < 14:        # flaky: occasional opposite
            st[s] = "failed" if base == "passed" else "passed"
        elif base == "skipped" and flip < 8:                   # sometimes ran in the past
            st[s] = "passed"
        else:
            st[s] = base
    return st


def iso(dt):
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


def write(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(obj, f, indent=2)
        f.write("\n")


def main():
    runs = 0
    for accel, svc, osn, intent, plat in RECIPES:
        rname = recipe_name(accel, svc, osn, intent, plat)
        group, dash, tab = coordinate(accel, svc, osn, intent, plat)
        checks = checks_for(accel, svc, osn, intent, plat)
        parts = participants(rname)
        latest = {ph: {c: latest_status(rname, c, parts) for c in cs} for ph, cs in checks.items()}

        for signer in parts:
            smeta = SIGNERS[signer]
            for b in range(N_BUILDS):
                attested = BASE - datetime.timedelta(days=b * 3, hours=H("hr", rname, signer, b) % 12)
                aicr = AICR_VERSIONS[b % len(AICR_VERSIONS)]
                k8s = K8S_POOL[H("k8s", rname, signer, b) % len(K8S_POOL)]
                digest = "%064x" % (H("dig", rname, signer, b) % (16 ** 64))
                runid = f"run-{rname}-{signer}-b{b}"
                base = f"{OUT}/{group}/{dash}/{tab}/{signer}/{runid}"
                meta = {
                    "schemaVersion": "aicr-corroboration-meta/v1",
                    "coordinate": {"group": group, "dashboard": dash, "tab": tab},
                    "recipe": rname,
                    "signer": {"idHash": signer, "identity": smeta["identity"], "issuer": smeta["issuer"],
                               "class": smeta["cls"], "allowlisted": smeta["allow"]},
                    "runId": runid, "aicrVersion": aicr, "k8sVersion": k8s,
                    "k8sConstraint": ">= 1.30.0",
                    "bundleDigest": "sha256:" + digest,
                    "evidenceRef": f"ghcr.io/nvidia/aicr-evidence/{rname}@sha256:{digest}",
                    "rekorLogIndex": H("rekor", rname, signer, b) % 90000000,
                    "attestedAt": iso(attested),
                }
                write(f"{base}/meta.json", meta)
                for ph, cs in checks.items():
                    tests = []
                    for c in cs:
                        stt = build_status(rname, c, parts, b, latest[ph][c])[signer]
                        t = {"name": c, "status": stt, "duration": 1000 + H("dur", c, b) % 90000, "suite": [ph]}
                        if stt == "skipped":
                            t["message"] = "not applicable to this coordinate"
                        tests.append(t)
                    summ = {"tests": len(tests),
                            "passed": sum(1 for t in tests if t["status"] == "passed"),
                            "failed": sum(1 for t in tests if t["status"] == "failed"),
                            "skipped": sum(1 for t in tests if t["status"] == "skipped"),
                            "pending": 0, "other": 0,
                            "start": int(attested.timestamp() * 1000),
                            "stop": int(attested.timestamp() * 1000) + 300000}
                    report = {"reportFormat": "CTRF", "specVersion": "0.0.1", "timestamp": iso(attested),
                              "generatedBy": "aicr validate",
                              "results": {"tool": {"name": "aicr", "version": aicr},
                                          "summary": summ, "tests": tests}}
                    write(f"{base}/ctrf/{ph}.json", report)
                runs += 1

    print(f"wrote {len(RECIPES)} recipes, {runs} runs under {OUT}")
    print("\nnext:")
    print(f"  go run ./tools/corroborate -in {OUT_ROOT}/evidence -out {OUT_ROOT}/site")
    print(f"  cd {OUT_ROOT}/site && python3 -m http.server 8000   # open http://localhost:8000/")


if __name__ == "__main__":
    main()
