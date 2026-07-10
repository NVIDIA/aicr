# Evidence Dashboard Publish (GP5)

The dashboard-publish pipeline is the repo's **first GitHub Pages** surface.
On every merge to `main` (and on demand) it regenerates the interim-evidence
dashboard from the source-keyed evidence tree in GCS and deploys the static
site to GitHub Pages. It is the consumer end of the chain whose producer is
[evidence-ingest (GP2)](evidence-ingest.md): ingest writes the tree, publish
renders and serves it. (For the full `GPn` stage map of the
evidence-corroboration pipeline, see
[evidence-ingest.md](evidence-ingest.md).)

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
- The build passes `-allowlist recipes/evidence/allowlist.yaml` to the
  generator, re-deriving each source's class from its verified signer against
  the in-tree GP1 allowlist — defense in depth on top of the class GP2 baked
  into `meta.json` at ingest time. `pkg/corroborate`'s loader delegates to
  the shared `pkg/evidence/allowlist` parser, so it reads the canonical
  `identityPattern`/`source` schema directly (#1505).
- The Pages site is served at the custom domain `validation.aicr.run`: the
  publish workflow reasserts it on every run by writing a `CNAME` file into
  the site artifact (a deploy whose artifact omits `CNAME` would otherwise
  silently clear the setting).
