# registry-inventory — AICR build/CI registry egress

Statically extracts every **package/container registry host** AICR's builds, CI,
and deploy paths reach out to, from the high-reliability *structured* sources,
and gates new hosts behind a committed allowlist.

This is the programmatic, drift-checked complement to the prose egress report:
where the report explains and classifies, this tool produces a deterministic
inventory and a CI gate so a newly introduced registry can't slip in unnoticed.

## What it parses (and what it doesn't)

| Source | Extracted |
|--------|-----------|
| `recipes/registry.yaml` | Helm chart repositories (HTTP + `oci://`), pinned versions — target-cluster pulls |
| `.settings.yaml` | pinned infra container images + chart repo URLs |
| `.goreleaser.yaml`, `.ko.yaml` | compiled-in base image (pull) + ko `repositories:` (push) |
| `**/Dockerfile`, `**/*.Dockerfile` | `FROM` base images (build-stage aliases and `scratch` skipped) |
| `.github/**/*.y{a,}ml` | GitHub Actions `uses:` refs + pin quality |
| `tools/setup-tools` | dev/CI tool installers (**best-effort**): `go install`, `pip`, `apt`, `brew`, and literal download URLs (`github.com`, `get.helm.sh`, `dl.k8s.io`, `kind.sigs.k8s.io`, `raw.githubusercontent.com`) |

The shell pass is deliberately conservative: it matches on a command **verb**
(not a bare URL) so advice strings and comments — e.g. `log_info "install from
https://go.dev/dl/"` — are not mined, and it emits a **warning** for every
`curl`/`wget` line whose URL is assembled from a shell variable rather than
silently dropping it. Pins that resolve from a `${VAR}` (sourced from
`.settings.yaml` at runtime) are recorded with `pinType: var` — pinned, just not
statically resolvable here — which keeps them distinct from the genuinely
floating cases (`@latest`, an install script pinned to `/main/`).

**Deliberately out of scope** (reported as "uncovered sources" in the output, so
the inventory reads as a floor, not a ceiling):

- Other **shell installers** (`tests/uat/lib/phases.sh`, `kwok/scripts/*`).
- **Rendered chart → image** pulls. That transitive surface is owned by
  [`tools/bom`](../bom) → `docs/user/container-images.md`; duplicating it here
  would drift.
- **Sigstore endpoint constants** in Go (`pkg/defaults/sigstore.go`: fulcio,
  rekor, tuf, oidc).
- Workflow inline `image:` values and `docker/login-action` registry inputs.

It performs **no network I/O** — every input is a repo file — so it is fast and
safe to run in a unit test.

## Usage

```bash
# Emit the inventory (YAML + Markdown) to dist/registry-inventory/
make registry-inventory
# or: go run ./tools/registry-inventory -repo-root . -out-dir /tmp/egress

# Print just the distinct host set
go run ./tools/registry-inventory -repo-root . -list-hosts

# Fail if any detected host is not in registry-allowlist.yaml (the drift gate)
make registry-check
```

## The drift gate

`registry-allowlist.yaml` is the committed set of approved hosts, and is
CODEOWNER-gated to a maintainer — and the whole `tools/registry-inventory/**`
directory is too, since weakening a *detector* is as dangerous as widening the
allowlist. The tool ships **three gates under `make test`, with different
sensitivities**:

- **`TestRegistryHostsWithinAllowlist` (low-churn).** Fails when a structured
  source introduces a host not on the allowlist. A detected host *missing* from
  the allowlist is an error; an allowlisted host *no longer detected* is
  informational only (so removing a dependency doesn't hard-fail an unrelated
  PR). It **also fails on a `sourceError`** — a required source (registry.yaml,
  .settings.yaml, .goreleaser.yaml/.ko.yaml, **Dockerfiles, .github**,
  setup-tools) that failed to read/parse, hit a scanner limit, or (for a
  Dockerfile) had an unresolvable `FROM` host, and thus contributed zero/opaque
  records — so under-detection can't hide behind a green gate. `make
  registry-check` applies the same logic on the CLI.
- **`TestExternalActionsArePinned` (higher-churn).** Asserts every third-party
  `uses:` ref is pinned to a 40-char commit SHA. This fails on *any* new
  tag-pinned marketplace action, even when no new host appears — stricter than
  the host gate by design. Note it enforces pin *form*, not *owner*: all `uses:`
  collapse to the logical host `github-actions`, so a new/unknown action owner is
  not host-gated (an acknowledged gap, listed in `uncoveredSources`).
- **`TestCommittedRegistryEgressDocFresh` (low-churn).** Asserts the committed
  `docs/contributor/registry-egress.md` is a byte projection of the live host
  surface — regenerate with `make registry-docs` when it drifts.

When a PR legitimately adds a registry, the fix is a one-line allowlist addition
with a justifying comment — the same review ergonomics as the BOM freshness gate.

## Record schema

Each fact is normalized to:

```yaml
- host: nvcr.io                 # registry/package host (allowlist unit)
  packageType: container-image  # container-image | oci-helm-chart | helm-chart-http | http | github-action
                                #   | binary-release | install-script | go-module | pypi | apt | brew
  direction: pull               # pull | push
  consumer: compiled-in         # ci-runner | target-cluster | compiled-in | dev (best-effort inference)
  pinType: digest               # digest | sha | tag | branch | latest | arg | var | none
                                #   (var = pinned via a shell var resolved from .settings.yaml)
  pin: sha256:d90158b6…         # the pin value, when useful
  detail: base_image            # image/chart/action identifier
  source: .goreleaser.yaml      # repo-relative provenance
```

## Extending

The obvious next increments, highest value first:

1. Fold in the BOM's rendered images by reading `docs/user/container-images.md`'s
   registry list rather than re-rendering.
2. Add the Sigstore constants from `pkg/defaults/sigstore.go` via a small Go AST pass.
3. Capture workflow inline `image:` refs and `docker/login-action` `registry:` inputs.
4. Extend the shell pass to `tests/uat/lib/phases.sh` and `kwok/scripts/*`, and
   optionally resolve `${VAR}` pins by cross-referencing `.settings.yaml`.

Each should add records and, if it introduces a new host, an allowlist entry —
never silently widen coverage without surfacing it.
