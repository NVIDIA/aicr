# Evidence Dashboard Publish (GP5)

The dashboard-publish pipeline is the repo's **first GitHub Pages** surface.
On every merge to `main` (and on demand) it regenerates the interim-evidence
dashboard from the source-keyed evidence tree in GCS and deploys the static
site to GitHub Pages. It is the consumer end of the chain whose producer is
[evidence-ingest (GP2)](evidence-ingest.md): ingest writes the tree, publish
renders and serves it.

Fern publishes the product docs to docs.nvidia.com via `publish-fern-docs.yml`;
that is a separate surface. This workflow is the only one that deploys to
GitHub Pages.

## Pipeline

```
GCS source-keyed tree (read-only)
        │
        ▼
  rsync  gs://<bucket>/results → evidence/results
        │
        ▼
  generate  corroborate -in evidence -out _site   (×2, byte-identical)
        │
        ▼
  determinism gate  diff -r _site _site_check  (fails closed)
        │
        ▼
  upload-pages-artifact (_site)
        │
        ▼
  deploy-pages → github-pages environment
```

The byte-deterministic generator (no clock, no random — see
[`tools/corroborate`](../../tools/corroborate/README.md)) makes the publish a
straight deploy: there is no drift-PR for the site. The in-job determinism
gate runs the generator twice and diffs the output, so a non-reproducible
build fails loudly instead of shipping.

## Identity and fork safety

Two jobs, neither holding the other's scope:

- **`build`** authenticates to GCS with a **read-only** (`objectViewer`)
  identity impersonated through the existing `github-actions-pool` WIF
  federation, syncs the tree, runs the generator, and uploads the Pages
  artifact. It holds `id-token: write` (for GCS WIF) but **not** `pages: write`.
  The read SA (`evidence-read@eidosx`, provisioned by
  `infra/uat-gcp-account/evidence-dashboard.tf`) is deliberately **not** the
  shared project-wide `github-actions@eidosx` SA and **not** the GP2
  `evidence-publish` write SA; a Pages publish must never run with GCS write or
  admin scope.
- **`deploy`** holds `pages: write` + `id-token: write` and deploys the
  artifact to the `github-pages` environment. It holds **no** GCS credentials,
  and additionally runs only on `refs/heads/main` — a `workflow_dispatch` run
  on a branch still builds (a preview) but never publishes a branch build over
  the single live site.

Every job is gated `if: github.repository == 'nvidia/aicr'`, so a fork never
obtains GCS credentials or Pages write.

## Tests

`tools/corroborate/publish_workflow_test.go` pins the contract that
actionlint cannot express: the deploy job declares `pages: write` +
`id-token: write` and targets the `github-pages` environment; the canonical
publish chain (`configure-pages` → `upload-pages-artifact` → `deploy-pages`)
is present; the build job uses the read-only identity (never the shared or
write SA); the determinism gate is wired; and every job carries the
canonical-repo guard. Workflow syntax is covered by the repo actionlint gate
in `merge-gate.yaml`.

## Forward limitations

- The read SA's impersonation is repository-scoped (the shared
  `github-actions-pool` provider, owned by `infra/demo-api-server`, maps only
  the repository attribute). It is least-privilege on the resource side
  (`objectViewer` on one bucket); GP3's `infra/evidence-dashboard` may tighten
  the subject condition further.
- The build does not pass `-allowlist` to the generator. Each source's class
  is already re-derived from its verified signer at GP2 ingest time and baked
  into `meta.json` (the trust gate); the generator's own allowlist
  re-derivation is deferred until `pkg/corroborate`'s loader is reconciled with
  the GP1 allowlist schema (`recipes/evidence/allowlist.yaml` uses
  `identityPattern`/`source`; the loader still expects an `identity` field).
- A custom domain for the Pages site is deferred; the site is served at the
  default `github.io` coordinate.
