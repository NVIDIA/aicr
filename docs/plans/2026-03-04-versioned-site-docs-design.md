# Versioned Site Documentation

## Problem

The project site (`aicr.dgxc.io`) is not wired into the release pipeline. The
`gh-pages.yaml` workflow triggers only on `site/**` changes to main, while
`on-tag.yaml` never rebuilds the site. Documentation has drifted from released
code (e.g., references to `v0.8.0` when latest is `v0.8.11`).

## Decision

**Approach A: Hugo Multi-Build with Composite Deploy.** Each release builds Hugo
once per retained version tag, composites the outputs into a single GitHub Pages
artifact, and deploys. Every deploy is a clean build from tagged source — no
mutable branch state, no artifact management.

## URL Structure

```
aicr.dgxc.io/                    → 302 redirect to /vX.Y.Z/ (latest)
aicr.dgxc.io/v0.8.11/            → docs built from v0.8.11 tag
aicr.dgxc.io/v0.8.11/docs/...    → versioned content pages
aicr.dgxc.io/v0.8.10/            → previous release
aicr.dgxc.io/v0.8.9/             → oldest retained
```

Root `index.html` is a static meta-refresh + JS redirect to the latest version.

## Retention

Keep the 3 most recent semver tags. Older versions are dropped on each deploy.

## Hugo Config Changes

Each version build overrides:
- `baseURL` → `https://aicr.dgxc.io/vX.Y.Z/`
- `params.versions` → dynamically generated list of 3 retained versions

The existing `version_menu` and `versions` params in `hugo.yaml` already support
Docsy's version dropdown. The build script generates the list at build time
instead of hardcoding it.

## New Composite Action

**`.github/actions/build-versioned-site/action.yml`**

Inputs:
- `current_tag` — the tag being released (e.g., `v0.8.11`)
- `retention_count` — number of versions to keep (default: `3`)
- `hugo_version`, `node_version`, `go_version` — from load-versions

Steps:
1. Determine N most recent semver tags via `git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sort -V -r | head -N`
2. For each tag:
   a. Check out the tag's `site/` content into a temp directory
   b. Override `baseURL` and `params.versions` in `hugo.yaml`
   c. Run `npm ci` and `hugo --minify --destination <output>/<tag>/`
3. Generate root `index.html` with redirect to latest tag
4. Output: combined directory ready for `upload-pages-artifact`

Tool versions come from the **current** `.settings.yaml` (not the tag's) to
avoid compatibility issues.

## Release Workflow Integration

New `site` job in `on-tag.yaml`, after `publish`, before `summary`:

```yaml
site:
  name: Deploy Site
  needs: [publish]
  runs-on: ubuntu-latest
  timeout-minutes: 15
  permissions:
    contents: read
    pages: write
    id-token: write
  environment:
    name: github-pages
  steps:
    - checkout (fetch-depth: 0)
    - load-versions
    - setup Go, Hugo, Node
    - build-versioned-site action
    - upload-pages-artifact
    - deploy-pages
```

The `summary` job adds `site` to its `needs` list and reports its status.

## Existing gh-pages.yaml Changes

- **PR builds**: unchanged — single unversioned preview build
- **Main push**: build versioned site using the same composite action
- Add `workflow_call` trigger so `on-tag.yaml` can invoke it as a reusable
  workflow instead of duplicating steps

## Files Changed

| File | Change |
|------|--------|
| `.github/actions/build-versioned-site/action.yml` | New composite action |
| `.github/workflows/on-tag.yaml` | Add `site` job after `publish` |
| `.github/workflows/gh-pages.yaml` | Add `workflow_call`, use composite action for main builds |
| `site/hugo.yaml` | Remove hardcoded `versions` list (generated at build time) |

## Unresolved Questions

1. Should the site job be blocking for the release (i.e., if site deploy fails,
   does the release summary show failure)? Current design: yes, it's in the
   `summary` needs chain, but it runs after `publish` so the release is already
   public.
2. Should we backfill versioned docs for existing tags (v0.8.9, v0.8.10,
   v0.8.11) with a one-time manual workflow dispatch, or wait for the next
   release (v0.8.12) to produce the first versioned deploy?
