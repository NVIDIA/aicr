## Summary

<!-- What changed? Keep it short (1-2 sentences). -->

## Motivation / Context

<!-- Why is this change needed? Link issues/discussions. -->

Fixes: <!-- #123 or N/A -->
Related: <!-- #123 or N/A -->

## Type of Change

<!-- Check all that apply -->

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to change)
- [ ] Documentation update
- [ ] Refactoring (no functional changes)
- [ ] Build/CI/tooling

## Component(s) Affected

- [ ] CLI (`cmd/aicr`, `pkg/cli`)
- [ ] API server (`cmd/aicrd`, `pkg/server`)
- [ ] Recipe engine / data (`pkg/recipe`)
- [ ] Bundlers (`pkg/bundler`, `pkg/component/*`)
- [ ] Collectors / snapshotter (`pkg/collector`, `pkg/snapshotter`)
- [ ] Validator (`pkg/validator`)
- [ ] Core libraries (`pkg/errors`, `pkg/k8s`)
- [ ] Docs/examples (`docs/`, `examples/`)
- [ ] Other: ____________

## Implementation Notes

<!-- Key decisions, trade-offs, and any non-obvious behavior changes. Delete if not applicable. -->

## Testing

```bash
# Commands run (prefer `make qualify` for non-trivial changes)
make qualify
```

<!-- Summarize test results or paste relevant output -->

## Risk Assessment

<!-- Select one and explain -->

- [ ] **Low** — Isolated change, well-tested, easy to revert
- [ ] **Medium** — Touches multiple components or has broader impact
- [ ] **High** — Breaking change, affects critical paths, or complex rollout

**Rollout notes:** <!-- Migration steps, feature flags, backwards compatibility, or N/A -->

## Checklist

- [ ] Tests pass locally (`make test` with `-race`)
- [ ] Linter passes (`make lint`)
- [ ] I did not skip/disable tests to make CI green
- [ ] I added/updated tests for new functionality
- [ ] I updated docs if user-facing behavior changed
- [ ] Changes follow existing patterns in the codebase
- [ ] Commits are cryptographically signed (`git commit -S`) — [GPG signing info](https://docs.github.com/en/authentication/managing-commit-signature-verification)
