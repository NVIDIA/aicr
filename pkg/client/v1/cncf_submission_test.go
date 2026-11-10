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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func testCNCFVendor() CNCFVendor {
	return CNCFVendor{
		Name: "Test Vendor", PlatformName: "Test Platform", PlatformVersion: "1.2.3", Description: "A GPU platform under test.",
		ContactEmail: "owner@vendor.example", WebsiteURL: "https://vendor.example/platform", DocumentationURL: "https://vendor.example/docs",
		ProductLogoURL: "https://vendor.example/logo.svg", K8sConformanceURL: "https://github.com/cncf/k8s-conformance/tree/master/v1.37/test-platform",
	}
}

// landscapeTransport answers the vendored upstream validator's requests in
// memory on http.DefaultTransport: the CNCF landscape with the given
// members, every other URL with 200.
type landscapeTransport struct{ members []string }

func (f landscapeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if strings.HasSuffix(req.URL.Path, "/landscape.yml") {
		var sb strings.Builder
		sb.WriteString("landscape:\n  - name: CNCF Members\n    subcategories:\n      - name: Platinum\n        items:\n")
		for _, m := range f.members {
			sb.WriteString("          - name: " + m + " (member)\n")
		}
		body = sb.String()
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func installLandscape(t *testing.T, members ...string) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = landscapeTransport{members: members}
	t.Cleanup(func() { http.DefaultTransport = prev })
}

// testSubmissionSnapshot is a snapshot with the fingerprint dimensions and
// node names the submission README and redaction read.
func testSubmissionSnapshot() *Snapshot {
	return &Snapshot{internal: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
		measurement.NewMeasurement(measurement.TypeK8s).
			WithSubtype(measurement.Subtype{Name: "server", Data: map[string]measurement.Reading{"version": measurement.Str("v1.37.2")}}).
			WithSubtype(measurement.Subtype{Name: "node", Data: map[string]measurement.Reading{
				"provider": measurement.Str("aks"), "source-node": measurement.Str("aks-gpu-vmss000001")}}).Build(),
		measurement.NewMeasurement(measurement.TypeNodeTopology).
			WithSubtype(measurement.Subtype{Name: "summary", Data: map[string]measurement.Reading{"node-count": measurement.Int(3)}}).
			WithSubtype(measurement.Subtype{Name: "label", Data: map[string]measurement.Reading{
				"nvidia.com/gpu.product":         measurement.Str("NVIDIA-H100-80GB-HBM3|aks-gpu-vmss000001,aks-gpu-vmss000002"),
				"kubernetes.azure.com/agentpool": measurement.Str("system|aks-sys-vmss000000")}}).
			WithSubtype(measurement.Subtype{Name: "taint", Data: map[string]measurement.Reading{
				"nvidia.com/gpu": measurement.Str("NoSchedule|present|aks-gpu-vmss000001,aks-gpu-vmss000002")}}).Build(),
		measurement.NewMeasurement(measurement.TypeOS).
			WithSubtype(measurement.Subtype{Name: "release", Data: map[string]measurement.Reading{"ID": measurement.Str("ubuntu"), "VERSION_ID": measurement.Str("24.04")}}).Build(),
	}}}
}

func TestSnapshotNodeNames(t *testing.T) {
	got := snapshotNodeNames(testSubmissionSnapshot().internal.Measurements)
	want := []string{"aks-gpu-vmss000001", "aks-gpu-vmss000002", "aks-sys-vmss000000"}
	if !slices.Equal(got, want) {
		t.Errorf("snapshotNodeNames() = %v, want %v", got, want)
	}
	if got := snapshotNodeNames([]*measurement.Measurement{nil}); len(got) != 0 {
		t.Errorf("nil measurement: %v", got)
	}
	// A redacted or partial snapshot carries no source-node, and a node
	// subtype may carry a non-string reading under any key.
	noSource := measurement.NewMeasurement(measurement.TypeK8s).
		WithSubtype(measurement.Subtype{Name: "node", Data: map[string]measurement.Reading{"provider": measurement.Str("aks"), "count": measurement.Int(2)}}).
		WithSubtype(measurement.Subtype{Name: "node", Data: map[string]measurement.Reading{"source-node": measurement.Int(7)}}).
		WithSubtype(measurement.Subtype{Name: "node", Data: map[string]measurement.Reading{"source-node": nil}}).Build()
	if got := snapshotNodeNames([]*measurement.Measurement{noSource}); len(got) != 0 {
		t.Errorf("snapshot without a string source-node: %v", got)
	}
	nilTopology := measurement.NewMeasurement(measurement.TypeNodeTopology).
		WithSubtype(measurement.Subtype{Name: "label", Data: map[string]measurement.Reading{"a": nil, "b": measurement.Str("v|n1")}}).
		WithSubtype(measurement.Subtype{Name: "taint", Data: map[string]measurement.Reading{"c": nil}}).Build()
	if got := snapshotNodeNames([]*measurement.Measurement{nilTopology}); !slices.Equal(got, []string{"n1"}) {
		t.Errorf("nil topology readings: %v", got)
	}
}

func TestCNCFPlatform(t *testing.T) {
	rec := &RecipeResult{Name: "h100-aks-ubuntu-inference-dynamo", Version: "v1.3.0", Components: []ComponentRef{
		{Name: "gpu-operator", Version: "v26.7.2"}, {Name: "dra-node-labeler", Tag: "v0.3.0"},
	}}
	p := cncfPlatform(testSubmissionSnapshot(), rec)
	if p.KubernetesVersion != "1.37.2" || p.Service != "aks" || p.OS != "ubuntu" || p.OSVersion != "24.04" || p.NodeCount != 3 || p.GPUNodeCount != 2 {
		t.Errorf("platform facts = %+v", p)
	}
	if p.Recipe.Name != rec.Name || len(p.Components) != 2 || p.Components[1].Version != "v0.3.0" {
		t.Errorf("recipe facts = %+v", p)
	}
	if len(p.NodeNames) != 3 {
		t.Errorf("NodeNames = %v", p.NodeNames)
	}
	if got := cncfPlatform(&Snapshot{}, rec); got.KubernetesVersion != "" || got.Recipe.Name != rec.Name {
		t.Errorf("snapshot without measurements = %+v", got)
	}
}

func TestValidateCNCFSubmissionOptions(t *testing.T) {
	client, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	dir := filepath.Join(t.TempDir(), "out")
	tests := []struct {
		name    string
		client  *Client
		mutate  func(*CNCFSubmissionOptions)
		dir     string
		wantErr string
	}{
		{name: "well-formed", client: client, dir: dir},
		{name: "nil client", dir: dir, wantErr: "not initialized"},
		{name: "missing vendor field", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) { o.Vendor.ProductLogoURL = "" }, wantErr: "vendor.productLogoUrl is required"},
		{name: "malformed url", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) { o.Vendor.WebsiteURL = "vendor.example" }, wantErr: "vendor.websiteUrl"},
		{name: "bad notApplicable", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) { o.NotApplicable = map[string]string{"gang_scheduling": "x"} }, wantErr: "not a conditional MUST"},
		{name: "bad documented", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) {
			o.Documented = map[string]CNCFDocumentation{"cluster_autoscaling": {Notes: "x"}}
		}, wantErr: "documentation URL"},
		{name: "unsupported version", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) { o.ConformanceVersion = "v1.30" }, wantErr: "unsupported"},
		{name: "uncarried kubernetes minor", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) { o.KubernetesVersion = "1.34.3" }, wantErr: "no carried CNCF AI Conformance checklist certifies Kubernetes"},
		{name: "unparseable kubernetes version", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) { o.KubernetesVersion = "latest" }, wantErr: "no carried CNCF AI Conformance checklist"},
		{name: "explicit version overrides an uncarried minor", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) { o.KubernetesVersion, o.ConformanceVersion = "1.38.0", "v1.37" }},
		{name: "carried minor", client: client, dir: dir, mutate: func(o *CNCFSubmissionOptions) { o.KubernetesVersion = "v1.36.2-eks-abc" }},
		{name: "empty dir", client: client, dir: "", wantErr: "directory is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := CNCFSubmissionOptions{Vendor: testCNCFVendor()}
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			err := tt.client.ValidateCNCFSubmissionOptions(opts, tt.dir)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateCNCFSubmissionOptions() = %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q lacks %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestWriteCNCFSubmissionGuards(t *testing.T) {
	client, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	report := &ctrf.Report{}
	snap, rec := testSubmissionSnapshot(), &RecipeResult{Name: "r"}
	opts := CNCFSubmissionOptions{Vendor: testCNCFVendor()}
	dir := filepath.Join(t.TempDir(), "out")
	var nilClient *Client
	var nilCtx context.Context
	for _, tc := range []struct {
		name string
		call func() (*CNCFSubmissionResult, error)
		want string
	}{
		{"nil client", func() (*CNCFSubmissionResult, error) {
			return nilClient.WriteCNCFSubmission(context.Background(), report, dir, snap, rec, opts)
		}, "not initialized"},
		{"nil ctx", func() (*CNCFSubmissionResult, error) {
			return client.WriteCNCFSubmission(nilCtx, report, dir, snap, rec, opts)
		}, "context is required"},
		{"nil report", func() (*CNCFSubmissionResult, error) {
			return client.WriteCNCFSubmission(context.Background(), nil, dir, snap, rec, opts)
		}, "CTRF report is required"},
		{"nil snapshot", func() (*CNCFSubmissionResult, error) {
			return client.WriteCNCFSubmission(context.Background(), report, dir, nil, rec, opts)
		}, "snapshot and the recipe"},
		{"empty dir", func() (*CNCFSubmissionResult, error) {
			return client.WriteCNCFSubmission(context.Background(), report, "", snap, rec, opts)
		}, "directory is required"},
		{"no kubernetes version", func() (*CNCFSubmissionResult, error) {
			return client.WriteCNCFSubmission(context.Background(), report, dir, snap, rec, CNCFSubmissionOptions{Vendor: testCNCFVendor()})
		}, "needs the cluster's Kubernetes version"},
		{"uncarried minor", func() (*CNCFSubmissionResult, error) {
			return client.WriteCNCFSubmission(context.Background(), report, dir, snap, rec, CNCFSubmissionOptions{Vendor: testCNCFVendor(), KubernetesVersion: "1.34.0"})
		}, "no carried CNCF AI Conformance checklist"},
		{"withheld check", func() (*CNCFSubmissionResult, error) {
			withheld := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{{Name: "gang-scheduling", Status: ctrf.StatusSkipped, Extra: map[string]string{"skipReason": validator.SkipCheckReasonCode}}}}}
			return client.WriteCNCFSubmission(context.Background(), withheld, dir, snap, rec, opts)
		}, "withheld by WithValidationSkipChecks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err.Error(), tc.want)
			}
		})
	}
}

// TestWriteCNCFSubmission drives the facade end to end on the v1.37
// fixture: the checklist follows the snapshot's Kubernetes minor, the
// README carries the platform facts and the recipe's components, node names
// from the snapshot are aliased, and the pinned validator accepts the
// result. The per-file bytes are pinned in pkg/evidence/cncf's goldens.
func TestWriteCNCFSubmission(t *testing.T) {
	installLandscape(t, "Test Vendor")
	client, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	report := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{
		{Name: "gang-scheduling", Status: "passed", Duration: 1000, Stdout: []string{"members on aks-gpu-vmss000001 and aks-gpu-vmss000002"}},
		{Name: "accelerator-metrics", Status: "passed", Duration: 1000},
		{Name: "ai-service-metrics", Status: "passed", Duration: 1000},
		{Name: "inference-gateway", Status: "passed", Duration: 1000},
		{Name: "pod-autoscaling", Status: "passed", Duration: 1000},
		{Name: "cluster-autoscaling", Status: "passed", Duration: 1000, Extra: map[string]string{"observationOnly": "true"}},
		{Name: "robust-controller", Status: "passed", Duration: 1000},
		{Name: "secure-accelerator-access", Status: "passed", Duration: 1000},
	}}}
	rec := &RecipeResult{Name: "h100-aks-ubuntu-inference-dynamo", Version: "v1.3.0", Components: []ComponentRef{{Name: "gpu-operator", Version: "v26.7.2"}}}
	dir := filepath.Join(t.TempDir(), "submission")
	opts := CNCFSubmissionOptions{
		KubernetesVersion: "1.37.2",
		Vendor:            testCNCFVendor(),
		Commit:            "0123456789abcdef0123456789abcdef01234567",
		Documented: map[string]CNCFDocumentation{"cluster_autoscaling": {
			Evidence: []string{"https://vendor.example/docs/cluster-autoscaler"}, Notes: "The managed autoscaler scales the GPU pool.",
		}},
	}
	res, err := client.WriteCNCFSubmission(context.Background(), report, dir, testSubmissionSnapshot(), rec, opts)
	if err != nil {
		t.Fatalf("WriteCNCFSubmission() = %v", err)
	}
	if !res.Complete || res.Dir != dir || res.Aggregate.Version != "v1.37" || !res.Aggregate.Passed() {
		t.Fatalf("result = %+v", res)
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| Kubernetes version | 1.37.2 |", "| Kubernetes service | aks |", "| GPU nodes | 2 |", "`gpu-operator` | v26.7.2",
		"commit `0123456789abcdef0123456789abcdef01234567`", "`cluster_autoscaling` | MUST | Implemented | OBSERVED"} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("README lacks %q", want)
		}
	}
	page, err := os.ReadFile(filepath.Join(dir, "evidence", "gang-scheduling.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "aks-gpu-vmss") || !strings.Contains(string(page), "members on node-1 and node-2") {
		t.Errorf("node names not aliased from the snapshot:\n%s", page)
	}
	if got := res.Aggregate.Outcome(report, true); got != nil {
		t.Errorf("Outcome = %v", got)
	}

	// Without the documentation the observed MUST leaves the run incomplete
	// and nothing reaches the requested path.
	opts.Documented = nil
	dir2 := filepath.Join(t.TempDir(), "submission")
	res, err = client.WriteCNCFSubmission(context.Background(), report, dir2, testSubmissionSnapshot(), rec, opts)
	if err != nil {
		t.Fatalf("WriteCNCFSubmission() = %v", err)
	}
	if res.Complete {
		t.Error("Complete = true for an observed, undocumented MUST")
	}
	if _, statErr := os.Stat(dir2); !os.IsNotExist(statErr) {
		t.Error("requested directory exists for an incomplete run")
	}
	errorstest.WantReportedCode(t, res.Aggregate.Outcome(report, true), aicrerrors.ErrCodeInternal)
}

// An explicit ConformanceVersion certifies a cluster whose minor has no
// carried checklist, and the README records the checklist used.
func TestWriteCNCFSubmissionExplicitVersion(t *testing.T) {
	installLandscape(t, "Test Vendor")
	client, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	report := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{
		{Name: "gang-scheduling", Status: "passed"}, {Name: "accelerator-metrics", Status: "passed"}, {Name: "ai-service-metrics", Status: "passed"},
		{Name: "inference-gateway", Status: "passed"}, {Name: "pod-autoscaling", Status: "passed"}, {Name: "robust-controller", Status: "passed"},
		{Name: "secure-accelerator-access", Status: "passed"},
	}}}
	dir := filepath.Join(t.TempDir(), "submission")
	res, err := client.WriteCNCFSubmission(context.Background(), report, dir, testSubmissionSnapshot(), &RecipeResult{Name: "r"},
		CNCFSubmissionOptions{Vendor: testCNCFVendor(), KubernetesVersion: "1.38.0", ConformanceVersion: "v1.37",
			NotApplicable: map[string]string{"cluster_autoscaling": "fixed-size pools"}})
	if err != nil || !res.Complete || res.Aggregate.Version != "v1.37" {
		t.Fatalf("WriteCNCFSubmission() = %v %+v", err, res)
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "**Checklist:** v1.37") {
		t.Errorf("README does not record the explicit checklist:\n%s", readme)
	}
	if !strings.Contains(string(readme), "selected explicitly by the submitter") {
		t.Errorf("README does not say the checklist was selected explicitly (#3703):\n%s", readme)
	}
}

// A validator that cannot start surfaces as unavailable through the facade.
func TestWriteCNCFSubmissionValidatorUnavailable(t *testing.T) {
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Status: "502", Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = prev })
	client, err := NewClient(WithRecipeSource(EmbeddedSource()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	report := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{
		{Name: "gang-scheduling", Status: "passed"}, {Name: "accelerator-metrics", Status: "passed"}, {Name: "ai-service-metrics", Status: "passed"},
		{Name: "inference-gateway", Status: "passed"}, {Name: "pod-autoscaling", Status: "passed"}, {Name: "robust-controller", Status: "passed"},
		{Name: "secure-accelerator-access", Status: "passed"},
	}}}
	_, err = client.WriteCNCFSubmission(context.Background(), report, filepath.Join(t.TempDir(), "out"), testSubmissionSnapshot(), &RecipeResult{Name: "r"},
		CNCFSubmissionOptions{Vendor: testCNCFVendor(), KubernetesVersion: "1.37.2", NotApplicable: map[string]string{"cluster_autoscaling": "fixed-size pools"}})
	errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeUnavailable)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
