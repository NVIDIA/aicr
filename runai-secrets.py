#!/usr/bin/env python3
"""
runai-secrets.py — generate self-signed certs + create the secrets the
Run:ai charts consume, in the right namespaces.

Outputs:
  ./runai-secrets-out/
    ca.crt, ca.key                                 # private CA
    runai-backend-tls.{crt,key}                    # ns runai-backend
    runai-cluster-domain-tls-secret.{crt,key}      # ns runai
    runai-cluster-domain-star-tls-secret.{crt,key} # ns runai (host-routing)
    runai-inference-tls.{crt,key}                  # ns knative-serving (later)
    secrets.yaml                                   # all manifests rendered

Secrets created (kubectl apply -f -):
  Namespace:         runai-backend, runai
  imagePullSecret:   runai-reg-creds                       (in BOTH ns)
  tls:               runai-backend-tls                     (runai-backend)
                     runai-cluster-domain-tls-secret       (runai)
                     runai-cluster-domain-star-tls-secret  (runai, *.fqdn)
  Opaque:            runai-ca-cert                         (in BOTH ns)
                       key: runai-ca.pem  (the CA cert we just made)

The inference TLS secret is rendered to disk only — no knative-serving
namespace yet on a CP-only bundle. Apply it later when knative is added.

Run --dry-run first to see what it would do.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import shutil
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

OUT_DIR = Path("./runai-secrets-out")
NS_BACKEND = "runai-backend"
NS_CLUSTER = "runai"
NS_KNATIVE = "knative-serving"

CA_DAYS = 3650
LEAF_DAYS = 825

DEFAULT_FQDN: str | None = "johny-kind.runailabs.com"
DEFAULT_WILDCARD_FQDN: str | None = "*.johny-kind.runailabs.com"
DEFAULT_INFERENCE_FQDN: str | None = "*.inference.johny-kind.runailabs.com"
DEFAULT_JFROG_TOKEN: str | None = os.getenv("AICR_JFROG_TOKEN")
DEFAULT_JFROG_USER: str | None = "self-hosted-image-puller-prod"
DEFAULT_REGISTRIES = ["runai.jfrog.io", "gcr.io"]
DEFAULT_OUT_DIR = str(OUT_DIR)
DEFAULT_DRY_RUN = False


@dataclass
class CertPair:
    crt: Path
    key: Path


def sh(cmd: list[str], check: bool = True, input_bytes: bytes | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, check=check, capture_output=True, input=input_bytes)


def require(tool: str) -> None:
    if shutil.which(tool) is None:
        sys.exit(f"missing required tool on PATH: {tool}")


# ---------- cert generation ----------

def gen_ca(out: Path) -> CertPair:
    key = out / "ca.key"
    crt = out / "ca.crt"
    sh(["openssl", "genrsa", "-out", str(key), "4096"])
    sh([
        "openssl", "req", "-x509", "-new", "-nodes",
        "-key", str(key),
        "-sha256", "-days", str(CA_DAYS),
        "-out", str(crt),
        "-subj", "/CN=Run:ai Local CA/O=runai-aicr",
    ])
    return CertPair(crt, key)


def gen_leaf(out: Path, ca: CertPair, name: str, cn: str, sans: list[str]) -> CertPair:
    key = out / f"{name}.key"
    csr = out / f"{name}.csr"
    crt = out / f"{name}.crt"
    cnf = out / f"{name}.cnf"

    san_lines = "\n".join(f"DNS.{i+1} = {s}" for i, s in enumerate(sans))
    cnf.write_text(
        f"""[req]
default_bits       = 2048
distinguished_name = req_distinguished_name
req_extensions     = v3_req
prompt             = no

[req_distinguished_name]
CN = {cn}
O  = runai-aicr

[v3_req]
keyUsage         = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName   = @alt_names

[alt_names]
{san_lines}
"""
    )

    sh(["openssl", "genrsa", "-out", str(key), "2048"])
    sh(["openssl", "req", "-new", "-key", str(key), "-out", str(csr), "-config", str(cnf)])
    sh([
        "openssl", "x509", "-req",
        "-in", str(csr),
        "-CA", str(ca.crt), "-CAkey", str(ca.key), "-CAcreateserial",
        "-out", str(crt),
        "-days", str(LEAF_DAYS), "-sha256",
        "-extensions", "v3_req", "-extfile", str(cnf),
    ])

    csr.unlink(missing_ok=True)
    cnf.unlink(missing_ok=True)
    (out / "ca.srl").unlink(missing_ok=True)
    return CertPair(crt, key)


# ---------- manifest rendering ----------

def b64(data: bytes) -> str:
    return base64.b64encode(data).decode()


def ns_manifest(name: str) -> str:
    return f"""---
apiVersion: v1
kind: Namespace
metadata:
  name: {name}
"""


def tls_secret_manifest(name: str, ns: str, pair: CertPair) -> str:
    return f"""---
apiVersion: v1
kind: Secret
metadata:
  name: {name}
  namespace: {ns}
type: kubernetes.io/tls
data:
  tls.crt: {b64(pair.crt.read_bytes())}
  tls.key: {b64(pair.key.read_bytes())}
"""


def docker_secret_manifest(name: str, ns: str, dockerconfig_b64: str) -> str:
    return f"""---
apiVersion: v1
kind: Secret
metadata:
  name: {name}
  namespace: {ns}
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: {dockerconfig_b64}
"""


def ca_cert_secret_manifest(name: str, ns: str, ca: CertPair) -> str:
    return f"""---
apiVersion: v1
kind: Secret
metadata:
  name: {name}
  namespace: {ns}
  labels:
    app.kubernetes.io/managed-by: Helm
type: Opaque
data:
  runai-ca.pem: {b64(ca.crt.read_bytes())}
"""


def build_dockerconfig(token: str, username: str, registries: list[str]) -> bytes:
    auth = b64(f"{username}:{token}".encode())
    cfg = {"auths": {reg: {"auth": auth} for reg in registries}}
    return json.dumps(cfg, indent=2).encode()


# ---------- driver ----------

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--fqdn", default=DEFAULT_FQDN, required=DEFAULT_FQDN is None,
                    help="Main Run:ai FQDN, e.g. runai.example.com")
    ap.add_argument("--wildcard-fqdn", default=DEFAULT_WILDCARD_FQDN,
                    help="Wildcard subject for host-routing (default: *.<fqdn>)")
    ap.add_argument("--inference-fqdn", default=DEFAULT_INFERENCE_FQDN,
                    help="Inference wildcard subject (default: *.inference.<fqdn>)")
    ap.add_argument("--jfrog-token", default=DEFAULT_JFROG_TOKEN, required=DEFAULT_JFROG_TOKEN is None,
                    help="JFrog access token / API key")
    ap.add_argument("--jfrog-user", default=DEFAULT_JFROG_USER,
                    help=f"JFrog username (default: {DEFAULT_JFROG_USER}, the bearer-style placeholder)")
    ap.add_argument("--registry", action="append", default=None,
                    help=f"Repeat to add registry to the dockerconfigjson "
                         f"(default: {', '.join(DEFAULT_REGISTRIES)})")
    ap.add_argument("--out-dir", default=DEFAULT_OUT_DIR,
                    help=f"Output directory for cert files + secrets.yaml (default: {DEFAULT_OUT_DIR})")
    ap.add_argument("--dry-run", action="store_true", default=DEFAULT_DRY_RUN,
                    help="Render to disk; do not kubectl apply")
    args = ap.parse_args()

    require("openssl")
    if not args.dry_run:
        require("kubectl")

    out = Path(args.out_dir)
    out.mkdir(parents=True, exist_ok=True)

    wildcard_fqdn = args.wildcard_fqdn or f"*.{args.fqdn}"
    inference_fqdn = args.inference_fqdn or f"*.inference.{args.fqdn}"
    registries = args.registry or list(DEFAULT_REGISTRIES)

    print(f"[runai-secrets] FQDN              : {args.fqdn}")
    print(f"[runai-secrets] wildcard FQDN     : {wildcard_fqdn}")
    print(f"[runai-secrets] inference FQDN    : {inference_fqdn}")
    print(f"[runai-secrets] dockerconfig regs : {registries}")
    print(f"[runai-secrets] out dir           : {out}")

    print("[runai-secrets] generating self-signed CA")
    ca = gen_ca(out)

    print("[runai-secrets] generating leaf certs")
    backend = gen_leaf(out, ca, "runai-backend-tls", args.fqdn, [args.fqdn])
    cluster = gen_leaf(out, ca, "runai-cluster-domain-tls-secret", args.fqdn, [args.fqdn])
    star = gen_leaf(out, ca, "runai-cluster-domain-star-tls-secret",
                    wildcard_fqdn, [wildcard_fqdn, args.fqdn])
    inference = gen_leaf(out, ca, "runai-inference-tls",
                         inference_fqdn, [inference_fqdn])

    print("[runai-secrets] building dockerconfigjson")
    dockerconfig = build_dockerconfig(args.jfrog_token, args.jfrog_user, registries)
    docker_b64 = b64(dockerconfig)

    print("[runai-secrets] rendering secrets.yaml")
    blocks: list[str] = []
    blocks.append(ns_manifest(NS_BACKEND))
    blocks.append(ns_manifest(NS_CLUSTER))

    blocks.append(docker_secret_manifest("runai-reg-creds", NS_BACKEND, docker_b64))
    blocks.append(docker_secret_manifest("runai-reg-creds", NS_CLUSTER, docker_b64))

    blocks.append(tls_secret_manifest("runai-backend-tls", NS_BACKEND, backend))
    blocks.append(tls_secret_manifest("runai-cluster-domain-tls-secret", NS_CLUSTER, cluster))
    blocks.append(tls_secret_manifest("runai-cluster-domain-star-tls-secret", NS_CLUSTER, star))

    blocks.append(ca_cert_secret_manifest("runai-ca-cert", NS_BACKEND, ca))
    blocks.append(ca_cert_secret_manifest("runai-ca-cert", NS_CLUSTER, ca))

    manifest = "".join(blocks)
    manifest_path = out / "secrets.yaml"
    manifest_path.write_text(manifest)
    print(f"[runai-secrets] manifests written: {manifest_path}")

    if args.dry_run:
        print("[runai-secrets] dry-run: not applying. Inspect with:")
        print(f"    less {manifest_path}")
        return 0

    print("[runai-secrets] kubectl apply -f -")
    sh(["kubectl", "apply", "-f", "-"], input_bytes=manifest.encode())

    print("[runai-secrets] applied. Inference TLS not applied (knative-serving ns absent).")
    print(f"[runai-secrets] CA cert         : {ca.crt}")
    print(f"[runai-secrets] inference cert  : {inference.crt}  (apply later in {NS_KNATIVE})")
    print()
    print("Add to your `aicr bundle ...` invocation:")
    print(f"  --set runaicp:global.domain={args.fqdn} \\")
    print(f"  --set runaicp:global.customCA.enabled=true")
    print()
    print("If/when you add the runai-cluster component, also pass:")
    print(f"  --set runaicluster:controlPlane.url={args.fqdn} \\")
    print(f"  --set runaicluster:cluster.url={args.fqdn} \\")
    print(f"  --set runaicluster:global.customCA.enabled=true")
    return 0


if __name__ == "__main__":
    sys.exit(main())
