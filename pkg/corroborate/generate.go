// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package corroborate

import (
	"context"
	_ "embed"
	"encoding/json"
	stderrors "errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator"
)

// errSkipRun is a loadRun sentinel: the run is malformed in a way that warrants
// dropping it (already logged) rather than aborting the whole dashboard —
// mirroring aggregate's per-run skips. It is a plain error (not a coded
// *StructuredError) so stderrors.Is matches it by identity.
var errSkipRun = stderrors.New("corroborate: skip run")

// recipeTab pairs a recipe's coordinate with its built grid Tab, for assembling
// the sorted catalog tree.
type recipeTab struct {
	coord recipe.Coordinate
	tab   Tab
}

// rendererHTML is the self-contained static dashboard renderer. It embeds no
// data: it fetches data/index.json on boot and lazy-loads data/series/*.json on
// a drilldown. Embedding it as a build-time asset makes the emitted HTML
// byte-identical across runs.
//
//go:embed renderer/index.html
var rendererHTML []byte

// platformNone is the synthetic platform facet value for recipes with no
// platform; it must match the renderer's PLATFORM_NONE constant.
const platformNone = "(none)"

// Facet axis keys, emitted in index.json's criteria map and each tab's coord.
const (
	axisService     = "service"
	axisAccelerator = "accelerator"
	axisOS          = "os"
	axisIntent      = "intent"
	axisPlatform    = "platform"
)

// criteriaAxes is the fixed facet-axis order emitted in index.json.
var criteriaAxes = []string{axisService, axisAccelerator, axisOS, axisIntent, axisPlatform}

// Options configures Generate.
type Options struct {
	// InputDir is the root of the source-keyed GCS layout (Contract 3),
	// synced to a local directory. Generate finds every meta.json beneath it.
	InputDir string

	// OutputDir is where index.html and data/{index.json,series/*.json} are
	// written.
	OutputDir string

	// AllowlistPath, when set, re-derives each signer's class from its verified
	// (issuer, identity) against the allowlist instead of trusting meta.json's
	// pre-derived class. When empty, the class/allowlisted fields in meta.json
	// are trusted as-is — safe only because GP2 (the trusted ingest job, Contract
	// 3) writes them post-verification; point -allowlist at the in-tree allowlist
	// to re-verify when the input tree is not from a trusted ingest.
	AllowlistPath string
}

// GenerateResult summarizes a Generate run for logging.
type GenerateResult struct {
	Recipes int
	Sources int
	Runs    int
}

// signerRun is one signer's single run for one recipe.
type signerRun struct {
	meta        RunMeta
	attestedAt  time.Time
	class       Class
	allowlisted bool
	label       string
	// statuses maps phase -> CTRF test name -> raw CTRF status.
	statuses map[string]map[string]string
}

// flatten collapses a run's per-phase statuses to a single name -> Result map
// in PhaseOrder, for the per-name series view.
func (r *signerRun) flatten() map[string]Result {
	out := make(map[string]Result)
	for _, ph := range phaseNames {
		for name, status := range r.statuses[ph] {
			out[name] = BucketStatus(status)
		}
	}
	return out
}

// recipeAgg accumulates every run for one recipe coordinate.
type recipeAgg struct {
	coord    recipe.Coordinate
	criteria recipe.Criteria
	name     string
	bySigner map[string][]*signerRun
}

// Generate reads the corroboration evidence under opts.InputDir, computes the
// consensus model, and writes the deterministic dashboard (index.json,
// series/<recipe>.json, index.html) under opts.OutputDir.
//
// The directory walk, per-run reads, and output writes are unbounded in the size
// of the evidence tree, so they observe ctx: a canceled or deadline-exceeded ctx
// stops the walk, the per-run collect loop, and the series-emit loop and returns
// ErrCodeTimeout. Pure in-memory aggregation/build between those phases is not a
// cancellation point.
func Generate(ctx context.Context, opts Options) (GenerateResult, error) {
	var allowlist *Allowlist
	if opts.AllowlistPath != "" {
		al, err := LoadAllowlist(opts.AllowlistPath)
		if err != nil {
			return GenerateResult{}, err
		}
		allowlist = al
	}

	runs, err := collectRuns(ctx, opts.InputDir, allowlist)
	if err != nil {
		return GenerateResult{}, err
	}

	recipes := aggregate(runs)

	index, seriesByRecipe, summary := build(recipes)

	if err := emit(ctx, opts.OutputDir, index, seriesByRecipe); err != nil {
		return GenerateResult{}, err
	}
	return summary, nil
}

// canceledErr wraps a context error as a coded ErrCodeTimeout, or returns nil if
// ctx is still live. Used at the loop/walk cancellation points in Generate.
func canceledErr(ctx context.Context, what string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeTimeout, what+" canceled", err)
	}
	return nil
}

// phaseNames is validator.PhaseOrder rendered as strings (the ctrf/<phase>.json
// basenames and the row phase order), computed once.
var phaseNames = func() []string {
	out := make([]string, len(validator.PhaseOrder))
	for i, p := range validator.PhaseOrder {
		out[i] = string(p)
	}
	return out
}()

// collectRuns walks inputDir for every meta.json and parses each run with its
// sibling ctrf/<phase>.json reports.
func collectRuns(ctx context.Context, inputDir string, allowlist *Allowlist) ([]*signerRun, error) {
	info, statErr := os.Stat(inputDir)
	switch {
	case os.IsNotExist(statErr):
		return nil, errors.New(errors.ErrCodeInvalidRequest, "input dir not found: "+inputDir)
	case statErr != nil:
		// A permission or filesystem error is not "not found" — surface it as an
		// operational failure with its original cause rather than masking it.
		return nil, errors.Wrap(errors.ErrCodeInternal, "stat input dir "+inputDir, statErr)
	case !info.IsDir():
		return nil, errors.New(errors.ErrCodeInvalidRequest, "input dir is not a directory: "+inputDir)
	}

	var metaPaths []string
	walkErr := filepath.WalkDir(inputDir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if err := ctx.Err(); err != nil {
			return err // honor cancellation mid-walk; classified below
		}
		if !d.IsDir() && d.Name() == "meta.json" {
			metaPaths = append(metaPaths, path)
		}
		return nil
	})
	if walkErr != nil {
		if stderrors.Is(walkErr, context.Canceled) || stderrors.Is(walkErr, context.DeadlineExceeded) {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "walk input dir canceled", walkErr)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "walk input dir", walkErr)
	}
	// Deterministic processing order.
	sort.Strings(metaPaths)

	runs := make([]*signerRun, 0, len(metaPaths))
	for _, mp := range metaPaths {
		if err := canceledErr(ctx, "collect runs"); err != nil {
			return nil, err
		}
		run, err := loadRun(mp, allowlist)
		if err != nil {
			if stderrors.Is(err, errSkipRun) {
				continue // run was skipped (already logged); see loadRun
			}
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// compatibleMetaSchema reports whether a meta.json schemaVersion is safe to parse
// under the current rules. An empty version is tolerated (GP2 may omit it); a
// non-empty version is compatible only when it shares RunMetaSchemaVersion's
// major (so a backward-compatible future minor still loads, but a new major does
// not). See the gate in loadRun for the fail-closed rationale.
func compatibleMetaSchema(got string) bool {
	if got == "" {
		return true
	}
	return schemaMajor(got) == schemaMajor(RunMetaSchemaVersion)
}

// schemaMajor returns the "<family>/v<major>" portion of a meta schemaVersion,
// stripping any ".<minor>[...]" suffix. A value with no minor suffix is returned
// unchanged.
func schemaMajor(v string) string {
	if i := strings.IndexByte(v, '.'); i >= 0 {
		return v[:i]
	}
	return v
}

// loadRun parses one run directory (the dir containing metaPath).
func loadRun(metaPath string, allowlist *Allowlist) (*signerRun, error) {
	meta, err := readRunMeta(metaPath)
	if err != nil {
		return nil, err
	}
	// Schema gate, fail-closed on an incompatible major — symmetric with
	// LoadAllowlist (the shared pkg/evidence/allowlist loader), which rejects an
	// unsupported allowlist schemaVersion because a future schema may change the
	// trust semantics. A
	// meta/v2 that repurposes a field (e.g. signer/allowlisted) must not be parsed
	// under v1 assumptions, so a different major is skipped (errSkipRun) rather
	// than aborting the whole dashboard. An empty version is tolerated by design
	// (GP2 may omit it); a same-major future minor still loads, with a warning.
	if !compatibleMetaSchema(meta.SchemaVersion) {
		slog.Warn("skipping run: incompatible meta.json schema major version",
			"path", metaPath, "got", meta.SchemaVersion, "want", RunMetaSchemaVersion,
			"signer", meta.Signer.Identity)
		return nil, errSkipRun
	}
	if meta.SchemaVersion != "" && meta.SchemaVersion != RunMetaSchemaVersion {
		slog.Warn("meta.json schema version differs within the supported major; parsing under current rules",
			"path", metaPath, "got", meta.SchemaVersion, "want", RunMetaSchemaVersion)
	}

	class := Class(meta.Signer.Class)
	allowlisted := meta.Signer.Allowlisted
	if allowlist != nil {
		derived, ok := classifySigner(allowlist, meta.Signer.Issuer, meta.Signer.Identity)
		if (derived != class || ok != allowlisted) && meta.Signer.Class != "" {
			slog.Warn("meta.json signer class disagrees with allowlist; using allowlist",
				"signer", meta.Signer.Identity, "metaClass", meta.Signer.Class,
				"derivedClass", derived, "metaAllowlisted", allowlisted, "derivedAllowlisted", ok)
		}
		class, allowlisted = derived, ok
	}

	statuses := make(map[string]map[string]string)
	ctrfDir := filepath.Join(filepath.Dir(metaPath), "ctrf")
	for _, phase := range phaseNames {
		phasePath := filepath.Join(ctrfDir, phase+".json")
		if _, statErr := os.Stat(phasePath); statErr != nil {
			if os.IsNotExist(statErr) {
				continue // a phase a run did not produce is simply absent
			}
			// A permission/I/O error is not "absent" — surface it rather than
			// silently dropping a phase that may actually exist.
			return nil, errors.Wrap(errors.ErrCodeInternal, "stat "+phasePath, statErr)
		}
		report, err := readCTRF(phasePath)
		if err != nil {
			return nil, err
		}
		byName := make(map[string]string, len(report.Results.Tests))
		for _, t := range report.Results.Tests {
			byName[t.Name] = t.Status
		}
		statuses[phase] = byName
	}

	// A run we cannot temporally order must not contribute: silently treating a
	// bad timestamp as the zero time would sort it oldest and let a stale run
	// masquerade as current (or hide a genuinely newer result). Skip it (loud),
	// consistent with aggregate's "one bad run never aborts the dashboard".
	at, parseErr := time.Parse(time.RFC3339, meta.AttestedAt)
	if parseErr != nil {
		slog.Warn("skipping run: unparseable attestedAt",
			"path", metaPath, "attestedAt", meta.AttestedAt, "signer", meta.Signer.Identity)
		return nil, errSkipRun
	}

	return &signerRun{
		meta:        *meta,
		attestedAt:  at,
		class:       class,
		allowlisted: allowlisted,
		label:       labelFor(*meta, class),
		statuses:    statuses,
	}, nil
}

// aggregate groups runs into per-recipe aggregates keyed by coordinate path.
//
// It reads a multi-contributor evidence tree, so one mislabeled or hostile run
// must not abort the whole dashboard. A run is skipped (with a warning) when its
// recipe slug is unsafe as a filename (path traversal), its coordinate does not
// invert/round-trip, or its recipe name collides with an already-claimed
// coordinate (which would otherwise overwrite a sibling's series file). Runs are
// pre-sorted by path, so the surviving coordinate for a colliding name is stable.
func aggregate(runs []*signerRun) map[string]*recipeAgg {
	recipes := make(map[string]*recipeAgg)
	nameToCoord := make(map[string]string) // recipe slug -> first coordinate that claimed it
	for _, run := range runs {
		co := recipe.Coordinate{
			Group:     run.meta.Coordinate.Group,
			Dashboard: run.meta.Coordinate.Dashboard,
			Tab:       run.meta.Coordinate.Tab,
		}
		key := co.Path()
		signerID := canonicalSourceID(run.meta.Signer)

		if agg := recipes[key]; agg != nil {
			agg.bySigner[signerID] = append(agg.bySigner[signerID], run)
			continue
		}

		name := run.meta.Recipe
		if !safeRecipeSlug(name) {
			slog.Warn("skipping run: unsafe recipe slug (not a flat, local filename)",
				"recipe", name, "coordinate", key, "signer", run.meta.Signer.Identity)
			continue
		}
		crit, err := criteriaFromCoordinate(co)
		if err != nil {
			slog.Warn("skipping run: coordinate does not invert to valid criteria",
				"coordinate", key, "signer", run.meta.Signer.Identity, "error", err)
			continue
		}
		if claimed, dup := nameToCoord[name]; dup && claimed != key {
			slog.Warn("skipping run: recipe name already claimed by another coordinate",
				"recipe", name, "coordinate", key, "claimedBy", claimed, "signer", run.meta.Signer.Identity)
			continue
		}

		nameToCoord[name] = key
		agg := &recipeAgg{
			coord:    co,
			criteria: crit,
			name:     name,
			bySigner: make(map[string][]*signerRun),
		}
		agg.bySigner[signerID] = append(agg.bySigner[signerID], run)
		recipes[key] = agg
	}
	return recipes
}

// safeRecipeSlug reports whether name is safe to use as a flat series-file
// basename: non-empty, local (no "..", not absolute), and free of path
// separators. The recipe slug comes from a contributor-controlled meta.json, so
// it must be validated before it reaches filepath.Join (CLAUDE.md path-traversal
// rule).
func safeRecipeSlug(name string) bool {
	return name != "" && name != "." && filepath.IsLocal(name) && !strings.ContainsAny(name, `/\`)
}

// build computes the consensus model for every recipe and assembles the Index
// plus per-recipe Series.
func build(recipes map[string]*recipeAgg) (Index, map[string]Series, GenerateResult) {
	sources := make(map[string]Source)
	seriesByRecipe := make(map[string]Series)
	present := map[string]map[string]struct{}{}
	for _, ax := range criteriaAxes {
		present[ax] = map[string]struct{}{}
	}

	// Order recipes by coordinate path for deterministic grouping.
	keys := make([]string, 0, len(recipes))
	for k := range recipes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	builts := make([]recipeTab, 0, len(keys))
	totalRuns := 0

	for _, k := range keys {
		agg := recipes[k]
		tab, series, runCount := buildRecipe(agg, sources)
		totalRuns += runCount
		seriesByRecipe[agg.name] = series
		builts = append(builts, recipeTab{coord: agg.coord, tab: tab})

		recordPresent(present, agg.criteria)
	}

	groups := assembleGroups(builts)

	// GeneratedAt is the newest run AttestedAt across all recipes — derived from
	// evidence (order-independent max), never the wall clock, so the emitted
	// index.json stays byte-reproducible.
	var newest time.Time
	for _, agg := range recipes {
		for _, rs := range agg.bySigner {
			for _, r := range rs {
				if r.attestedAt.After(newest) {
					newest = r.attestedAt
				}
			}
		}
	}
	generatedAt := ""
	if !newest.IsZero() {
		generatedAt = newest.UTC().Format("2006-01-02 15:04 UTC")
	}

	index := Index{
		Schema: SchemaVersion,
		Meta: Meta{
			Links:       Links{Install: LinkInstall, Docs: LinkDocs, GitHub: LinkGitHub},
			Counts:      Counts{Recipes: len(builts), CSPs: len(groups), Sources: len(sources)},
			GeneratedAt: generatedAt,
		},
		Criteria: criteriaValues(present),
		Sources:  sources,
		Groups:   groups,
	}
	return index, seriesByRecipe, GenerateResult{Recipes: len(builts), Sources: len(sources), Runs: totalRuns}
}

// buildRecipe computes one recipe's grid (Tab) and time-series (Series), and
// registers its grid signers into the shared sources map.
func buildRecipe(agg *recipeAgg, sources map[string]Source) (Tab, Series, int) {
	// Pre-reduce by the VERIFIED (issuer, identity): one verified signer that
	// submitted runs under two IDHashes must render as ONE source with one latest
	// result, not two. (ComputeConsensus already de-dups the count via
	// signerIdentityKey; this aligns the grid/series display with that count.)
	// Each verified identity gets a canonicalSourceID — derived locally from the
	// verified pair, never the contributor-controlled IDHash — as its public
	// display key (Latest.Src, Sources, Series). Distinct identities cannot
	// collide on a canonical ID, so no signer can be overwritten/dropped.
	runsByID := make(map[string][]*signerRun, len(agg.bySigner)) // canonicalSourceID -> all of that identity's runs
	runCount := 0
	{
		byIdentity := make(map[string][]*signerRun, len(agg.bySigner))
		for _, rs := range agg.bySigner {
			for _, r := range rs {
				k := signerIdentityKey(r.meta.Signer)
				byIdentity[k] = append(byIdentity[k], r)
			}
		}
		for _, rs := range byIdentity {
			sortRunsNewestFirst(rs)
			runsByID[canonicalSourceID(rs[0].meta.Signer)] = rs
			runCount += len(rs)
		}
	}

	signerIDs := make([]string, 0, len(runsByID))
	latestAny := make(map[string]*signerRun, len(runsByID)) // newest run per signer, version-blind
	for id, rs := range runsByID {
		latestAny[id] = rs[0]
		signerIDs = append(signerIDs, id)
	}
	sort.Strings(signerIDs)

	// Every signing identity is cataloged (class/allowlist are per-identity and
	// stable across its runs), independent of which version grids it lands in.
	for _, id := range signerIDs {
		registerSource(sources, latestAny[id])
	}

	// Version-aware consensus: bucket each signer's runs by AICR version and take
	// its newest run AT that version, so corroboration only counts agreement at
	// the SAME version (cross-version agreement is not reproduction). Order
	// versions newest-first by SEMANTIC version so Versions[0] is the newest tool
	// release (the default view) — a re-run of an older release must not jump the
	// queue just because it was attested more recently.
	verLatest := map[string]map[string]*signerRun{}
	verNewest := map[string]time.Time{}
	for id, rs := range runsByID { // rs is sorted newest-first
		seenVer := map[string]struct{}{}
		for _, r := range rs {
			v := r.meta.AICRVersion
			if _, ok := seenVer[v]; !ok {
				seenVer[v] = struct{}{}
				if verLatest[v] == nil {
					verLatest[v] = map[string]*signerRun{}
				}
				verLatest[v][id] = r
			}
			if r.attestedAt.After(verNewest[v]) {
				verNewest[v] = r.attestedAt
			}
		}
	}
	versions := make([]string, 0, len(verLatest))
	for v := range verLatest {
		versions = append(versions, v)
	}
	orderVersionsNewestFirst(versions, verNewest)

	gridSigners := map[string]struct{}{}
	tabVersions := make([]TabVersion, 0, len(versions))
	for _, v := range versions {
		latest := verLatest[v]
		ids := make([]string, 0, len(latest))
		for id := range latest {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		rowKeys := unionRows(latest)
		rows, statesByPhase := computeGrid(rowKeys, ids, latest, gridSigners)
		tabVersions = append(tabVersions, TabVersion{
			AICRVer:     v,
			PhaseRollup: phaseRollup(statesByPhase),
			Tests:       rows,
		})
	}

	tab := Tab{
		Recipe:   agg.name,
		Coord:    coordMap(agg.criteria),
		Versions: tabVersions,
	}
	// The time-series spans every build/version, so its union test set must come
	// from ALL runs (not just each signer's newest), or a test that ran only in an
	// older build would be dropped from the history.
	series := buildSeries(agg, gridSigners, unionRowsAllBuilds(runsByID), runsByID)
	return tab, series, runCount
}

// computeGrid bakes one version's consensus grid: for each (phase, CTRF) row it
// buckets every signer's latest-at-version result, computes the consensus, and
// records the pass/fail signers for the cell. not-run signers feed consensus but
// are omitted from the cell's Signers list (they render as empty cells). Signers
// that contributed a pass/fail are added to gridSigners (the series' column set).
func computeGrid(rowKeys []rowKey, signerIDs []string, latest map[string]*signerRun, gridSigners map[string]struct{}) ([]Row, map[string][]State) {
	rows := make([]Row, 0, len(rowKeys))
	statesByPhase := map[string][]State{}
	for _, rk := range rowKeys {
		signerResults := make([]SignerResult, 0, len(signerIDs))
		signers := make([]Latest, 0, len(signerIDs))
		for _, id := range signerIDs {
			run := latest[id]
			res := ResultNotRun
			if status, ok := run.statuses[rk.phase][rk.name]; ok {
				res = BucketStatus(status)
			}
			// Anti-sybil: the consensus distinct-signer key is the VERIFIED
			// (issuer, identity), not meta.json's contributor-controlled IDHash.
			signerResults = append(signerResults, SignerResult{SignerID: signerIdentityKey(run.meta.Signer), Allowlisted: run.allowlisted, Result: res})
			if res != ResultPass && res != ResultFail {
				continue
			}
			signers = append(signers, Latest{
				Src:         id,
				Result:      string(res),
				AICRVer:     run.meta.AICRVersion,
				K8sVer:      run.meta.K8sVersion,
				When:        formatWhen(run.meta.AttestedAt),
				Build:       run.meta.RunID,
				EvidenceRef: run.meta.EvidenceRef,
			})
			gridSigners[id] = struct{}{}
		}
		c := ComputeConsensus(signerResults)
		statesByPhase[rk.phase] = append(statesByPhase[rk.phase], c.State)
		rows = append(rows, Row{
			Phase:     rk.phase,
			Name:      rk.name,
			Consensus: string(c.State),
			Reported:  c.Reported,
			Signers:   signers,
		})
	}
	return rows, statesByPhase
}

// rowKey identifies a single grid row.
type rowKey struct {
	phase string
	name  string
}

// orderVersionsNewestFirst sorts AICR version strings so the newest tool RELEASE
// leads (Versions[0], the default dashboard view): by semantic version first —
// semver.Compare gets v0.10.0 > v0.9.0 right where a lexical sort does not — then
// by most-recent attestation, then a stable string tiebreak. A malformed / non-
// "v"-prefixed tag is invalid semver and ranks below every real release.
func orderVersionsNewestFirst(versions []string, verNewest map[string]time.Time) {
	sort.Slice(versions, func(i, j int) bool {
		vi, vj := versions[i], versions[j]
		if c := semver.Compare(vi, vj); c != 0 {
			return c > 0
		}
		if !verNewest[vi].Equal(verNewest[vj]) {
			return verNewest[vi].After(verNewest[vj])
		}
		return vi > vj
	})
}

// addRunRows records every (phase, CTRF name) present in a single run.
func addRunRows(seen map[rowKey]struct{}, run *signerRun) {
	for phase, byName := range run.statuses {
		for name := range byName {
			seen[rowKey{phase: phase, name: name}] = struct{}{}
		}
	}
}

// sortedRowKeys returns the row set ordered by PhaseOrder then CTRF name.
func sortedRowKeys(seen map[rowKey]struct{}) []rowKey {
	keys := make([]rowKey, 0, len(seen))
	for rk := range seen {
		keys = append(keys, rk)
	}
	order := orderIndex(phaseNames)
	sort.Slice(keys, func(i, j int) bool {
		pi, pj := order[keys[i].phase], order[keys[j].phase]
		if pi != pj {
			return pi < pj
		}
		return keys[i].name < keys[j].name
	})
	return keys
}

// unionRows collects the row set across a latest-per-signer map (one AICR-version
// consensus grid).
func unionRows(latest map[string]*signerRun) []rowKey {
	seen := map[rowKey]struct{}{}
	for _, run := range latest {
		addRunRows(seen, run)
	}
	return sortedRowKeys(seen)
}

// unionRowsAllBuilds collects the row set across EVERY run (all versions/builds).
// The time-series history must allocate a row for any test that ever ran — using
// only the newest-per-signer set would silently drop tests that exist solely in
// older builds/versions.
func unionRowsAllBuilds(runsByID map[string][]*signerRun) []rowKey {
	seen := map[rowKey]struct{}{}
	for _, rs := range runsByID {
		for _, run := range rs {
			addRunRows(seen, run)
		}
	}
	return sortedRowKeys(seen)
}

// sortRunsNewestFirst orders a signer's runs by AttestedAt descending, breaking
// ties by RunID descending so the ordering is total and deterministic.
func sortRunsNewestFirst(rs []*signerRun) {
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].attestedAt.Equal(rs[j].attestedAt) {
			return rs[i].attestedAt.After(rs[j].attestedAt)
		}
		return rs[i].meta.RunID > rs[j].meta.RunID
	})
}

// registerSource records a signer's display record in the shared sources map
// (first write wins; all of a signer's runs carry the same identity/class).
func registerSource(sources map[string]Source, run *signerRun) {
	id := canonicalSourceID(run.meta.Signer)
	if _, ok := sources[id]; ok {
		return
	}
	sources[id] = Source{
		Label:       run.label,
		Class:       string(run.class),
		Allowlisted: run.allowlisted,
		SignerID:    run.meta.Signer.Identity,
	}
}

// buildSeries assembles the per-recipe time-series for the grid signers.
// runsByID maps each signer's canonicalSourceID to all of that verified
// identity's runs (newest-first), pre-reduced in buildRecipe so a signer that
// submitted under two IDHashes contributes one column series, not two.
func buildSeries(agg *recipeAgg, gridSigners map[string]struct{}, rowKeys []rowKey, runsByID map[string][]*signerRun) Series {
	names := make([]string, 0, len(rowKeys))
	nameSeen := map[string]struct{}{}
	for _, rk := range rowKeys {
		if _, ok := nameSeen[rk.name]; ok {
			continue
		}
		nameSeen[rk.name] = struct{}{}
		names = append(names, rk.name)
	}

	builds := make(map[string][]SeriesBuild)
	health := make(map[string]SeriesHealth)
	for id := range gridSigners {
		runs := runsByID[id] // already newest-first from buildRecipe
		cols := make([]SeriesBuild, 0, len(runs))
		for i, run := range runs {
			flat := run.flatten()
			results := make(map[string]string, len(names))
			for _, name := range names {
				if res, ok := flat[name]; ok && (res == ResultPass || res == ResultFail) {
					results[name] = string(res)
				} else {
					results[name] = string(ResultNotRun)
				}
			}
			cols = append(cols, SeriesBuild{
				ID:          run.meta.RunID,
				AICRVer:     run.meta.AICRVersion,
				K8sVer:      run.meta.K8sVersion,
				When:        formatWhenDate(run.meta.AttestedAt),
				Newest:      i == 0,
				EvidenceRef: run.meta.EvidenceRef,
				Results:     results,
			})
		}
		builds[id] = cols
		health[id] = computeHealth(cols, names)
	}

	return Series{Recipe: agg.name, Builds: builds, Health: health}
}

// computeHealth derives a signer's run-health summary across its build columns.
func computeHealth(cols []SeriesBuild, names []string) SeriesHealth {
	h := SeriesHealth{Builds: len(cols)}

	// lastPassBuild: newest build (cols are newest-first) in which the signer
	// ran >= 1 test and none failed.
	for _, c := range cols {
		ranAny, failed := false, false
		for _, name := range names {
			switch c.Results[name] {
			case string(ResultPass):
				ranAny = true
			case string(ResultFail):
				ranAny, failed = true, true
			}
		}
		if ranAny && !failed {
			h.LastPassBuild = c.ID
			break
		}
	}

	// flakePct: share of adjacent build pairs (per test, both runs) that flip
	// between pass and fail.
	flips, pairs := 0, 0
	for _, name := range names {
		for i := 0; i+1 < len(cols); i++ {
			a, b := cols[i].Results[name], cols[i+1].Results[name]
			aRun := a == string(ResultPass) || a == string(ResultFail)
			bRun := b == string(ResultPass) || b == string(ResultFail)
			if aRun && bRun {
				pairs++
				if a != b {
					flips++
				}
			}
		}
	}
	if pairs > 0 {
		h.FlakePct = (flips*100 + pairs/2) / pairs // rounded
	}
	return h
}

// assembleGroups folds the built tabs into the sorted CSP-first catalog tree.
func assembleGroups(builts []recipeTab) []Group {
	svcOrder := orderIndex(recipe.GetCriteriaServiceTypes())
	accOrder := orderIndex(recipe.GetCriteriaAcceleratorTypes())
	osOrder := orderIndex(recipe.GetCriteriaOSTypes())

	// service -> dashboard("accel-os") -> tabs
	type dashAgg struct {
		accelerator, os string
		tabs            []Tab
	}
	groupMap := map[string]map[string]*dashAgg{}
	for _, b := range builts {
		svc := b.coord.Group
		dash := b.coord.Dashboard
		if groupMap[svc] == nil {
			groupMap[svc] = map[string]*dashAgg{}
		}
		da := groupMap[svc][dash]
		if da == nil {
			da = &dashAgg{accelerator: b.tab.Coord[axisAccelerator], os: b.tab.Coord[axisOS]}
			groupMap[svc][dash] = da
		}
		da.tabs = append(da.tabs, b.tab)
	}

	svcKeys := sortedByOrderThenAlpha(keysOf(groupMap), svcOrder)
	groups := make([]Group, 0, len(svcKeys))
	for _, svc := range svcKeys {
		dashes := groupMap[svc]
		dashKeys := keysOf(dashes)
		sort.Slice(dashKeys, func(i, j int) bool {
			di, dj := dashes[dashKeys[i]], dashes[dashKeys[j]]
			if ai, aj := idx(accOrder, di.accelerator), idx(accOrder, dj.accelerator); ai != aj {
				return ai < aj
			}
			if oi, oj := idx(osOrder, di.os), idx(osOrder, dj.os); oi != oj {
				return oi < oj
			}
			return dashKeys[i] < dashKeys[j]
		})
		dashboards := make([]Dashboard, 0, len(dashKeys))
		for _, dk := range dashKeys {
			da := dashes[dk]
			sort.Slice(da.tabs, func(i, j int) bool {
				return da.tabs[i].Coord[axisIntent]+"-"+da.tabs[i].Coord[axisPlatform] < da.tabs[j].Coord[axisIntent]+"-"+da.tabs[j].Coord[axisPlatform]
			})
			dashboards = append(dashboards, Dashboard{Accelerator: da.accelerator, OS: da.os, Tabs: da.tabs})
		}
		groups = append(groups, Group{Service: svc, Dashboards: dashboards})
	}
	return groups
}

// phaseRollup rolls each phase's row states up to a single state, in PhaseOrder.
// Every phase is emitted; a phase with no rows rolls up to UNTESTED.
func phaseRollup(statesByPhase map[string][]State) map[string]string {
	out := make(map[string]string, len(validator.PhaseOrder))
	for _, ph := range phaseNames {
		out[ph] = string(RollupPhase(statesByPhase[ph]))
	}
	return out
}

// coordMap renders Criteria as the renderer's coord map; an empty platform is
// emitted as "" (the renderer treats it as the (none) facet).
func coordMap(c recipe.Criteria) map[string]string {
	return map[string]string{
		axisService:     string(c.Service),
		axisAccelerator: string(c.Accelerator),
		axisOS:          string(c.OS),
		axisIntent:      string(c.Intent),
		axisPlatform:    string(c.Platform),
	}
}

// recordPresent tracks which criteria values actually appear, for the facet
// dropdowns.
func recordPresent(present map[string]map[string]struct{}, c recipe.Criteria) {
	present[axisService][string(c.Service)] = struct{}{}
	present[axisAccelerator][string(c.Accelerator)] = struct{}{}
	present[axisOS][string(c.OS)] = struct{}{}
	present[axisIntent][string(c.Intent)] = struct{}{}
	if p := string(c.Platform); p != "" {
		present[axisPlatform][p] = struct{}{}
	} else {
		present[axisPlatform][platformNone] = struct{}{}
	}
}

// criteriaValues orders each axis's present values by the registry's canonical
// order, with any extras appended alphabetically.
func criteriaValues(present map[string]map[string]struct{}) map[string][]string {
	// Copy the platform list before appending the synthetic (none) value so we
	// never mutate the backing array the registry handed back.
	platformVals := append([]string{}, recipe.GetCriteriaPlatformTypes()...)
	platformVals = append(platformVals, platformNone)
	canonical := map[string][]string{
		axisService:     recipe.GetCriteriaServiceTypes(),
		axisAccelerator: recipe.GetCriteriaAcceleratorTypes(),
		axisOS:          recipe.GetCriteriaOSTypes(),
		axisIntent:      recipe.GetCriteriaIntentTypes(),
		axisPlatform:    platformVals,
	}
	out := make(map[string][]string, len(criteriaAxes))
	for _, axis := range criteriaAxes {
		seen := present[axis]
		ordered := make([]string, 0, len(seen))
		used := map[string]struct{}{}
		for _, v := range canonical[axis] {
			if _, ok := seen[v]; ok {
				ordered = append(ordered, v)
				used[v] = struct{}{}
			}
		}
		extras := make([]string, 0)
		for v := range seen {
			if _, ok := used[v]; !ok {
				extras = append(extras, v)
			}
		}
		sort.Strings(extras)
		out[axis] = append(ordered, extras...)
	}
	return out
}

// emit writes index.html and the data tree deterministically.
//
// It stages the entire dashboard in a temporary sibling directory and swaps it
// into place only once every file is written. Writing in place would (a) leave a
// partial or stale dashboard if a write fails or ctx is canceled mid-emit, and
// (b) retain orphaned series/<recipe>.json files from a previous run whose recipe
// set has since changed. The staging dir is a sibling of outputDir so the final
// rename stays within one filesystem.
func emit(ctx context.Context, outputDir string, index Index, seriesByRecipe map[string]Series) error {
	parent := filepath.Dir(outputDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "mkdir output parent", err)
	}
	staging, err := os.MkdirTemp(parent, ".corroborate-emit-*")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "create staging dir", err)
	}
	// Clean the staging tree up on any early return; cleared once the swap lands.
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	// MkdirTemp creates 0o700; the published tree is world-readable by design.
	if err := os.Chmod(staging, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "chmod staging dir", err)
	}

	seriesDir := filepath.Join(staging, "data", "series")
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "mkdir staging tree", err)
	}

	if err := canceledErr(ctx, "emit index"); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(staging, "data", "index.json"), index); err != nil {
		return err
	}
	for recipeName, series := range seriesByRecipe {
		if err := canceledErr(ctx, "emit series"); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(seriesDir, recipeName+".json"), series); err != nil {
			return err
		}
	}
	if err := canceledErr(ctx, "emit renderer"); err != nil {
		return err
	}
	htmlPath := filepath.Join(staging, "index.html")
	if err := os.WriteFile(htmlPath, rendererHTML, 0o644); err != nil { //nolint:gosec // a static renderer asset is world-readable by design
		return errors.Wrap(errors.ErrCodeInternal, "write "+htmlPath, err)
	}

	// Swap the staged tree into place. Rename cannot overwrite a non-empty
	// directory, so remove the previous output first; a failure between the two
	// leaves no dashboard, which is preferable to a half-updated one.
	if err := os.RemoveAll(outputDir); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "remove previous output", err)
	}
	if err := os.Rename(staging, outputDir); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "swap output into place", err)
	}
	committed = true
	return nil
}

// writeJSON marshals v with a stable 2-space indent and a trailing newline.
// encoding/json sorts map keys, and every slice is pre-sorted, so the bytes are
// reproducible across runs.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "marshal "+path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // dashboard data is world-readable by design
		return errors.Wrap(errors.ErrCodeInternal, "write "+path, err)
	}
	return nil
}

// --- small ordering helpers -------------------------------------------------

func orderIndex(values []string) map[string]int {
	out := make(map[string]int, len(values))
	for i, v := range values {
		out[v] = i
	}
	return out
}

// idx returns the order rank of v, or a large sentinel so unknown values sort
// after the canonical ones.
func idx(order map[string]int, v string) int {
	if i, ok := order[v]; ok {
		return i
	}
	return len(order) + 1
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sortedByOrderThenAlpha sorts keys by their canonical order rank, falling back
// to alphabetical for anything outside the canonical set.
func sortedByOrderThenAlpha(keys []string, order map[string]int) []string {
	sort.Slice(keys, func(i, j int) bool {
		if ai, aj := idx(order, keys[i]), idx(order, keys[j]); ai != aj {
			return ai < aj
		}
		return keys[i] < keys[j]
	})
	return keys
}
