# localformat test fixtures

Golden-file fixtures for `localformat.Write`'s per-folder output. Each
subdirectory captures the bytes a single bundle folder of a particular
kind looks like, and the harness in
[`writer_test.go`](../writer_test.go) (`assertGolden`) byte-compares.

| Directory | Folder kind under test |
|---|---|
| `upstream_helm_only/` | `KindUpstreamHelm` — folder with `install.sh` + `upstream.env`, no `Chart.yaml` |
| `local_helm_manifest_only/` | `KindLocalHelm` for a manifest-only component — `Chart.yaml` + `templates/` + values |
| `kustomize_input/` | Input to `buildKustomize` (kustomization.yaml + resource); not a golden — fed into the kustomize build path |

For background on the pattern, regen command, and why these files don't
carry Apache license headers, see
[`pkg/bundler/deployer/helm/testdata/README.md`](../../helm/testdata/README.md).

## Regenerate

```bash
go test ./pkg/bundler/deployer/localformat/... -update
```
