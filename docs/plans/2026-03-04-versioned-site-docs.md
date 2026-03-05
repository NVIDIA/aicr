# Versioned Site Documentation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Deploy per-release-tag versioned documentation to `aicr.dgxc.io` with a version dropdown, subdirectory-per-version URL scheme, and automatic rebuild on each release.

**Architecture:** New composite action (`build-versioned-site`) builds Hugo once per retained tag into versioned subdirectories, generates a root redirect, and produces a single Pages artifact. The release workflow (`on-tag.yaml`) calls this after publishing. The existing `gh-pages.yaml` is refactored to use the same action for main-branch pushes.

**Tech Stack:** Hugo (Docsy theme), GitHub Actions composite actions, GitHub Pages, yq for YAML manipulation, shell scripting.

---

### Task 1: Create the build-versioned-site composite action

**Files:**
- Create: `.github/actions/build-versioned-site/action.yml`

**Step 1: Create the composite action file**

Write `.github/actions/build-versioned-site/action.yml` with the following content. This action:
- Takes `retention_count` (default 3) as input
- Discovers the N most recent semver tags
- For each tag, checks out that tag's `site/` directory, patches `hugo.yaml` with the correct `baseURL` and `versions` list, runs `npm ci` + `hugo --minify`
- Generates a root `index.html` redirect to the latest version
- Outputs the combined directory path

```yaml
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

name: 'Build Versioned Site'
description: 'Builds Hugo site for each retained version tag into versioned subdirectories'

inputs:
  retention_count:
    description: 'Number of recent versions to retain'
    required: false
    default: '3'
  hugo_version:
    description: 'Hugo version to install'
    required: true
  node_version:
    description: 'Node.js major version'
    required: true
  go_version:
    description: 'Go version'
    required: true

outputs:
  output_dir:
    description: 'Path to the combined versioned site output'
    value: ${{ steps.build.outputs.output_dir }}
  latest_version:
    description: 'The latest version tag that was built'
    value: ${{ steps.build.outputs.latest_version }}

runs:
  using: 'composite'
  steps:
    - name: Build versioned site
      id: build
      shell: bash
      env:
        RETENTION_COUNT: ${{ inputs.retention_count }}
      run: |
        set -euo pipefail

        OUTPUT_DIR="${GITHUB_WORKSPACE}/versioned-site-output"
        mkdir -p "${OUTPUT_DIR}"

        # Discover retained tags (most recent N semver tags)
        mapfile -t TAGS < <(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sort -V -r | head -n "${RETENTION_COUNT}")

        if [[ ${#TAGS[@]} -eq 0 ]]; then
          echo "::error::No semver tags found"
          exit 1
        fi

        LATEST="${TAGS[0]}"
        echo "latest_version=${LATEST}" >> "$GITHUB_OUTPUT"
        echo "Building ${#TAGS[@]} versions: ${TAGS[*]}"
        echo "Latest: ${LATEST}"

        # Build version menu YAML snippet (used by all builds)
        VERSION_YAML=""
        for TAG in "${TAGS[@]}"; do
          LABEL="${TAG}"
          if [[ "${TAG}" == "${LATEST}" ]]; then
            LABEL="${TAG} (latest)"
          fi
          VERSION_YAML="${VERSION_YAML}    - version: \"${LABEL}\"\n      url: \"https://aicr.dgxc.io/${TAG}/\"\n"
        done

        # Build each version
        for TAG in "${TAGS[@]}"; do
          echo "::group::Building ${TAG}"

          WORK_DIR="${RUNNER_TEMP}/site-build-${TAG}"
          mkdir -p "${WORK_DIR}"

          # Check out this tag's site/ directory
          git archive "${TAG}" -- site/ | tar -x -C "${WORK_DIR}"

          # Patch hugo.yaml: baseURL and versions list
          HUGO_CONFIG="${WORK_DIR}/site/hugo.yaml"

          if [[ ! -f "${HUGO_CONFIG}" ]]; then
            echo "::warning::Tag ${TAG} has no site/hugo.yaml, skipping"
            echo "::endgroup::"
            continue
          fi

          yq eval -i ".baseURL = \"https://aicr.dgxc.io/${TAG}/\"" "${HUGO_CONFIG}"
          yq eval -i ".params.version_menu = \"Versions\"" "${HUGO_CONFIG}"
          yq eval -i ".params.version_menu_pagelinks = true" "${HUGO_CONFIG}"

          # Replace versions list using a temp file to avoid inline YAML issues
          VERSIONS_FILE="${RUNNER_TEMP}/versions-${TAG}.yaml"
          echo -n "" > "${VERSIONS_FILE}"
          for VTAG in "${TAGS[@]}"; do
            VLABEL="${VTAG}"
            if [[ "${VTAG}" == "${LATEST}" ]]; then
              VLABEL="${VTAG} (latest)"
            fi
            echo "- version: \"${VLABEL}\"" >> "${VERSIONS_FILE}"
            echo "  url: \"https://aicr.dgxc.io/${VTAG}/\"" >> "${VERSIONS_FILE}"
          done
          yq eval -i ".params.versions = load(\"${VERSIONS_FILE}\")" "${HUGO_CONFIG}"

          # Install deps and build
          cd "${WORK_DIR}/site"
          npm ci --prefer-offline --no-audit 2>/dev/null
          hugo --minify --destination "${OUTPUT_DIR}/${TAG}/"
          cd "${GITHUB_WORKSPACE}"

          echo "Built ${TAG} -> ${OUTPUT_DIR}/${TAG}/"
          echo "::endgroup::"
        done

        # Generate root redirect to latest
        cat > "${OUTPUT_DIR}/index.html" <<REDIRECT_EOF
        <!DOCTYPE html>
        <html lang="en">
        <head>
          <meta charset="utf-8">
          <meta http-equiv="refresh" content="0; url=/${LATEST}/">
          <link rel="canonical" href="https://aicr.dgxc.io/${LATEST}/">
          <title>Redirecting to ${LATEST}</title>
        </head>
        <body>
          <p>Redirecting to <a href="/${LATEST}/">${LATEST}</a>...</p>
          <script>window.location.replace("/${LATEST}/");</script>
        </body>
        </html>
        REDIRECT_EOF

        echo "output_dir=${OUTPUT_DIR}" >> "$GITHUB_OUTPUT"
        echo "Site build complete: ${#TAGS[@]} versions in ${OUTPUT_DIR}"
```

**Step 2: Verify the action file is valid YAML**

Run: `yq eval '.' .github/actions/build-versioned-site/action.yml > /dev/null`
Expected: no output (valid YAML)

**Step 3: Commit**

```bash
git add .github/actions/build-versioned-site/action.yml
git commit -S -m "feat(site): add build-versioned-site composite action

Builds Hugo site once per retained version tag into versioned
subdirectories with a root redirect to latest."
```

---

### Task 2: Update hugo.yaml to remove hardcoded versions

The hardcoded `versions` list will be replaced at build time by the composite
action. Remove it from the static config so it doesn't confuse local development.

**Files:**
- Modify: `site/hugo.yaml:77-81`

**Step 1: Replace the hardcoded versions block with a placeholder comment**

In `site/hugo.yaml`, replace lines 77-81:

```yaml
  version_menu: "Versions"
  version_menu_pagelinks: true
  versions:
    - version: "Latest (main)"
      url: "https://aicr.dgxc.io"
```

With:

```yaml
  version_menu: "Versions"
  version_menu_pagelinks: true
  # versions: populated at build time by build-versioned-site action
```

**Step 2: Verify Hugo can still build locally**

Run: `cd site && hugo --minify 2>&1 | tail -5`
Expected: Build succeeds (Docsy handles missing `versions` gracefully — dropdown just won't appear)

**Step 3: Commit**

```bash
git add site/hugo.yaml
git commit -S -m "feat(site): remove hardcoded version list from hugo.yaml

The versions list is now generated at build time by the
build-versioned-site composite action."
```

---

### Task 3: Refactor gh-pages.yaml to support versioned builds

Convert `gh-pages.yaml` to:
- Use `build-versioned-site` action for main-branch pushes
- Keep single unversioned build for PR previews
- Add `workflow_call` trigger so `on-tag.yaml` can invoke it

**Files:**
- Modify: `.github/workflows/gh-pages.yaml`

**Step 1: Rewrite gh-pages.yaml**

Replace the entire file with:

```yaml
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

name: GitHub Pages

on:
  push:
    branches:
      - main
    paths:
      - 'site/**'
      - '.github/workflows/gh-pages.yaml'
      - '.github/actions/build-versioned-site/**'
  pull_request:
    branches:
      - main
    paths:
      - 'site/**'
      - '.github/workflows/gh-pages.yaml'
      - '.github/actions/build-versioned-site/**'
  workflow_call: {}
  workflow_dispatch: {}

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.ref }}
  cancel-in-progress: true

jobs:

  build:
    name: Build
    runs-on: ubuntu-latest
    timeout-minutes: 15
    permissions:
      contents: read
    steps:

      - name: Checkout Code
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
        with:
          persist-credentials: false
          fetch-depth: 0

      - name: Load versions
        id: versions
        uses: ./.github/actions/load-versions

      - name: Setup Go
        uses: actions/setup-go@7a3fe6cf4cb3a834922a1244abfce67bcef6a0c5  # v6.2.0
        with:
          go-version: ${{ steps.versions.outputs.go }}
          cache: false

      - name: Setup Hugo
        run: |
          HUGO_VERSION="${{ steps.versions.outputs.hugo }}"
          wget -q "https://github.com/gohugoio/hugo/releases/download/v${HUGO_VERSION}/hugo_extended_${HUGO_VERSION}_linux-amd64.deb" -O hugo.deb
          sudo dpkg -i hugo.deb
          rm hugo.deb
          hugo version

      - name: Setup Node
        uses: actions/setup-node@49933ea5288caeca8642d1e84afbd3f7d6820020  # v4.4.0
        with:
          node-version: ${{ steps.versions.outputs.node }}

      # PR preview: single unversioned build (fast)
      - name: Build site (PR preview)
        if: github.event_name == 'pull_request'
        working-directory: site
        run: |
          npm ci
          hugo --minify

      - name: Upload PR preview artifact
        if: github.event_name == 'pull_request'
        uses: actions/upload-pages-artifact@56afc609e74202658d3ffba0e8f6dda462b719fa  # v3.0.1
        with:
          path: site/public

      # Main/release: versioned multi-build
      - name: Build versioned site
        if: github.event_name != 'pull_request'
        uses: ./.github/actions/build-versioned-site
        with:
          hugo_version: ${{ steps.versions.outputs.hugo }}
          node_version: ${{ steps.versions.outputs.node }}
          go_version: ${{ steps.versions.outputs.go }}

      - name: Upload versioned artifact
        if: github.event_name != 'pull_request'
        uses: actions/upload-pages-artifact@56afc609e74202658d3ffba0e8f6dda462b719fa  # v3.0.1
        with:
          path: versioned-site-output

  deploy:
    name: Deploy
    if: github.event_name != 'pull_request'
    needs: build
    runs-on: ubuntu-latest
    timeout-minutes: 10
    permissions:
      pages: write
      id-token: write
    environment:
      name: github-pages
      url: ${{ steps.deployment.outputs.page_url }}
    steps:

      - name: Deploy to GitHub Pages
        id: deployment
        uses: actions/deploy-pages@d6db90164ac5ed86f2b6aed7e0febac5b3c0c03e  # v4.0.5
```

**Step 2: Validate YAML**

Run: `yq eval '.' .github/workflows/gh-pages.yaml > /dev/null`
Expected: no output (valid)

**Step 3: Commit**

```bash
git add .github/workflows/gh-pages.yaml
git commit -S -m "feat(site): refactor gh-pages workflow for versioned builds

PR previews remain single unversioned builds. Main and release
triggers use the build-versioned-site action to produce
subdirectory-per-version output."
```

---

### Task 4: Wire site deployment into the release workflow

Add a `site` job to `on-tag.yaml` that runs after `publish` and before `summary`.

**Files:**
- Modify: `.github/workflows/on-tag.yaml:358-404`

**Step 1: Add the site job after the publish job**

After line 357 (end of `publish` job) in `on-tag.yaml`, insert:

```yaml

  # =============================================================================
  # Site: Deploy versioned documentation to GitHub Pages
  # =============================================================================

  site:
    name: Deploy Site
    needs: [publish]
    uses: ./.github/workflows/gh-pages.yaml
    permissions:
      contents: read
      pages: write
      id-token: write
```

**Step 2: Add `site` to the summary job's needs and env**

In the `summary` job at line 366, update the `needs` list:

Change:
```yaml
    needs: [tests, build-ko, build-docker, docker-manifest, image-vuln-scan, attest, release-check, deploy, publish]
```
To:
```yaml
    needs: [tests, build-ko, build-docker, docker-manifest, image-vuln-scan, attest, release-check, deploy, publish, site]
```

Add `SITE` to the env block (after `PUBLISH`):
```yaml
          SITE: ${{ needs.site.result }}
```

Add the site row to the summary table (after the Publish row):
```yaml
            echo "| Site | ${SITE} |"
```

**Step 3: Validate YAML**

Run: `yq eval '.' .github/workflows/on-tag.yaml > /dev/null`
Expected: no output (valid)

**Step 4: Commit**

```bash
git add .github/workflows/on-tag.yaml
git commit -S -m "feat(release): deploy versioned site after release publish

Adds a site job to the release pipeline that calls gh-pages.yaml
as a reusable workflow after the release is published."
```

---

### Task 5: Test the build locally

Verify the composite action's core logic works by simulating it locally.

**Step 1: Fetch tags for local testing**

Run: `git fetch origin --tags`
Expected: Tags v0.8.2 through v0.8.11 are fetched

**Step 2: Test tag discovery**

Run: `git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sort -V -r | head -3`
Expected: `v0.8.11`, `v0.8.10`, `v0.8.9` (one per line, descending)

**Step 3: Test git archive for a tag's site directory**

Run:
```bash
mkdir -p /tmp/site-test && git archive v0.8.11 -- site/ | tar -x -C /tmp/site-test && ls /tmp/site-test/site/hugo.yaml
```
Expected: File exists

**Step 4: Test yq patching**

Run:
```bash
cp /tmp/site-test/site/hugo.yaml /tmp/hugo-test.yaml
yq eval -i '.baseURL = "https://aicr.dgxc.io/v0.8.11/"' /tmp/hugo-test.yaml
yq eval '.baseURL' /tmp/hugo-test.yaml
```
Expected: `https://aicr.dgxc.io/v0.8.11/`

**Step 5: Test Hugo build with patched config**

Run:
```bash
cd /tmp/site-test/site && npm ci && hugo --minify --baseURL https://aicr.dgxc.io/v0.8.11/ 2>&1 | tail -3
```
Expected: Hugo build succeeds, shows page count and build time

**Step 6: Clean up**

Run: `rm -rf /tmp/site-test /tmp/hugo-test.yaml`

**Step 7: Commit all changes (if any local fixups were needed)**

```bash
git add -A && git diff --cached --quiet || git commit -S -m "fix(site): address issues found during local testing"
```

---

### Task 6: Final review and push

**Step 1: Run make lint to check for YAML issues**

Run: `make lint`
Expected: All checks pass (yamllint covers workflow files)

**Step 2: Review all changes**

Run: `git log --oneline origin/main..HEAD`
Expected: 3-4 commits covering the composite action, hugo.yaml, gh-pages.yaml, and on-tag.yaml

**Step 3: Push and open PR (or push to main per project convention)**

Defer to user preference on whether to push directly or open a PR.
