# Adding a CLI Command

Contributor guide for `pkg/cli`. For end-user flag reference see
[docs/user/cli-reference.md](../user/cli-reference.md).

`pkg/cli` is a user-interaction package built on
[`urfave/cli/v3`](https://github.com/urfave/cli). It parses flags, loads
optional config files, formats output, and maps errors to exit codes. It
**must not contain business logic**. Recipe resolution, bundle
generation, snapshot capture, validation, evidence handling — all of it
lives in functional packages and is composed by the `pkg/client/v1`
`aicr.Client` facade. Handlers in `pkg/cli` are thin adapters over that
facade, the same way `pkg/server` handlers are. Crossing this boundary
will be rejected in review.

`cmd/aicr/main.go` is a one-line entry point that calls
`cli.Execute()`. `Execute` builds the root command tree, runs it with a
`signal.NotifyContext` (SIGINT/SIGTERM), and on return calls
`os.Exit(errors.ExitCodeFromError(err))`. Handlers never call
`os.Exit` themselves — they return errors.

## Command Inventory

Subcommands registered in `pkg/cli/root.go` (`Commands:` slice on
`newRootCmd`):

| Subcommand | File | Purpose |
|------------|------|---------|
| `snapshot` | `snapshot.go` | Collect cluster/OS/GPU state; write file, stdout, or `cm://ns/name` ConfigMap. Can deploy a one-shot Job. |
| `recipe` | `recipe.go` | Resolve a recipe from criteria flags or a snapshot; emit the hydrated spec. |
| `query` | `query.go` | Extract a single hydrated value from a recipe (`--selector components.gpu-operator.values.driver.version`). |
| `bundle` | `bundle.go` | Render per-component deployment artifacts from a recipe via a chosen deployer (`helm`, `helmfile`, `argocd`, `argocdhelm`, `flux`). |
| `bundle verify` | `bundle_verify.go` | Verify a bundle's Sigstore signature and OCI digest. |
| `validate` | `validate.go` | Evaluate recipe constraints against a snapshot or live cluster; optionally emit evidence. |
| `evidence digest` | `evidence_digest.go` | Print the canonical digest of a resolved recipe (offline). |
| `evidence publish` | `evidence_publish.go` | Sign and push an already-emitted evidence bundle; write its pointer. |
| `evidence verify` | `evidence_verify.go` | Verify integrity claims on an evidence bundle (offline or registry). |
| `diff` | `diff.go` | Structural diff between two recipes or snapshots. |
| `mirror` / `mirror list` | `mirror.go` | Mirror charts and images referenced by a recipe to an air-gapped registry; list what would be mirrored. |
| `trust update` | `trust.go` | Refresh the Sigstore TUF trust root used by `verify` / `evidence verify`. |
| `skill` | `skill.go` | Generate an agent skill file (Claude Code, Codex) from the live CLI command tree. |

Each `*Cmd()` factory returns a `*cli.Command`. Verb-group parents
(`evidence`, `mirror`, `trust`, `bundle verify`) declare their
subcommands in their own `Commands:` slice; see `evidence.go` for the
canonical shape.

## Adding a New Subcommand

Mechanical walkthrough. Pick an existing command of similar shape
(`query.go` for a read-only single-value command, `validate.go` for a
cluster-touching one) and follow its pattern rather than inventing one.

**1. Create `pkg/cli/<name>.go`.** Export a single factory:

```go
func myCmdCmd() *cli.Command {
    return &cli.Command{
        Name:     "mycmd",
        Category: functionalCategoryName, // groups it under "Functional" in --help
        Usage:    "One-line summary.",
        Description: `Longer description shown by --help.`,
        Flags: []cli.Flag{
            outputFlag(),     // shared flag factory from root.go
            formatFlag(),     // shared
            configFlag(),     // shared (enables --config)
            &cli.StringFlag{
                Name:     "thing",
                Usage:    "the thing",
                Category: catInput, // see consts.go for category labels
            },
        },
        Action: myCmdAction,
    }
}

func myCmdAction(ctx context.Context, cmd *cli.Command) error {
    // 1. Catch repeated single-value flags (urfave/cli/v3 accepts them
    //    silently otherwise).
    if err := validateSingleValueFlags(cmd, "thing", "output", "format", "config"); err != nil {
        return err
    }

    // 2. Optional: load --config file. (nil, nil) when --config not set.
    cfg, err := loadCmdConfig(ctx, cmd)
    if err != nil {
        return err
    }

    // 3. Build a per-command Client. Owns its own DataProvider; must Close.
    client, err := recipeClientFromCmd(cmd, cfg)
    if err != nil {
        return err
    }
    defer client.Close()

    // 4. Call the facade. All business logic lives there.
    result, err := client.ResolveRecipe(ctx, aicr.RecipeRequest{ /* ... */ })
    if err != nil {
        return err // pkg/errors code propagates to exit code
    }

    // 5. Write output through cmd.Root().Writer for testability.
    format, err := parseOutputFormat(cmd)
    if err != nil {
        return err
    }
    return writeResult(cmd.Root().Writer, result, format)
}
```

**2. Register in `root.go`.** Add `myCmdCmd()` to the `Commands:` slice
on `newRootCmd`. `setShellComplete` walks subcommands recursively so no
completion wiring is needed beyond `withCompletions` on enum flags.

**3. Tests.** Add `pkg/cli/<name>_test.go`. Build a `cli.Command`, set
`cmd.Writer = &bytes.Buffer{}`, call `cmd.Run(ctx, args)`, and assert
on buffer contents and error code. See `recipe_test.go` or
`query_test.go` for the template.

**4. Docs.** Add an entry in `docs/user/cli-reference.md` (flag table)
and update this file's Command Inventory table.

## The `pkg/client/v1` Facade Boundary

`pkg/cli` calls into the facade; the facade calls into functional
packages (`pkg/recipe`, `pkg/bundler`, `pkg/snapshotter`,
`pkg/validator`, `pkg/evidence`, ...). Public entry points
(`pkg/client/v1/aicr.go`):

| Facade method | Used by |
|---------------|---------|
| `NewClient(opts...)` / `Close()` | All commands. Construct with `WithRecipeSource(EmbeddedSource())` or `FilesystemSource(dir)` and `WithVersion(version)`. Each `Client` owns its own `DataProvider`. |
| `ResolveRecipe(ctx, RecipeRequest)` | `recipe`, `query` (request can hold criteria, file path, or snapshot input) |
| `ResolveRecipeFromCriteria(ctx, *Criteria)` | criteria-only fast path |
| `ResolveRecipeFromSnapshot(ctx, *Criteria, *Snapshot)` | `validate`, `recipe --snapshot` |
| `LoadRecipe(ctx, path, kubeconfig)` | `bundle`, `validate`, `diff` (read a previously emitted recipe file) |
| `BundleComponents(ctx, *RecipeResult)` | `bundle` |
| `CollectSnapshot(ctx, *AgentConfig)` | `snapshot` |
| `ValidateState(ctx, ...)` | `validate` |

Construction in CLI happens via `recipeClientFromCmd(cmd, cfg)` in
`root.go` — it reads `--data` (or `cfg.Recipe().DataDir()`), picks
`FilesystemSource` vs `EmbeddedSource`, and threads `version` through
to `Metadata.Version`. Callers **must** `defer client.Close()`; the
client owns goroutines that drain on Close.

Adding business logic in the handler — recipe resolution loops, bundle
rendering, validator orchestration, OCI pushes — is a boundary
violation. If the facade is missing the surface you need, add it to
`pkg/client/v1` first.

## Output Writers

User-facing output goes through `cmd.Root().Writer`, never
`fmt.Println` / `fmt.Printf` to stdout. Tests capture by assigning
`cmd.Writer = &bytes.Buffer{}` before `cmd.Run`; printing directly to
stdout breaks that capture and the `root_test.go` pattern.

```go
// GOOD
fmt.Fprintln(cmd.Root().Writer, "done")

// BAD — bypasses the writer, can't be captured in tests
fmt.Println("done")
```

Long structured output uses `pkg/serializer` (deterministic YAML/JSON).
Diagnostic / debug messages go through `slog`, not the user writer.

## Flag Factory Pattern

Shared flags are declared as **functions returning `cli.Flag`**, not
package vars (see `root.go`):

```go
outputFlag      = func() cli.Flag { ... }
formatFlag      = func() cli.Flag { ... }
configFlag      = func() cli.Flag { ... }
kubeconfigFlag  = func() cli.Flag { ... }
dataFlag        = func() cli.Flag { ... }
```

Why: `urfave/cli/v3` mutates parsed state (`Count`, parsed value) on
the `cli.Flag` value itself. A single shared instance leaks parsed
state across `cmd.Run` invocations — most visible in tests that build
multiple command trees in one process. Each `Command` gets a fresh
flag instance by calling the factory.

Flag names, category labels (`catInput`, `catOutput`, `catScheduling`,
`catOCIRegistry`, …) and well-known string constants (`flagOutput`,
`flagPush`, `flagIdentityToken`, …) live in `consts.go`. `goconst`
flags any literal repeated ≥ 3 times across the package — extract it
there.

## `--config` and `loadCmdConfig`

Commands that accept a config file declare `configFlag()` and call
`loadCmdConfig(ctx, cmd)`. The loader returns `(*config.AICRConfig,
nil)` when `--config` is set, `(nil, nil)` when it is not. Errors from
`config.Load` are returned unchanged so their `pkg/errors` codes
(`ErrCodeNotFound`, `ErrCodeInvalidRequest`, `ErrCodeUnavailable`)
survive to the exit-code mapper.

Precedence is **CLI flag > config file > flag default**, implemented
by three helpers in `root.go`:

| Helper | Purpose |
|--------|---------|
| `stringFlagOrConfig(cmd, flagName, fallback)` | String flags; logs an INFO line when CLI overrides a non-empty config value. Uses `cmd.IsSet` so a flag's compile-time `Value:` default still wins when both CLI and config are unset. |
| `intFlagOrConfig(cmd, flagName, fallback)` | Int flags; symmetric guard so a config `0` is not silently overridden. |
| `durationFlagOrConfig(cmd, flagName, *fallback)` | Duration flags; `*fallback == nil` means "config did not set this field" (lets CLI default flow through), distinct from `*fallback == 0` ("explicit zero / disable timeout"). |

Use these helpers everywhere; do not call `cmd.IsSet` + manual
ternaries inline.

## `validateSingleValueFlags`

`urfave/cli/v3` silently accepts repeated single-value flags
(`--namespace a --namespace b` keeps the last). That's a usability
trap for flags like `--recipe` or `--output`. Every command's `Action`
calls `validateSingleValueFlags(cmd, names...)` as its first step:

```go
if err := validateSingleValueFlags(cmd, "recipe", "snapshot", "output",
    "config", "namespace", "image", "job-name", flagPush); err != nil {
    return err
}
```

It uses `cmd.Count(name)` to catch repeats and returns
`ErrCodeInvalidRequest` (→ exit code 2). List every single-value flag
the command declares; omitting one re-introduces the trap.

## Error → Exit Code Mapping

Handlers return errors. `Execute` in `root.go` calls
`os.Exit(errors.ExitCodeFromError(err))`. Mapping
(`pkg/errors/exitcode.go`):

| Error code | Exit code | Meaning |
|------------|-----------|---------|
| (nil) | 0 | Success |
| `ErrCodeInvalidRequest`, `ErrCodeMethodNotAllowed`, `ErrCodeConflict` | 2 | Bad input |
| `ErrCodeNotFound` | 3 | Resource missing |
| `ErrCodeUnauthorized` | 4 | Auth |
| `ErrCodeTimeout` | 5 | Deadline exceeded |
| `ErrCodeUnavailable` | 6 | Dependency unavailable |
| `ErrCodeRateLimitExceeded` | 7 | Throttled |
| `ErrCodeInternal` | 8 | Internal |
| (unstructured) | 1 | Generic |

Rules:

- Never call `os.Exit` from a handler — return the error.
- Never `fmt.Errorf` in CLI code: use `pkg/errors.New` /
  `errors.Wrap` with a code.
- Don't re-wrap an error that already has the right code; that
  overwrites it. Return as-is.
- Validate user input early and return `ErrCodeInvalidRequest`. Don't
  `slog.Warn; continue` — a `--set` typo or malformed flag must not
  ship a misconfigured artifact.

## Shell Completion

`completion_values.go` defines:

- `CompletableFlag` — interface with `Completions() []string` on top
  of `cli.Flag`.
- `completableStringFlag` — wraps `cli.StringFlag` with a completion
  function.
- `withCompletions(flag, fn)` — adornment used at flag-declaration
  time.

For enum flags, declare with `withCompletions`:

```go
return withCompletions(&cli.StringFlag{
    Name:     "intent",
    Category: catQueryParameters,
}, recipe.SupportedIntents)
```

`completeWithAllFlags` in `root.go` reads `os.Args` directly (not
`cmd.Args()`, because partial flags like `--form` fail the parser and
never land in `cmd.Args`) and emits suggestions for flag names, flag
values, and subcommands. `setShellComplete` recursively wires this on
every subcommand so aliases (e.g., `--gpu` for `--accelerator`)
appear in completions.

## Skill Plugin Generator

`aicr skill --agent claude-code|codex` generates an agent skill file
from the live CLI command tree. Adding an agent:

1. Define an `agentType` constant and add it to `supportedAgents()` in
   `skill_generator.go`.
2. Implement `skillGenerator` (`generate(meta *cliMeta) ([]byte,
   error)`, `installPath() (string, error)`) in a new
   `skill_<agent>.go`.
3. Register it in the `parseAgentType` → generator switch in
   `skill.go`.

The reflection over the command tree happens in `skill_generator.go`
so generators only consume `cliMeta`.

## Logging and Color

Configured in the root `Before` hook (`root.go`):

| Flag / env | Effect |
|------------|--------|
| `--debug` / `AICR_DEBUG` | `slog` text logger at debug level, full metadata |
| `--log-json` / `AICR_LOG_JSON` | structured JSON logger; wins over `--debug` for output format, debug level still applied |
| neither | `pkg/logging.SetDefaultCLILogger` — human-readable, TTY-aware |
| `AICR_LOG_LEVEL` | overrides level for the structured logger (unprefixed `LOG_LEVEL` is not honored) |
| `NO_COLOR` (de-facto) | suppresses ANSI color |
| stderr is not a TTY | suppresses ANSI color (`pkg/logging` detects via `golang.org/x/term`) |

User output (`cmd.Root().Writer`, defaults to stdout) is separate from
`slog` output (stderr). Tests should assert on the writer, not on log
capture.

## Testing

Pattern (`pkg/cli/*_test.go`):

```go
func TestMyCmd(t *testing.T) {
    var buf bytes.Buffer
    cmd := newRootCmd()
    cmd.Writer = &buf
    err := cmd.Run(t.Context(), []string{"aicr", "mycmd", "--thing", "x"})
    if err != nil {
        t.Fatalf("run: %v", err)
    }
    if !strings.Contains(buf.String(), "expected") {
        t.Errorf("output = %q", buf.String())
    }
}
```

Rules:

- Capture user output via `cmd.Writer` (or `cmd.Root().Writer` when
  building the tree by hand). Do not parse stderr.
- Anything that resolves a recipe against an actual cluster or
  deploys a Job must pass `--no-cluster` in tests. The validator and
  collector honor it; live-cluster tests belong in `tests/e2e` or
  `tests/chainsaw`.
- Use `t.Context()` (Go 1.24+) so signal-cancellation is exercised
  end-to-end.
- Table-driven tests for flag-precedence and config-merge cases —
  these have many small permutations and the existing tests
  (`config_helpers_test.go`, `bundle_resolve_helpers_test.go`) are
  the template.

## Anti-Patterns

| Don't | Do |
|-------|----|
| Put business logic in `pkg/cli` handlers | Call the `pkg/client/v1` facade; add the missing method there |
| Declare a shared `cli.Flag` as a package var | Declare as a function returning `cli.Flag` (urfave parsed-state leak) |
| `fmt.Println` / `fmt.Printf` to stdout | `fmt.Fprint*(cmd.Root().Writer, ...)` |
| `fmt.Errorf` for errors | `pkg/errors.New(code, msg)` / `errors.Wrap(code, msg, err)` |
| Call `os.Exit` from a handler | Return the error; `Execute` maps it via `ExitCodeFromError` |
| Skip `validateSingleValueFlags` for "obvious" flags | List every single-value flag the command declares |
| `slog.Warn; continue` on user input or config parse failure | Return `ErrCodeInvalidRequest` |
| Re-wrap an error that already has the correct code | Return it as-is |
| Forget `defer client.Close()` after `recipeClientFromCmd` | Always defer Close — the client owns goroutines |
| Hardcode a string literal used in ≥ 3 files | Add it to `consts.go` (`goconst` will flag it anyway) |
