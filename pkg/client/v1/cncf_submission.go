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
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// CNCFVendor is the product metadata a CNCF AI Conformance submission's
// PRODUCT.yaml carries: spec.validate.evidence.cncf.vendor
// (CNCFEvidenceOptions.Vendor). Every field but RepoURL is required by the
// upstream validator, which also checks that each URL answers and that Name
// matches a CNCF member in the landscape. ValidateCNCFSubmissionOptions
// applies the shape rules before a run; reachability is the validator's.
type CNCFVendor struct {
	// Name is vendorName, the certifying legal entity exactly as listed
	// under CNCF Members in the CNCF landscape.
	Name string

	// PlatformName and PlatformVersion identify the product certified.
	PlatformName    string
	PlatformVersion string

	// Description is the one-sentence description of the offering.
	Description string

	// ContactEmail is contactEmailAddress.
	ContactEmail string

	// WebsiteURL, DocumentationURL and ProductLogoURL (SVG, AI or EPS) are
	// required; RepoURL is the optional source repository.
	WebsiteURL       string
	DocumentationURL string
	RepoURL          string
	ProductLogoURL   string

	// K8sConformanceURL is the product's Kubernetes conformance submission,
	// https://github.com/cncf/k8s-conformance/tree/master/vX.Y/<product>.
	K8sConformanceURL string
}

// CNCFDocumentation is a vendor's manual attestation of one requirement the
// run only observed: public documentation URLs and the PRODUCT.yaml note.
type CNCFDocumentation struct {
	Evidence []string
	Notes    string
}

// CNCFSubmissionOptions configures Client.WriteCNCFSubmission.
type CNCFSubmissionOptions struct {
	// ConformanceVersion names the checklist explicitly; otherwise
	// KubernetesVersion's minor selects it and must be a carried version
	// exactly: unlike the evidence-pages render, a submission never falls
	// back to a neighboring checklist, since it certifies one minor.
	ConformanceVersion string
	KubernetesVersion  string

	// NotApplicable and Documented are RenderCNCFEvidenceOptions' fields of
	// the same names.
	NotApplicable map[string]string
	Documented    map[string]CNCFDocumentation

	// Vendor is required.
	Vendor CNCFVendor

	// Commit is the AICR build commit recorded in README.md beside the
	// Client's version; empty leaves it out.
	Commit string
}

// CNCFStagedDirContextKey is the StructuredError.Context key under which an
// error WriteCNCFSubmission returns after the redaction pass completed (a
// validator rejection, an interrupted validator, a failure to move the
// directory into place) carries the staged product directory that was kept
// for inspection; it must be removed before the next run. Errors before
// that point carry none: the tree was removed.
const CNCFStagedDirContextKey = cncf.StagedDirContextKey

// CNCFSubmissionResult is what Client.WriteCNCFSubmission produced.
type CNCFSubmissionResult struct {
	// Aggregate is the requirement aggregation the directory was rendered
	// from; the run's exit status is its Outcome, as for the evidence-pages
	// render.
	Aggregate *CNCFRequirementAggregate

	// Complete is true when every applicable MUST is established and the
	// pinned upstream validator accepted the directory, which then lives at
	// Dir, the requested path. When false nothing was written to the
	// requested path: Dir is the kept staging copy
	// (<requested>.incomplete/vX.Y/<name>) that documents what was and was
	// not shown.
	Complete bool
	Dir      string

	// ValidatorOutput is the upstream validator's report when it ran.
	ValidatorOutput string

	// Redaction lists what the redaction pass changed.
	Redaction []string
}

// ValidateCNCFSubmissionOptions checks CNCFSubmissionOptions and the
// submission directory the way WriteCNCFSubmission will, without a run: the
// checklist version resolves, NotApplicable and Documented are well-formed
// (ValidateCNCFEvidenceOptions), every required vendor field is present and
// well-formed, and dir is absent or empty with no stale incomplete sibling
// and no "vX.Y" parent segment. It exists so a bad config fails before the
// validator Jobs run; whether the vendor's URLs answer and whether Vendor.Name
// is a CNCF member is the upstream validator's check at the end of the run.
//
// Errors:
//   - ErrCodeInvalidRequest naming the field or condition that fails.
//   - ErrCodeInternal when dir cannot be resolved or read.
func (c *Client) ValidateCNCFSubmissionOptions(opts CNCFSubmissionOptions, dir string) error {
	if c == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if err := c.ValidateCNCFEvidenceOptions(RenderCNCFEvidenceOptions{
		ConformanceVersion: opts.ConformanceVersion, KubernetesVersion: opts.KubernetesVersion,
		NotApplicable: opts.NotApplicable, Documented: opts.Documented,
	}); err != nil {
		return err
	}
	vendor := toInternalVendor(opts.Vendor)
	if err := vendor.Validate(); err != nil {
		return err
	}
	if opts.KubernetesVersion != "" {
		if err := requireExactConformanceVersion(opts); err != nil {
			return err
		}
	}
	return cncf.ValidateSubmissionDir(dir)
}

// requireExactConformanceVersion refuses a submission for a cluster whose
// Kubernetes minor has no carried checklist unless ConformanceVersion names
// one explicitly. The evidence-pages render falls back to the nearest
// carried checklist with a warning, which is fine for pages; a submission
// certifies the exact minor and must not be filed against a neighbor.
//
// Errors:
//   - ErrCodeInvalidRequest when ConformanceVersion is empty and
//     KubernetesVersion is empty, unparseable, or an uncarried minor.
func requireExactConformanceVersion(opts CNCFSubmissionOptions) error {
	if opts.ConformanceVersion != "" {
		return nil
	}
	if opts.KubernetesVersion == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"a submission needs the cluster's Kubernetes version (CNCFSubmissionOptions.KubernetesVersion) or an explicit ConformanceVersion: the checklist certifies one minor")
	}
	if _, ok := cncf.VersionForKubernetes(opts.KubernetesVersion); !ok {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"no carried CNCF AI Conformance checklist certifies Kubernetes %q (carried: %v); a submission is filed for the cluster's exact minor, so this cluster cannot be submitted with this build, or pass ConformanceVersion to choose a checklist deliberately",
			opts.KubernetesVersion, cncf.SupportedVersions()))
	}
	return nil
}

// WriteCNCFSubmission writes the complete CNCF AI Conformance submission
// directory for one conformance run to dir, from the merged CTRF report
// (MergeReports), the snapshot the run was validated against and the recipe
// it was selected from: PRODUCT.yaml, README.md, junit.xml, e2e.log and the
// evidence pages under evidence/, redacted in one pass, then checked by the
// pinned upstream validator (cncf/k8s-ai-conformance scripts/validate.go,
// which needs network access to the CNCF landscape and to every URL cited).
// It is what `aicr validate --cncf-submission` writes.
//
// Statuses are never typed: an established requirement is Implemented, a
// conditional MUST in opts.NotApplicable is N/A, everything else is Not
// Implemented. A requirement the run only observed is established by an
// opts.Documented entry, citing the documentation rather than the testcase.
// In junit.xml and e2e.log a non-passing check behind a Not Implemented
// SHOULD, an N/A requirement or supporting evidence is exported as skipped
// with the original verdict in its message; one behind an applicable MUST
// stays a failure.
//
// The directory is written under <dir>.incomplete and moved to dir only when
// every applicable MUST is established and the validator accepts it, so a
// directory at dir is complete by construction. Otherwise the staging copy
// is kept and the result's Dir names it; when a MUST is not established the
// validator is not run and the error is nil, since the exit status is the
// aggregate's Outcome, exactly as for RenderCNCFEvidenceWithResult.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client, ctx, report, snap or rec is
//     nil, dir is empty or fails ValidateCNCFSubmissionOptions, an option
//     is malformed, the cluster's Kubernetes minor has no carried checklist
//     and no ConformanceVersion was given, or the report carries a check
//     withheld by WithValidationSkipChecks (as RenderCNCFEvidenceWithResult
//     refuses).
//   - ErrCodeConflict when <dir>.incomplete appeared between the pre-flight
//     and the claim (another run, or a tree left behind); it is never
//     removed by this run.
//   - ErrCodeUnavailable when the upstream validator could not run.
//   - ErrCodeInternal when a file cannot be written or the validator
//     rejected the directory; the message carries its report.
//   - ErrCodeTimeout when writing exceeds defaults.CNCFSubmissionTimeout,
//     or ErrCodeCanceled when ctx is canceled.
//
// An error returned after the redaction pass completed carries the kept
// staged directory in its context under CNCFStagedDirContextKey.
func (c *Client) WriteCNCFSubmission(ctx context.Context, report *ctrf.Report, dir string, snap *Snapshot, rec *RecipeResult, opts CNCFSubmissionOptions) (*CNCFSubmissionResult, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if report == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "a CTRF report is required (got nil)")
	}
	if snap == nil || rec == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "the snapshot and the recipe of the run are required (got nil)")
	}
	if dir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "CNCF submission directory is required")
	}
	if err := rejectWithheldChecks(report); err != nil {
		return nil, err
	}
	if err := c.ValidateCNCFSubmissionOptions(opts, dir); err != nil {
		return nil, err
	}
	if err := requireExactConformanceVersion(opts); err != nil {
		return nil, err
	}
	version, err := resolveConformanceVersion(RenderCNCFEvidenceOptions{
		ConformanceVersion: opts.ConformanceVersion, KubernetesVersion: opts.KubernetesVersion,
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.CNCFSubmissionTimeout)
	defer cancel()

	res, err := cncf.WriteSubmission(ctx, dir, cncf.SubmissionInput{
		Report:          report,
		Version:         version,
		VersionExplicit: opts.ConformanceVersion != "",
		Options:         cncf.AggregateOptions{NotApplicable: opts.NotApplicable, Documented: toInternalDocumented(opts.Documented)},
		Vendor:          toInternalVendor(opts.Vendor),
		Platform:        cncfPlatform(snap, rec),
		Tool:            cncf.Tool{Version: c.version, Commit: opts.Commit},
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "CNCF submission failed")
	}
	return &CNCFSubmissionResult{
		Aggregate:       res.Aggregate,
		Complete:        res.Complete,
		Dir:             res.Dir,
		ValidatorOutput: res.ValidatorOutput,
		Redaction:       res.Redaction,
	}, nil
}

func toInternalVendor(v CNCFVendor) cncf.Vendor {
	return cncf.Vendor{
		Name:              v.Name,
		PlatformName:      v.PlatformName,
		PlatformVersion:   v.PlatformVersion,
		Description:       v.Description,
		ContactEmail:      v.ContactEmail,
		WebsiteURL:        v.WebsiteURL,
		DocumentationURL:  v.DocumentationURL,
		RepoURL:           v.RepoURL,
		ProductLogoURL:    v.ProductLogoURL,
		K8sConformanceURL: v.K8sConformanceURL,
	}
}

func toInternalDocumented(in map[string]CNCFDocumentation) map[string]cncf.Documentation {
	if in == nil {
		return nil
	}
	out := make(map[string]cncf.Documentation, len(in))
	for id, d := range in {
		out[id] = cncf.Documentation{Evidence: append([]string(nil), d.Evidence...), Notes: d.Notes}
	}
	return out
}

// cncfPlatform derives the README's platform facts from the snapshot's
// fingerprint dimensions and the recipe's enabled components, and the node
// names the redaction pass aliases from the snapshot's topology and node
// measurements.
func cncfPlatform(snap *Snapshot, rec *RecipeResult) cncf.Platform {
	p := cncf.Platform{Recipe: cncf.RecipeInfo{Name: rec.Name, Version: rec.Version}}
	for _, comp := range rec.Components {
		version := comp.Version
		if version == "" {
			version = comp.Tag
		}
		p.Components = append(p.Components, cncf.Component{Name: comp.Name, Version: version})
	}
	internal := snap.Unwrap()
	if internal == nil {
		return p
	}
	fp := fingerprint.FromMeasurements(internal.Measurements)
	p.KubernetesVersion = fp.K8sVersion.Value
	p.Service = fp.Service.Value
	p.Accelerator = fp.Accelerator.Value
	p.GPUModel = fp.GPUModel.Value
	p.OS = fp.OS.Value
	p.OSVersion = fp.OS.Version
	p.NodeCount = fp.NodeCount.Value
	p.GPUNodeCount = fp.GPUNodeCount.Value
	p.NodeNames = snapshotNodeNames(internal.Measurements)
	return p
}

// Snapshot measurement subtypes that carry node names: the topology
// collector's label and taint maps ("<value>|<node>,<node>" and
// "<effect>|<value>|<node>,<node>") and the K8s node subtype's source-node.
const (
	topologySubtypeLabel  = "label"
	topologySubtypeTaint  = "taint"
	k8sSubtypeNode        = "node"
	k8sNodeKeySourceNode  = "source-node"
	topologyNodeSeparator = "|"
)

// snapshotNodeNames collects every node name the snapshot mentions, sorted
// and distinct, for the redaction pass.
func snapshotNodeNames(measurements []*measurement.Measurement) []string {
	seen := map[string]bool{}
	for _, m := range measurements {
		if m == nil {
			continue
		}
		for _, st := range m.Subtypes {
			switch {
			case m.Type == measurement.TypeNodeTopology && (st.Name == topologySubtypeLabel || st.Name == topologySubtypeTaint):
				for _, reading := range st.Data {
					if reading == nil {
						continue
					}
					parts := strings.Split(reading.String(), topologyNodeSeparator)
					for _, name := range strings.Split(parts[len(parts)-1], ",") {
						if name = strings.TrimSpace(name); name != "" {
							seen[name] = true
						}
					}
				}
			case m.Type == measurement.TypeK8s && st.Name == k8sSubtypeNode:
				// Absent in redacted and partial snapshots; a Reading is an
				// interface, so the lookup must be checked before use.
				if reading, ok := st.Data[k8sNodeKeySourceNode]; ok && reading != nil {
					if name, isString := reading.Any().(string); isString && strings.TrimSpace(name) != "" {
						seen[strings.TrimSpace(name)] = true
					}
				}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
