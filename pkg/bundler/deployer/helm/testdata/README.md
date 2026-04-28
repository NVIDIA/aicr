# helm deployer test fixtures

Each subdirectory under `testdata/` is a golden-file snapshot of a complete
generated bundle for a representative recipe shape. The harness in
[`helm_test.go`](../helm_test.go) (`assertBundleGolden`) walks the freshly
generated tempdir bundle and byte-compares every file against the
checked-in tree.

The same pattern is used in the sister package's
[`pkg/bundler/deployer/localformat/testdata/`](../../localformat/testdata).

## Scenarios

| Directory | What it exercises |
|---|---|
| `upstream_helm_only/` | Bundle with a single non-OCI Helm component (cert-manager) — no Chart.yaml in folder, `upstream.env` carries CHART/REPO/VERSION |
| `manifest_only/` | Component with `defaultRepository: ""` + `manifestFiles` — wrapped chart with synthesized Chart.yaml + templates/ |
| `mixed_gpu_operator/` | Mixed component (Helm chart + raw manifests) — primary `001-gpu-operator/` (upstream-helm) plus injected `002-gpu-operator-post/` (local-helm wrapping the manifests) |
| `kai_scheduler_present/` | OCI Helm component (`oci://...`) — `upstream.env` writes the full OCI URI to CHART, leaves REPO empty; `install.sh` uses `${REPO:+--repo "${REPO}"}` so `--repo` is omitted for OCI |
| `nodewright_present/` | Bundle containing nodewright-operator — exercises the name-matched node taint cleanup block in `deploy.sh` |

## Regenerating the goldens

After any change to the helm deployer, the templates, or `localformat`:

```bash
go test -run "^TestBundleGolden_" ./pkg/bundler/deployer/helm/ -update
```

This rewrites every file under `testdata/<scenario>/` to match the freshly
generated bundle. Inspect the diff carefully — every byte change is
reviewer-visible and that is the entire point.

The rule is symmetric with `pkg/bundler/deployer/localformat`:

```bash
go test ./pkg/bundler/deployer/localformat/... -update
```

## Why these aren't real bundles

Goldens use **minimal synthetic** input where possible: a one-key
`ConfigMap`, a stub `Service` without a `spec.ports`, etc. They are NOT
meant to be installable into a real cluster. The harness asserts on
generated **bundle layout and rendered text**, not on the runtime
correctness of the manifests inside. Real-cluster runtime correctness is
covered by the chainsaw end-to-end tests under `tests/chainsaw/`.

## Why no Apache license headers on the YAML/scripts here

`testdata/**` is excluded from `make license` (see `Makefile`'s
`LICENSE_IGNORES` block). These files are test fixtures, not source
artifacts; running `addlicense` over them would corrupt the goldens by
prepending headers that the runtime generator does not emit, causing the
round-trip test to fail. Test-driven proof: removing the ignore would
break `TestBundleGolden_*` immediately on the next `make lint`.

## Adding a new scenario

1. Add a `TestBundleGolden_<Scenario>` test in `helm_test.go` mirroring the
   existing examples — construct a `Generator`, call `Generate`, then
   `assertBundleGolden(t, outDir, "testdata/<scenario_name>")`.
2. Run `go test -run TestBundleGolden_<Scenario> ./pkg/bundler/deployer/helm/ -update`
   once to materialize the golden tree.
3. Inspect the generated tree on disk. If anything looks wrong, fix the
   generator (not the golden) and re-`-update`. The golden should be a
   faithful capture of what `Generate` actually produces.
4. Commit the test plus the entire `testdata/<scenario_name>/` directory.

The tree of goldens you check in becomes a reference catalog of "this is
what a bundle of shape X looks like" — a deliberate side effect of the
pattern that helps reviewers understand the deployer output without
running anything locally.
