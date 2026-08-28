# Vendored: Go proxy setup action

`action.yml` and `oidc-exchange.sh` in this directory are **copied verbatim from
upstream**, not authored here. Do not edit them in place — an edit breaks the
digest check in `make lint` and silently forks us from upstream.

| | |
|---|---|
| Kit version | `v0.4.0` |
| Upstream commit | `328a004f84a94f77602fb92764e6753ab1c3b3db` |
| Source | <https://github.com/NVIDIA-dev/dgxc-depproxy> |
| Adoption issue | [#2372](https://github.com/NVIDIA/aicr/issues/2372) |

The upstream commit was confirmed to be what the `v0.4.0` tag resolves to, not
just what the kit's own `VERSION` file claims.

## Why the digests are tracked

Every other action under `.github/actions/` is ours, so "drift" is meaningless
for them. This one is a vendored copy of code that will keep releasing, so
`MANIFEST.sha256` answers two questions our own actions never raise: has our copy
been edited, and are we still on the version we think we are.

`MANIFEST.sha256` ships with the kit and its paths are repo-root relative. It
also lists files we deliberately did **not** copy (`ADOPTION.md`, `VERSION`, and
two example workflows), which would collide with or add noise to this repo's
root. `tools/check-depproxy-kit` therefore verifies the two files we did take and
asserts they are present, rather than trusting `shasum --ignore-missing` alone —
that flag fails closed when *every* file is missing but passes when only some
are, so a deleted action would otherwise go green.

## Upgrading

```shell
gh release download <tag> --repo NVIDIA-dev/dgxc-depproxy

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/NVIDIA-dev/dgxc-depproxy/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

shasum -a 256 -c checksums.txt --ignore-missing
tar -xzf dgxc-goproxy-adoption-kit_<tag>.tar.gz
```

Verify the publisher **before** the contents: the signature is what makes
`checksums.txt` trustworthy, and `checksums.txt` is what makes the archive
trustworthy. Checking the archive alone proves only that it downloaded intact.

Then copy `action.yml`, `oidc-exchange.sh`, and `MANIFEST.sha256` over the files
here, update the table above, and run `make lint`.
