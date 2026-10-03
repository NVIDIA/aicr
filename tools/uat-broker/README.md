# uat-broker

Day/night UAT broker helper (#1274, DC1). Reads the reservation registry
(`infra/uat/reservations.yaml`) and the harness-compat floors
(`tests/uat/compat.yaml`, #2860), and expands the nightly version-matrix
schedule. It holds no credentials and performs no network or git I/O — the
calling workflow feeds it file paths, the raw `git tag` list on stdin, and the
git-derived tag-containment file for `compat check`. Business logic lives in [`pkg/uatbroker`](../../pkg/uatbroker); this
package is a thin CLI over it.

## Build

```sh
go build -o ./bin/uat-broker ./tools/uat-broker
```

## Subcommands

### `reservations`

Resolve one reservation row to `GITHUB_OUTPUT`-style `key=value` lines:

```sh
uat-broker reservations --name aws-h100 >> "$GITHUB_OUTPUT"
# slug=ah1
# cloud=aws
# reservation-id=cr-0e16ad417f9a5bf69
# accelerator=h100
# gpu-count=8
# cluster-config-path=tests/uat/aws/cluster-config.yaml
# test-config-dir=tests/uat/aws/tests
# nightly-intents=training,inference
# daytime-intent=training
```

`slug` is the short (2-4 char) registry-unique discovery key the daytime cluster
name embeds — `aicr-uat-day-<slug>-<slot>-<run_id>` (ADR-017); `uat-run.yaml`
forwards it to the cloud pipelines as `needs.resolve.outputs.slug`.

`nightly-intents` is the comma-separated list of intents the nightly batch runs
on this reservation (#1276, DC3); it is emitted **resolved** (an un-annotated
reservation reports the `training` default rather than an empty value). The
launch set is `training,inference` on every reservation, so both CUJs run
nightly on all reservations. The emitted CSV is the leg-level enrollment
summary and opt-out gate (an explicit empty list skips the leg); the actual
per-cell intents come from the broker's `schedule` output, further gated per
version by the harness-compat floors (see [`compat`](#compat)).

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
cell drops any intent below its lane's harness-compat floor and lists it in
`skipped` (`{intent, floor, lane, reason}`; the key is omitted when nothing is
skipped). The controller dispatches one run per `intents` entry and announces
each `skipped` entry, so a fully-gated release cell dispatches nothing but is
never silent.

```sh
git tag -l 'v*' | uat-broker schedule --previous-n 2
# {
#   "gcp-h100": [
#     { "reservation": "gcp-h100", "aicr_version": "",        "is_main": true,  "intents": ["training","inference"] },
#     { "reservation": "gcp-h100", "aicr_version": "v0.22.0", "is_main": false, "intents": ["training","inference"] },
#     { "reservation": "gcp-h100", "aicr_version": "v0.21.1", "is_main": false, "intents": ["inference"],
#       "skipped": [{ "intent": "training", "floor": "v0.22.0", "lane": "gcp", "reason": "#2705: ..." }] }
#   ]
# }
```

Flags: `--file` (registry path, default `infra/uat/reservations.yaml`; always
loaded, since each cell's eligible intents come from the row),
`--compat` (floor file, default `tests/uat/compat.yaml`),
`--ignore-floor-lines n,m` (min-release line numbers of floors NOT to honor —
the rows `compat check` rejected; a line matching no floor is an error),
`--reservations a,b` (schedule a subset — each name must exist in `--file`),
`--previous-n N` (default 2), `--include-main` (default true).

### `compat`

Harness-compat floors: release cells run a released binary against main's
`tests/uat/**`, so `tests/uat/compat.yaml` records, per lane (reservation
`cloud`) and intent, the oldest release main's fixtures still accept. Every
`compat` subcommand takes `--file` (floor file, default
`tests/uat/compat.yaml`) and `--registry` (default
`infra/uat/reservations.yaml`, used to validate lanes).

`compat list` prints one TSV row per (floor row × intent) — `line`, `lane`,
`intent`, `floor` — where `line` is the 1-based line of the row's
`min-release:` key (rows listing several intents share a line):

```sh
uat-broker compat list
# 42	gcp	training	v0.22.0
```

`compat check` proves no floor is over-high. Tags come on stdin; `--containing`
names a file with one record per floor line: the line number followed by the
tags (tab- or space-separated) containing the commit that last touched it —
`git blame --porcelain -L n,n` then `git tag --contains <sha> 'v*'`. Write the
line number even when no tag contains the commit; **omit** it only when the
git lookup failed, which marks that row inconclusive. It prints a JSON array
(`[]` when clean) and exits 0 whenever the check ran:

```json
[{ "line": 42, "lane": "gcp", "intents": ["training"], "floor": "v0.22.0",
   "kind": "over-high", "severity": "error", "expected": "v0.21.1",
   "message": "gcp/training floor v0.22.0 is above v0.21.1, ...; expected <= v0.21.1" }]
```

`kind` is `over-high` (a lower stable tag already contains the commit, or an
untagged floor is not the next patch/minor/major release), `inconclusive` (no
stable tags on stdin, or no containment record for the line) — both
`severity: error`, so the row must not be honored — or `inert`
(`severity: notice`; reported only with `--previous-n N`, when the row skips
nothing in that release window).

`compat gate --cloud C --intent I --version V` exits 0 with an `ok: ...` line
when the release meets its floor or has none, and exits 2 (`INVALID_REQUEST`)
with a message naming the floor and reason when it is below.

## Exit codes

Follows the `pkg/errors` coded contract: `0` success, `2` invalid
request / bad flags, `3` reservation not found. Other coded failures map
to their `pkg/errors` exit codes too — e.g. `5` (timeout) when a
SIGINT/SIGTERM interrupts a blocking stdin read, and `8` (internal) on a
stdout write or JSON-encode failure.
