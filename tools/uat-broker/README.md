# uat-broker

Day/night UAT broker helper (#1274, DC1). Reads the reservation registry
(`infra/uat/reservations.yaml`) and expands the nightly version-matrix
schedule. It holds no credentials and performs no network or git I/O — the
calling workflow feeds it the registry path and the raw `git tag` list on
stdin. Business logic lives in [`pkg/uatbroker`](../../pkg/uatbroker); this
package is a thin CLI over it.

## Build

```sh
GOFLAGS=-mod=vendor go build -o ./bin/uat-broker ./tools/uat-broker
```

## Subcommands

### `reservations`

Resolve one reservation row to `GITHUB_OUTPUT`-style `key=value` lines:

```sh
uat-broker reservations --name aws-h100 >> "$GITHUB_OUTPUT"
# cloud=aws
# reservation-id=cr-0cbe491320188dfa6
# accelerator=h100
# gpu-count=8
# cluster-config-path=tests/uat/aws/cluster-config.yaml
# test-config-dir=tests/uat/aws/tests
# nightly-intents=training,inference
# daytime-intent=training
```

`nightly-intents` is the comma-separated list of intents the nightly batch runs
on this reservation (#1276, DC3); it is emitted **resolved** (an un-annotated
reservation reports the `training` default rather than an empty value). The
launch set is `training,inference` on both reservations, so both CUJs run
nightly on both clouds — the controller splits on comma and dispatches one
serialized cell per intent per version.

List every reservation name (one per line):

```sh
uat-broker reservations --list
```

Print the daytime human-access rotation (#1281, DC8) as JSON — one
`{reservation, intent}` entry per row with a non-empty `daytime-intent`,
in document order — for the daytime scheduler's dispatch matrix:

```sh
uat-broker reservations --daytime | jq -c .
# [{"reservation":"aws-h100","intent":"training"},{"reservation":"gcp-h100","intent":"inference"}]
```

The output is pretty-printed; the daytime scheduler compacts it with `jq -c`
into a one-line `strategy.matrix.include` array.

`--name`, `--list`, and `--daytime` are mutually exclusive.

### `schedule`

Expand the ordered nightly version matrix as JSON — the tip-of-main cell
first, then the previous N stable releases in descending semver order, per
reservation. Candidate tags are read from stdin; pre-release and
non-semver tags are dropped. Cells are ordered newest-first so the nightly
controller drops the oldest releases first when its time-box closes.

Each cell carries `intents` — the nightly intents eligible at that cell's
version. The main cell carries every intent the reservation runs; a release
cell drops any intent gated off by the row's `nightly-intent-min-versions`
(a release older than an intent's minimum version). The controller dispatches
one run per entry, so a fully-gated release cell dispatches nothing.

```sh
git tag -l 'v*' | uat-broker schedule --previous-n 2
# {
#   "azure-h100": [
#     { "reservation": "azure-h100", "aicr_version": "",       "is_main": true,  "intents": ["training","inference"] },
#     { "reservation": "azure-h100", "aicr_version": "v2.0.0",  "is_main": false, "intents": ["training","inference"] },
#     { "reservation": "azure-h100", "aicr_version": "v0.17.0", "is_main": false, "intents": ["training"] }
#   ]
# }
```

Flags: `--file` (registry path, default `infra/uat/reservations.yaml`; always
loaded, since each cell's eligible intents come from the row),
`--reservations a,b` (schedule a subset — each name must exist in `--file`),
`--previous-n N` (default 2), `--include-main` (default true).

## Exit codes

Follows the `pkg/errors` coded contract: `0` success, `2` invalid
request / bad flags, `3` reservation not found. Other coded failures map
to their `pkg/errors` exit codes too — e.g. `5` (timeout) when a
SIGINT/SIGTERM interrupts a blocking stdin read, and `8` (internal) on a
stdout write or JSON-encode failure.
