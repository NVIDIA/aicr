# The facade architecture gate

`pkg/cli` and `pkg/server` are user interaction only: they parse
input, validate it, call `pkg/client/v1` (the `aicr.Client` facade),
and format output. They contain no business logic — that lives in
functional packages (`pkg/recipe`, `pkg/bundler`, `pkg/validator`,
`pkg/collector`, `pkg/evidence`, and friends), composed by the facade
so the CLI and the API server share one code path instead of two
drifting ones.

`TestFacadeBoundary` in `tests/architecture/facade_boundary_test.go`
enforces that boundary mechanically. It resolves every package under
`./pkg/cli/...` and `./pkg/server/...` (via `go list`, so a future
split like `pkg/cli/foo` is covered automatically instead of needing
to be added by hand), type-checks each one from source, walks every
symbol they reference into a package under `github.com/NVIDIA/aicr/`,
and compares that observed reference set against the committed policy
in `tests/architecture/facade-policy.yaml`. A reference the policy
doesn't account for fails the build. References between packages
within the analyzed set itself (for example a call from `pkg/cli/foo`
into `pkg/cli/bar`, or into `pkg/cli` itself) are internal wiring, not
business-logic references, and are not recorded.

The gate is deliberately not import-level. A package that is already
allowlisted can still introduce a new violation by calling a new
method on a type it already holds — no new import line appears, so an
import scan would miss it. The gate instead classifies every
individual symbol (function, type, const, var, or `Type.Method`) and
checks each one against what the policy records for it. There is no
`testing.Short()` skip: `make test` runs with `-short`, so a skip here
would disable the gate in the only place it runs. It also runs as
part of `make qualify`.

## Reading a failure

`checkAgainstPolicy` reports one of four violation kinds. The test
output names the kind, the package, the symbol, and a one-line
detail — start there.

### unclassified

A reference to a package the policy already constrains, but to a
symbol that package's `symbols:` map doesn't list — usually a new
function call, a new type, or a new method call that pulled in a
`pkg/cli` or `pkg/server` change.

**The first question is not "how do I add an exception."** It's
**"should this go through `pkg/client/v1` instead?"** Most new
business-logic references belong behind the facade: add the
capability to `aicr.Client` (or an existing facade method) and call
that from the CLI or server handler. Recording a policy exception is
the fallback for the cases the facade genuinely can't or shouldn't
cover — see [When an exception is warranted](#when-an-exception-is-warranted) below — not
the default response to a red gate.

### class-changed

A symbol the policy already permits, but at a different class than
recorded — for example, the policy says `type` (holding a value,
reading a field) and the observed use is now `behavioral` (calling a
method on it). This is the subtle bypass the gate is built to catch:
a type already on the allowlist gaining a new method call introduces
no new import, so only per-symbol, per-class tracking notices it.

**Fix:** the same question as `unclassified` — should the new
behavior go through the facade? If yes, route it there instead. If
the existing exception's reasoning genuinely covers the new class of
use, update the `symbols:` entry to the observed class and, if the
`reason:` no longer accurately describes what's used, update that
too.

### stale

A symbol the policy records but that `pkg/cli` and `pkg/server` no
longer reference at all. This is generally good news — a prior
exception is no longer needed.

**Fix:** delete the symbol entry from the package's `symbols:` map
(and the whole package entry if it was the last symbol). Regenerating
the policy (below) will also drop it, but hand-editing is fine for a
single stale symbol.

### unknown-package

A reference into a package that appears in none of the policy's three
buckets (`facade`, `infrastructure`, `constrained`).

**Fix:** decide which bucket it belongs in — see
[The three buckets](#the-three-buckets) below — and add it with a reason. A brand-new
constrained package needs the same `unclassified` question first:
would this be better served by extending the facade than by adding a
new exception outright?

## When an exception is warranted

Recording a `constrained` exception is legitimate when the reference
is genuinely not a resolution or business-logic gap — CLI flag-string
parsing with no facade equivalent, wire-format types kept identical to
an upstream package on purpose, or a narrow operational path (like
in-pod agent-mode collection) that the facade deliberately doesn't
model. `tests/architecture/facade-policy.yaml` has worked examples of
each of these under `constrained:`. But the exception is earned by
that reasoning, not by the gate being red — if the honest answer is
"this could go through `pkg/client/v1`, we just haven't wired it up
yet," that's a `tracking:` exception pointing at the issue that will
close the gap, not a `permanent:` one.

## The three buckets

The policy sorts every package `pkg/cli` and `pkg/server` reach into
exactly one of three buckets.

- **`facade`** — `pkg/client/v1` itself. Any reference here is always
  clean; it's the intended path.
- **`infrastructure`** — cross-cutting packages that carry no business
  logic regardless of who calls them: `pkg/defaults`, `pkg/deprecation`,
  `pkg/errors`, `pkg/header`, `pkg/logging`, `pkg/serializer`. These
  are **not symbol-enumerated** — the policy records one reason per
  package and every symbol in it is permitted, because the whole
  package is infrastructure, not a set of individually-reasoned
  exceptions.
- **`constrained`** — everything else `pkg/cli` or `pkg/server`
  legitimately reaches outside the facade. Unlike infrastructure,
  every constrained package is symbol-enumerated: each function,
  type, const, or method the code actually uses is listed with its
  class, and the package carries one `reason:` explaining why none of
  those uses are a facade gap.

## tracking vs. permanent

Every `constrained` package entry sets a `reason:` plus exactly one of
`tracking:` or `permanent:` — `policy.validate()` fails the build if
an entry sets neither or both. This isn't bookkeeping trivia; it's a
real judgment call a reviewer will check on any PR that touches the
policy:

- **`tracking: "#NNNN"`** names the issue that will remove this
  exception. Use it when the reference is a real gap the facade should
  eventually close, and someone intends to close it — for example
  `pkg/evidence/cncf` is tracked under `#2016` because the facade
  documents that it has no CNCF-evidence entry point yet.
- **`permanent: true`** asserts the exception will never go away
  because the underlying reason is structural, not a backlog item —
  for example CLI flag-string parsing that has no facade equivalent by
  design, or a documented type alias where the facade's own return
  type is the constrained package's type.

Picking `permanent` for something that's actually a deferred facade
gap hides technical debt from the tracking system that's supposed to
surface it. Picking `tracking` for something structural creates an
issue nobody will ever close. When in doubt, `tracking` is the safer
default — it costs a stale-looking issue, not a silently permanent
gap.

## Regenerating the policy

`tests/architecture/generate_test.go` holds an authoring tool, not a
gate — `TestGeneratePolicy` only runs when explicitly asked:

```bash
AICR_WRITE_FACADE_POLICY=1 go test ./tests/architecture/ -run TestGeneratePolicy
```

This mirrors the `AICR_UPDATE_GOLDEN` convention used by `pkg/recipe`'s
golden tests. It rewrites `facade-policy.yaml` from the current
observed reference set, sorting every referenced package into
`constrained` with every symbol it uses at the correct class.

**The generator does not classify.** Every package it writes goes into
`constrained`, none into `facade` or `infrastructure` — sorting a
package into the right bucket is a human judgment the tool
deliberately doesn't guess at. Every `reason:` field comes out as the
literal placeholder `"TODO: state why this is not a facade gap"`, and
every entry defaults to `permanent: true`. Both are placeholders a
human must replace before committing: write the real reason (why this
isn't a resolution gap, per [When an exception is warranted](#when-an-exception-is-warranted)
above), move packages that are genuinely infrastructure or the facade
itself out of `constrained`, and change `permanent` to `tracking:
"#NNNN"` wherever the exception is actually a deferred gap. A wrong
reason is worse than a missing one, which is why the generator
refuses to write one.

After regenerating and hand-editing, run `go test ./tests/architecture/`
to confirm `TestFacadeBoundary` passes against the edited file.

## Limitations

The gate is precise about what it checks, but a green
`TestFacadeBoundary` is not a claim of complete coverage. A reader
should know what it doesn't see.

- **Interface dispatch declared inside the analyzed set.** When
  `pkg/cli` or `pkg/server` declares its own interface and calls a
  method through a value of that interface type, the call resolves to
  the local interface method, not to whichever concrete business type
  implements it — so the call itself is not classified. This is
  narrower than it sounds: the constructor or first reference that
  produces the concrete business value is still a classified symbol
  reference, and that's where the boundary actually holds in practice.
- **Reflection.** Anything reached through `reflect` is outside the
  gate's static reach — it type-checks source, it doesn't execute it.
- **`infrastructure`-bucket membership.** Sorting a package into
  `infrastructure` exempts it from all symbol, class, and staleness
  tracking (see [The three buckets](#the-three-buckets) above). That
  sorting decision is reviewer-gated, not mechanically enforced by
  `checkAgainstPolicy` — `TestInfrastructureAllowlistIsClosed` pins
  the current package set to a literal, so growing it requires a
  deliberate test change instead of a silent policy-file edit.
