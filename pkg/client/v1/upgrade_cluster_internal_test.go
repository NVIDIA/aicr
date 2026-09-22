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

package aicr

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/inventory"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// Component names here are synthetic and match no registry entry, so no
// transition record applies to any of them. ADR-021's testing strategy forbids
// asserting a verdict for a real component; the same reasoning keeps the
// at-risk assertions off the real records' affectedResources, which every pin
// bump may legitimately change.

// clusterRecipe writes a hydrated RecipeResult carrying the given
// component-to-version table, which is all UpgradeCheck reads from the `to`
// side.
func clusterRecipe(t *testing.T, path string, components map[string]string) string {
	t.Helper()
	doc := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\nmetadata:\n  version: test\ncomponentRefs:\n"
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		doc += fmt.Sprintf("  - name: %s\n    type: Helm\n    source: https://charts.invalid/synthetic\n    version: %s\n",
			name, components[name])
	}
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("setup: write %s: %v", path, err)
	}

	return path
}

// fakeCluster substitutes both cluster entry points and records what reached
// them, including the order, which is what the scan's dependence on the match
// results is observable through.
type fakeCluster struct {
	calls    []string
	readOpts []inventory.Options
	scanOpts []inventory.AtRiskOptions

	result     inventory.Result
	readErr    error
	scanResult inventory.AtRiskResult
	scanErr    error

	// onScan runs inside the scan, which is the only place a test can make
	// the caller's context die while the scan is the operation in flight.
	onScan func()
}

func (f *fakeCluster) client(t *testing.T) *Client {
	t.Helper()
	deps := defaultClientDependencies()
	deps.readInventory = func(_ context.Context, opts inventory.Options) (inventory.Result, error) {
		f.calls = append(f.calls, "read")
		f.readOpts = append(f.readOpts, opts)

		return f.result, f.readErr
	}
	deps.scanAtRisk = func(_ context.Context, opts inventory.AtRiskOptions) (inventory.AtRiskResult, error) {
		f.calls = append(f.calls, "scan")
		f.scanOpts = append(f.scanOpts, opts)
		if f.onScan != nil {
			f.onScan()
		}

		return f.scanResult, f.scanErr
	}
	client, err := newClientWithContextAndDependencies(
		context.Background(), deps, WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// Every count is distinct, so a Source adapter that crosses the two readers'
// numbers cannot land on the value the assertion expects.
func distinctSourceInfo() inventory.SourceInfo {
	return inventory.SourceInfo{
		Helm: inventory.HelmInfo{
			Records: 11, Unattributed: 2, Unreadable: 3, Uninstalled: 4, StampedUnmatched: 5,
		},
		Argo: inventory.ArgoInfo{Applications: 7, Unattributed: 8, Unreadable: 9},
	}
}

// rowsFailing is what Summary.Failing is allowed to count. Asserting a literal
// zero instead would pass on a fixture whose rows happen not to fail, and the
// at-risk sections are exactly the ones that must never reach it.
func rowsFailing(report *upgrade.Report) int {
	failing := 0
	for _, c := range report.Components {
		if c.FailsRun {
			failing++
		}
	}

	return failing
}

func TestUpgradeCheckFromClusterReadsTheInstalledInventory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{
		"synthetic-alpha": "1.2.0", // installed at the target, so no row
		"synthetic-beta":  "0.19.0",
		"synthetic-new":   "0.1.0",
	})
	kubeconfig := filepath.Join(dir, "kubeconfig")

	f := &fakeCluster{
		result: inventory.Result{
			Versions: map[string]string{
				"synthetic-alpha": "1.2.0",
				"synthetic-beta":  "0.18.0",
				"synthetic-gone":  "2.0.0",
			},
			Source: distinctSourceInfo(),
		},
	}
	client := f.client(t)

	report, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{
		From:       FromCluster,
		To:         to,
		Deployer:   "HELM", // folded by ParseDeployerType; the read must get the canonical name
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		t.Fatalf("UpgradeCheck: %v", err)
	}

	if len(f.readOpts) != 1 {
		t.Fatalf("inventory read called %d times, want once", len(f.readOpts))
	}
	opts := f.readOpts[0]
	if opts.Deployer != inventory.DeployerHelm {
		t.Errorf("read Deployer = %q, want %q; an unrecognized name matches no release and would "+
			"report a fully deployed cluster as bare", opts.Deployer, inventory.DeployerHelm)
	}
	if opts.Kubeconfig != kubeconfig {
		t.Errorf("read Kubeconfig = %q, want %q", opts.Kubeconfig, kubeconfig)
	}
	if len(opts.Components) == 0 {
		t.Fatal("read got no components, so it could only report that nothing is installed")
	}
	seen := map[string]struct{}{}
	for _, c := range opts.Components {
		if c.Name == "" {
			t.Errorf("read got a component with no name: %#v", c)
		}
		if _, dup := seen[c.Name]; dup {
			t.Errorf("read got component %q twice", c.Name)
		}
		seen[c.Name] = struct{}{}
	}

	got := map[string]upgrade.ChangeKind{}
	for _, c := range report.Components {
		got[c.Component] = c.Change
	}
	want := map[string]upgrade.ChangeKind{
		"synthetic-beta": upgrade.ChangeVersion,
		"synthetic-gone": upgrade.ChangeRemoved,
		"synthetic-new":  upgrade.ChangeAdded,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v; the cluster read must supply the `from` table", got, want)
	}
	for _, c := range report.Components {
		if c.Component == "synthetic-beta" && (c.From != "0.18.0" || c.To != "0.19.0") {
			t.Errorf("synthetic-beta row = %s -> %s, want 0.18.0 -> 0.19.0", c.From, c.To)
		}
	}

	if report.From != FromCluster {
		t.Errorf("report.From = %q, want %q", report.From, FromCluster)
	}
	if report.Source == nil {
		t.Fatal("report.Source is nil; a cluster read with nothing recognized is indistinguishable " +
			"from an empty cluster without it")
	}
	wantSource := upgrade.ReportSource{
		Kubeconfig: kubeconfig,
		Matched:    3,
		Helm: upgrade.ReportSourceHelm{
			Records: 11, Unattributed: 2, Unreadable: 3, Uninstalled: 4, StampedUnmatched: 5,
		},
		Argo: upgrade.ReportSourceArgo{Applications: 7, Unattributed: 8, Unreadable: 9},
	}
	if !reflect.DeepEqual(*report.Source, wantSource) {
		t.Errorf("report.Source = %#v, want %#v", *report.Source, wantSource)
	}
}

// TestReportSourceFromKeepsTheReadersSeparate is the unit-level guard on the
// one adapter with two same-shaped halves. The Helm side counts storage
// records and the Argo side counts Applications; a crossed assignment reads
// fine and reports a cluster nobody has.
func TestReportSourceFromKeepsTheReadersSeparate(t *testing.T) {
	t.Parallel()

	got := reportSourceFrom(distinctSourceInfo(), "/etc/kubeconfig", 6)
	want := &upgrade.ReportSource{
		Kubeconfig: "/etc/kubeconfig",
		Matched:    6,
		Helm: upgrade.ReportSourceHelm{
			Records: 11, Unattributed: 2, Unreadable: 3, Uninstalled: 4, StampedUnmatched: 5,
		},
		Argo: upgrade.ReportSourceArgo{Applications: 7, Unattributed: 8, Unreadable: 9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reportSourceFrom = %#v, want %#v", got, want)
	}
}

// TestUpgradeCheckFromClusterLabelsTheResolvedKubeconfig pins that an omitted
// path is reported as the one the read actually resolves to, rather than as
// the empty string the caller happened to pass.
//
// The Source block exists to tell an operator which cluster produced a table
// they did not expect, so "" where the read went to KUBECONFIG is the one
// answer that cannot help. Not parallel: it sets the variable the resolution
// reads, and no other test in this package consults it.
func TestUpgradeCheckFromClusterLabelsTheResolvedKubeconfig(t *testing.T) {
	dir := t.TempDir()
	// A single path, so ResolveKubeconfigPath returns it rather than deferring
	// to clientcmd's merge, which reports no single file.
	ambient := filepath.Join(dir, "ambient-kubeconfig")
	t.Setenv("KUBECONFIG", ambient)

	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-beta": "0.19.0"})

	f := &fakeCluster{result: inventory.Result{Versions: map[string]string{"synthetic-beta": "0.18.0"}}}
	client := f.client(t)

	report, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{
		From: FromCluster, To: to, Deployer: "helm",
	})
	if err != nil {
		t.Fatalf("UpgradeCheck: %v", err)
	}
	if report.Source == nil {
		t.Fatal("report.Source is nil")
	}
	if report.Source.Kubeconfig != ambient {
		t.Errorf("report.Source.Kubeconfig = %q, want %q; an omitted path must be reported as the "+
			"cluster the read actually went to", report.Source.Kubeconfig, ambient)
	}
	// The read itself still gets what the caller passed: resolution is the
	// report's label, not a rewrite of the request.
	if f.readOpts[0].Kubeconfig != "" {
		t.Errorf("read Kubeconfig = %q, want the caller's empty value passed through",
			f.readOpts[0].Kubeconfig)
	}
}

// TestUpgradeCheckFromClusterRequiresATarget separates the cluster's reason
// from the artifact's. A cluster has nothing to re-resolve from, which is a
// different fact from an artifact that carries no criteria, and the message an
// operator reads must not describe the other one.
func TestUpgradeCheckFromClusterRequiresATarget(t *testing.T) {
	t.Parallel()

	f := &fakeCluster{}
	client := f.client(t)

	_, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{From: FromCluster, Deployer: "helm"})
	if err == nil {
		t.Fatal("UpgradeCheck with no target error = nil, want rejection")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
	if strings.Contains(err.Error(), "carries no criteria") {
		t.Errorf("error = %q, want a cluster-specific message; a cluster has no criteria to "+
			"misdescribe as missing", err)
	}
	if !strings.Contains(err.Error(), "cluster") {
		t.Errorf("error = %q, want it to name the cluster as the reason", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("cluster was contacted (%v) before the request was rejected", f.calls)
	}
}

// TestUpgradeCheckFromClusterRequiresADeployer pins the stricter-than-usual
// requirement: without a deployer no release name maps to a component, so the
// read cannot run at all, whatever the verdicts turn out to be.
func TestUpgradeCheckFromClusterRequiresADeployer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-beta": "0.19.0"})

	f := &fakeCluster{}
	client := f.client(t)

	_, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{From: FromCluster, To: to})
	if err == nil {
		t.Fatal("UpgradeCheck with no deployer error = nil, want rejection")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "deployer") {
		t.Errorf("error = %q, want it to name the deployer", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("cluster was contacted (%v) before the request was rejected", f.calls)
	}
}

// TestUpgradeCheckClusterReadFailureFailsTheRun separates the primary read
// from the advisory scan: the `from` table is the comparison, so its failure
// has nowhere to be reported except an error.
func TestUpgradeCheckClusterReadFailureFailsTheRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-beta": "0.19.0"})

	f := &fakeCluster{readErr: errors.New(errors.ErrCodeUnavailable, "apiserver is unreachable")}
	client := f.client(t)

	if _, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{
		From: FromCluster, To: to, Deployer: "helm",
	}); err == nil {
		t.Fatal("UpgradeCheck error = nil, want the read failure")
	} else if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
		t.Errorf("error = %v, want the read's own ErrCodeUnavailable preserved", err)
	}
}

// TestUpgradeCheckScanSeesOnlyTheCrossedKinds is the ordering guard. The scan
// consumes AffectedKinds of the match results, so a run whose jumps cross no
// record has nothing to look for, while the loaded record set, which a scan
// placed before the match would have to read instead, names plenty.
func TestUpgradeCheckScanSeesOnlyTheCrossedKinds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-beta": "0.19.0"})

	f := &fakeCluster{result: inventory.Result{Versions: map[string]string{"synthetic-beta": "0.18.0"}}}
	client := f.client(t)

	report, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{
		From: FromCluster, To: to, Deployer: "helm",
	})
	if err != nil {
		t.Fatalf("UpgradeCheck: %v", err)
	}
	if want := []string{"read", "scan"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %v, want %v; the scan's kinds come from the match results", f.calls, want)
	}
	if len(f.scanOpts[0].Kinds) != 0 {
		t.Errorf("scan got %d kinds for a jump that crosses no record, want 0: %#v",
			len(f.scanOpts[0].Kinds), f.scanOpts[0].Kinds)
	}
	if !report.AtRisk.Scanned {
		t.Error("AtRisk.Scanned = false after a scan that ran; only a skipped or failed scan says that")
	}
}

// TestUpgradeCheckScanIsImpliedByClusterAndAvailableToArtifacts pins ADR-021
// Decision 5's two axes: the scan needs a cluster regardless of where the
// `from` table came from, and ScanAtRisk's three states say whether the caller
// left that implication alone, asked for the scan, or refused it.
func TestUpgradeCheckScanIsImpliedByClusterAndAvailableToArtifacts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	from := clusterRecipe(t, filepath.Join(dir, "from.yaml"), map[string]string{"synthetic-beta": "0.18.0"})
	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-beta": "0.19.0"})

	tests := []struct {
		name        string
		req         UpgradeCheckRequest
		wantCalls   []string
		wantScanned bool
		wantReason  string
		wantSource  bool
	}{
		{
			name:       "two artifacts, unset",
			req:        UpgradeCheckRequest{From: from, To: to},
			wantCalls:  nil,
			wantReason: upgrade.NotScannedOffline,
		},
		{
			name:        "two artifacts scanning a live cluster",
			req:         UpgradeCheckRequest{From: from, To: to, ScanAtRisk: boolPtr(true)},
			wantCalls:   []string{"scan"},
			wantScanned: true,
		},
		{
			name:       "two artifacts, refused",
			req:        UpgradeCheckRequest{From: from, To: to, ScanAtRisk: boolPtr(false)},
			wantCalls:  nil,
			wantReason: upgrade.NotScannedDeclined,
		},
		{
			name:        "cluster implies the scan",
			req:         UpgradeCheckRequest{From: FromCluster, To: to, Deployer: "helm"},
			wantCalls:   []string{"read", "scan"},
			wantScanned: true,
			wantSource:  true,
		},
		{
			name: "cluster with the scan asked for as well",
			req: UpgradeCheckRequest{
				From: FromCluster, To: to, Deployer: "helm", ScanAtRisk: boolPtr(true),
			},
			wantCalls:   []string{"read", "scan"},
			wantScanned: true,
			wantSource:  true,
		},
		{
			// The case a plain bool cannot express. The implication is stated
			// by the source, so only an explicit refusal can override it, and
			// the section has to say which of the two silences this is.
			name: "cluster with the scan refused",
			req: UpgradeCheckRequest{
				From: FromCluster, To: to, Deployer: "helm", ScanAtRisk: boolPtr(false),
			},
			wantCalls:  []string{"read"},
			wantReason: upgrade.NotScannedDeclined,
			wantSource: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCluster{result: inventory.Result{
				Versions: map[string]string{"synthetic-beta": "0.18.0"},
			}}
			client := f.client(t)

			report, err := client.UpgradeCheck(t.Context(), tt.req)
			if err != nil {
				t.Fatalf("UpgradeCheck: %v", err)
			}
			if !reflect.DeepEqual(f.calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", f.calls, tt.wantCalls)
			}
			if report.AtRisk.Scanned != tt.wantScanned {
				t.Errorf("AtRisk.Scanned = %v, want %v", report.AtRisk.Scanned, tt.wantScanned)
			}
			if report.AtRisk.Reason != tt.wantReason {
				t.Errorf("AtRisk.Reason = %q, want %q", report.AtRisk.Reason, tt.wantReason)
			}
			if (report.Source != nil) != tt.wantSource {
				t.Errorf("report.Source set = %v, want %v", report.Source != nil, tt.wantSource)
			}
		})
	}
}

// boolPtr names ScanAtRisk's set states. A bare literal is not addressable,
// and nil being a third value is the whole reason the field is a pointer.
func boolPtr(v bool) *bool { return &v }

// TestUpgradeCheckScanFailureDoesNotFailTheRun keeps the advisory feature from
// taking down the primary one: an RBAC gap or an unreachable apiserver on the
// scan leaves a comparison that otherwise succeeded intact, and is reported in
// the section that could not be filled.
func TestUpgradeCheckScanFailureDoesNotFailTheRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-beta": "0.19.0"})

	f := &fakeCluster{
		result:  inventory.Result{Versions: map[string]string{"synthetic-beta": "0.18.0"}},
		scanErr: errors.New(errors.ErrCodeUnauthorized, "customresourcedefinitions is forbidden"),
	}
	client := f.client(t)

	report, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{
		From: FromCluster, To: to, Deployer: "helm",
	})
	if err != nil {
		t.Fatalf("UpgradeCheck failed on an advisory scan error: %v", err)
	}
	if report.AtRisk.Scanned {
		t.Error("AtRisk.Scanned = true after a scan that failed; that publishes an all-clear nothing earned")
	}
	if report.AtRisk.Reason == "" || report.AtRisk.Reason == upgrade.NotScannedOffline {
		t.Errorf("AtRisk.Reason = %q, want the failure named rather than the offline default",
			report.AtRisk.Reason)
	}
	if !strings.Contains(report.AtRisk.Reason, "forbidden") {
		t.Errorf("AtRisk.Reason = %q, want the underlying cause in it", report.AtRisk.Reason)
	}
	if want := rowsFailing(report); report.Summary.Failing != want {
		t.Errorf("Summary.Failing = %d, want %d (the failing rows); a failed advisory scan must not "+
			"move the exit code", report.Summary.Failing, want)
	}
	// The comparison itself still has to be there.
	if len(report.Components) != 1 {
		t.Errorf("rows = %d, want the comparison to have completed", len(report.Components))
	}
}

// TestUpgradeCheckScanFailureRendersAsUnscanned closes the loop the struct
// assertions above leave open: an operator reads the table, not the fields.
//
// Scanned and the sentence it selects are what must not drift. The renderer
// prints Reason only on the unscanned branch and prints "nothing was examined"
// on the scanned-with-no-kinds one, so a failed scan flagged as scanned would
// reach the operator as an all-clear with the cause dropped entirely.
func TestUpgradeCheckScanFailureRendersAsUnscanned(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-beta": "0.19.0"})

	f := &fakeCluster{
		result:  inventory.Result{Versions: map[string]string{"synthetic-beta": "0.18.0"}},
		scanErr: errors.New(errors.ErrCodeUnauthorized, "customresourcedefinitions is forbidden"),
	}
	client := f.client(t)

	report, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{
		From: FromCluster, To: to, Deployer: "helm",
	})
	if err != nil {
		t.Fatalf("UpgradeCheck: %v", err)
	}

	var buf strings.Builder
	if err := WriteUpgradeReportTable(&buf, report); err != nil {
		t.Fatalf("WriteUpgradeReportTable: %v", err)
	}
	// The reason is wrapped to the section's width, so line breaks fall
	// wherever the text runs out; collapse them before matching.
	rendered := strings.Join(strings.Fields(buf.String()), " ")

	if !strings.Contains(rendered, "not scanned: the scan failed") {
		t.Errorf("rendered table does not say the scan failed:\n%s", buf.String())
	}
	if !strings.Contains(rendered, "customresourcedefinitions is forbidden") {
		t.Errorf("rendered table drops the cause, leaving nothing to act on:\n%s", buf.String())
	}
	// The scanned-with-no-kinds sentence. Reached only when Scanned is true,
	// and an all-clear over a scan that never answered.
	if strings.Contains(rendered, "so nothing was examined") {
		t.Errorf("a failed scan rendered as the nothing-to-examine all-clear:\n%s", buf.String())
	}
}

// TestUpgradeCheckAtRiskFindingsNeverFailTheRun pins ADR-021 Decision 3: AICR
// blocking an upgrade over resources it does not own is a claim it has not
// earned.
func TestUpgradeCheckAtRiskFindingsNeverFailTheRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	to := clusterRecipe(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-beta": "0.19.0"})

	f := &fakeCluster{
		result: inventory.Result{Versions: map[string]string{"synthetic-beta": "0.18.0"}},
		scanResult: inventory.AtRiskResult{
			Kinds: []inventory.ScannedKind{
				{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
					Present: true, Examined: 4},
				{Group: "", Kind: "ConfigMap", Components: []string{"grove", "nvsentinel"}},
			},
			Findings: []inventory.AtRiskObject{
				{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
					Namespace: "team-a", Name: "topo-1"},
			},
		},
	}
	client := f.client(t)

	report, err := client.UpgradeCheck(t.Context(), UpgradeCheckRequest{
		From: FromCluster, To: to, Deployer: "helm",
	})
	if err != nil {
		t.Fatalf("UpgradeCheck: %v", err)
	}
	if want := rowsFailing(report); report.Summary.Failing != want {
		t.Errorf("Summary.Failing = %d, want %d (the failing rows); at-risk findings are advisory",
			report.Summary.Failing, want)
	}
	want := upgrade.AtRiskReport{
		Scanned: true,
		Kinds: []upgrade.AtRiskKind{
			{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
				Present: true, Examined: 4},
			{Kind: "ConfigMap", Components: []string{"grove", "nvsentinel"}},
		},
		Findings: []upgrade.AtRiskFinding{
			{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove"},
				Namespace: "team-a", Name: "topo-1"},
		},
	}
	if !reflect.DeepEqual(report.AtRisk, want) {
		t.Errorf("AtRisk = %#v, want %#v", report.AtRisk, want)
	}
}

// TestAtRiskKindsCarryTheirComponents pins the half of the kind adapter that a
// field-for-field copy makes easy to drop. Naming whose upgrade endangers an
// object is what makes the warning actionable; without it the finding says
// only that something might delete something.
func TestAtRiskKindsCarryTheirComponents(t *testing.T) {
	t.Parallel()

	got := atRiskKinds([]upgrade.ResourceKind{
		{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove", "nvsentinel"}},
		{Kind: "ConfigMap"},
	})
	want := []inventory.ResourceKind{
		{Group: "grove.io", Kind: "ClusterTopology", Components: []string{"grove", "nvsentinel"}},
		{Kind: "ConfigMap"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("atRiskKinds = %#v, want %#v", got, want)
	}
	if got := atRiskKinds(nil); got != nil {
		t.Errorf("atRiskKinds(nil) = %#v, want nil", got)
	}
}

// TestAtRiskReportFromScannedIsNotInferred pins that an empty result from a
// scan that ran says so, rather than falling back to the offline default a
// report is built with.
func TestAtRiskReportFromScannedIsNotInferred(t *testing.T) {
	t.Parallel()

	got := atRiskReportFrom(inventory.AtRiskResult{})
	if got == nil {
		t.Fatal("atRiskReportFrom returned nil for a scan that ran and found nothing")
	}
	if !got.Scanned || got.Reason != "" {
		t.Errorf("atRiskReportFrom(empty) = %#v, want Scanned with no reason", got)
	}
}

// abortingContext reports the caller's context as dead on cue, which no real
// context can be made to do at the moment the scan returns: cancellation would
// have to race the recipe loads that run first, and a deadline short enough to
// expire during the scan would expire during them instead.
type abortingContext struct {
	context.Context

	mu   sync.Mutex
	err  error
	done chan struct{}
}

func newAbortingContext(parent context.Context) *abortingContext {
	return &abortingContext{Context: parent, done: make(chan struct{})}
}

func (c *abortingContext) Done() <-chan struct{} { return c.done }

func (c *abortingContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}

	return c.Context.Err()
}

// abort makes the context report err, as context.WithCancel and
// context.WithTimeout report context.Canceled and context.DeadlineExceeded.
func (c *abortingContext) abort(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

// TestUpgradeCheckScanAbortIsNotAFinding separates an abort from a finding.
// ADR-021 Decision 3 keeps what the scan reports advisory; a caller who
// stopped the run reported nothing, and answering them with a report and a
// nil error says the comparison stands.
//
// The two rows that must not fail the run are the discriminating ones. A scan
// that exhausted its own defaults.AtRiskScanTimeout carries exactly the code
// and the sentinel an expired caller deadline carries, because pkg/inventory
// codes both through the same abortError, so neither distinguishes them; the
// caller's context does.
func TestUpgradeCheckScanAbortIsNotAFinding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// abort is what the caller's context reports by the time the scan
		// returns; nil leaves it alive.
		abort   error
		scanErr error
		// wantCode empty means the run has to survive, with the failure in
		// the section it could not fill.
		wantCode      errors.ErrorCode
		wantTransient bool
	}{
		{
			name:     "operator cancels during the scan",
			abort:    context.Canceled,
			scanErr:  context.Canceled,
			wantCode: errors.ErrCodeCanceled,
		},
		{
			name:          "the run's own deadline expires during the scan",
			abort:         context.DeadlineExceeded,
			scanErr:       context.DeadlineExceeded,
			wantCode:      errors.ErrCodeTimeout,
			wantTransient: true,
		},
		{
			// The scan's budget is the shorter of the two, so this is the
			// slow cluster it exists to cut short, and nobody aborted.
			name: "the scan's own budget expires",
			scanErr: errors.Wrap(errors.ErrCodeTimeout,
				"timed out reading the cluster for resources an upgrade could disturb",
				context.DeadlineExceeded),
		},
		{
			name:    "RBAC denies the scan",
			scanErr: errors.New(errors.ErrCodeUnauthorized, "customresourcedefinitions is forbidden"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			to := clusterRecipe(t, filepath.Join(dir, "to.yaml"),
				map[string]string{"synthetic-beta": "0.19.0"})

			ctx := newAbortingContext(t.Context())
			f := &fakeCluster{
				result:  inventory.Result{Versions: map[string]string{"synthetic-beta": "0.18.0"}},
				scanErr: tt.scanErr,
			}
			if tt.abort != nil {
				f.onScan = func() { ctx.abort(tt.abort) }
			}
			client := f.client(t)

			report, err := client.UpgradeCheck(ctx, UpgradeCheckRequest{
				From: FromCluster, To: to, Deployer: "helm",
			})

			// Whichever way the row goes, the scan has to be what produced
			// it: an error raised before the scan ran would satisfy a bare
			// non-nil assertion without the branch under test being reached.
			if want := []string{"read", "scan"}; !reflect.DeepEqual(f.calls, want) {
				t.Fatalf("calls = %v, want %v (err = %v)", f.calls, want, err)
			}

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("UpgradeCheck failed on an advisory scan error: %v", err)
				}
				if report.AtRisk.Scanned {
					t.Error("AtRisk.Scanned = true after a scan that failed")
				}
				if !strings.Contains(report.AtRisk.Reason, tt.scanErr.Error()) {
					t.Errorf("AtRisk.Reason = %q, want the scan's own failure in it",
						report.AtRisk.Reason)
				}
				if want := rowsFailing(report); report.Summary.Failing != want {
					t.Errorf("Summary.Failing = %d, want %d (the failing rows); a scan that could "+
						"not answer must not move the exit code", report.Summary.Failing, want)
				}

				return
			}

			if err == nil {
				t.Fatal("UpgradeCheck error = nil; an aborted run must not report as a completed one")
			}
			if report != nil {
				t.Errorf("report = %#v, want none beside the error", report)
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("error = %v, want code %s", err, tt.wantCode)
			}
			// The code is not the property on its own: ErrCodeCanceled exists
			// so IsTransient reports false, while the bare context.Canceled
			// the scan returned carries the right sentinel and IsTransient
			// reports true for it.
			if got := errors.IsTransient(err); got != tt.wantTransient {
				t.Errorf("IsTransient = %v, want %v; a canceled run must not re-enter a retry loop "+
					"and an expired deadline must stay in the transient bucket", got, tt.wantTransient)
			}
			if !stderrors.Is(err, tt.abort) {
				t.Errorf("error = %v, want %v kept in the cause chain", err, tt.abort)
			}
		})
	}
}
