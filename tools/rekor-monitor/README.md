# rekor-monitor: AICR release-signer transparency-log monitor (Rekor v2)

Watches the Rekor **v2** transparency log for AICR's release supply chain. On
each run it does two checks and, if either is unhappy, exits non-zero and prints
a `CLASSIFICATION=` line so the calling workflow can alert and branch on
severity (see "Exit status and classification" below):

- **Consistency**: proves the log stayed append-only from the last checkpoint
  to the current tree head (an O(log n) tile proof). Its main job here is to
  *anchor the incremental identity scan*: it guarantees nothing was removed or
  rewritten between runs, which is what makes "scan only the new window" sound
  (otherwise a rewritten view could hide a malicious entry). It is also the
  standard append-only tamper check, though ecosystem witnesses are the primary
  guarantee of that.
- **Identity**: scans the entries added since the last checkpoint for AICR's
  release signing identity. An entry under that identity for a tag with no
  corresponding release signals OIDC/key compromise. This is the AICR-specific
  check. Matches whose `@refs/tags/<tag>` is a known release are suppressed as
  expected signings (see `--known-tags-file`), so a legitimate release does not
  alert; an unknown tag, a malformed SAN, or a missing allowlist still alert
  (fail closed).

It is run hourly by `.github/workflows/rekor-monitor.yaml`.

## Why not the upstream `sigstore/rekor-monitor` reusable workflow?

**Because upstream cannot monitor the public-good Rekor v2 that AICR signs to.**

The upstream reusable workflow decides which Rekor API version to talk to (and
which shards to read) from Sigstore's **default** signing config,
`signing_config.v0.2.json`. That config lists **only Rekor v1**, and per
Sigstore's [rekor-evolution](https://blog.sigstore.dev/rekor-evolution/) plan it
keeps v1 as the ecosystem default "for the foreseeable future".

AICR opted into Rekor v2 **early** (see [#1650](https://github.com/NVIDIA/aicr/issues/1650))
via a *separate* TUF target, `signing_config_rekor_v2.v0.2.json`, which the
upstream tool never reads and exposes no flag to select. Both its version
detection (`getRekorVersion`) and its v2 shard discovery (`RefreshSigningConfig`)
go through `root.GetSigningConfig`, which is hardcoded to the v1-only default
target. So passing it a v2 shard URL just falls through to v1, and its v1 client
then times out against the v2 tile endpoint. (We tried exactly that first; see
the history on [#1623](https://github.com/NVIDIA/aicr/issues/1623).)

This tool changes **only that one AICR-specific bit**: it reads the v2 signing
config AICR actually signs against (`pkg/trust`), and otherwise **reuses the
upstream rekor-monitor library packages** (`pkg/rekor/v2`, `pkg/tiles`,
`pkg/identity`) for the security-critical verification, so we are not
reimplementing transparency-log crypto, just pointing it at the right config.

When Sigstore makes v2 the ecosystem default, `signing_config.v0.2.json` will
list the v2 shards, the upstream reusable workflow can monitor v2 directly, and
**this tool can be retired**. Until then, upstream has no flag to target a
non-default signing config.

## Layout

| File | Responsibility |
|------|----------------|
| `doc.go` | Package doc: rationale (above) and structure, surfaced by `go doc`. |
| `main.go` | Flags, `run()` orchestration, process exit codes. |
| `monitor.go` | `monitor` (resolved v2 shards + watched identity) and its two checks; the `outcome` of a pass. |
| `checkpoint.go` | `checkpointStore`: restore the cursor from the fetched artifact zip, read the last checkpoint, write the new one. |

## Usage

```bash
go run ./tools/rekor-monitor \
  --file checkpoint_v2.txt \
  --restore-zip checkpoint.zip \
  --cert-subject '^https://github\.com/NVIDIA/aicr/\.github/workflows/on-tag\.yaml@refs/tags/.*$' \
  --cert-issuer '^https://token\.actions\.githubusercontent\.com$' \
  --known-tags-file known-tags.txt
```

| Flag | Purpose |
|------|---------|
| `--file` | Path to the persisted v2 checkpoint (the cursor). Missing/empty = first run: baseline at the current head and skip the identity scan. |
| `--restore-zip` | Optional GitHub-artifact zip to seed `--file` from before monitoring. Missing file = first run. |
| `--cert-subject` | Regex for the monitored certificate SAN. Empty = consistency-only (no identity scan). |
| `--cert-issuer` | Regex for the monitored certificate issuer (requires `--cert-subject`). |
| `--known-tags-file` | Optional file of newline-separated known release tags. Identity matches whose `@refs/tags/<tag>` is listed are suppressed as expected releases. Omitting the flag or pointing it at an existing empty file disables suppression (all matches alert); a set-but-missing or unreadable file is a hard error (exit `2`), so a broken correlation input fails the run rather than silently alerting on everything. |
| `--user-agent` | User-Agent for requests to the log. |

## Exit status and classification

On a completed run the process prints a final `CLASSIFICATION=<value>` line to
stdout and exits with the matching code below. A pre-run input error is the
exception: invalid arguments or an unreadable `--known-tags-file` exit `2`
before the run starts and print no classification line.

| Exit | Classification | Meaning |
|------|----------------|---------|
| `0` | `clean` | Consistency verified and no unexpected identity entry; the cursor advanced. |
| `1` | `tamper` | The consistency (Merkle) proof failed: the log did not verify as append-only. |
| `1` | `identity` | An entry under the release identity for a tag with no corresponding release (or an entry that failed verification). |
| `3` | `operational` | Could not complete the check (Sigstore/Rekor/TUF/network trouble, or a timeout). Operational errors are retried a few times before this is returned. |
| `2` | (none) | A pre-run input error: invalid arguments, or a set-but-unreadable `--known-tags-file`. |

Only `tamper` and `identity` are security signals; the calling workflow pages maintainers on those and treats `operational` as infrastructure noise.
