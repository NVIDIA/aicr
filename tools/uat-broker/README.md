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
# daytime-intent=training
```

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

```sh
git tag -l 'v*' | uat-broker schedule --previous-n 2
# {
#   "aws-h100": [
#     { "reservation": "aws-h100", "aicr_version": "",       "is_main": true  },
#     { "reservation": "aws-h100", "aicr_version": "v2.0.0",  "is_main": false },
#     { "reservation": "aws-h100", "aicr_version": "v1.10.0", "is_main": false }
#   ],
#   "gcp-h100": [ ... ]
# }
```

Flags: `--file` (registry path, default `infra/uat/reservations.yaml`),
`--reservations a,b` (override the registry's reservation set),
`--previous-n N` (default 2), `--include-main` (default true).

## Exit codes

Follows the `pkg/errors` coded contract: `0` success, `2` invalid
request / bad flags, `3` reservation not found. Other coded failures map
to their `pkg/errors` exit codes too — e.g. `5` (timeout) when a
SIGINT/SIGTERM interrupts a blocking stdin read, and `8` (internal) on a
stdout write or JSON-encode failure.
