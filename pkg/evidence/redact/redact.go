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

// Package redact minimizes the sensitive operational detail an evidence
// bundle physically ships, while leaving the cryptographic verification
// story intact.
//
// The signed predicate commits to artifacts by hash and carries the derived
// fingerprint / criteria-match / per-phase counts — that is the conformance
// signal. The snapshot and CTRF payloads are the *backing content* those
// digests point at, not the signal itself, so they can be shrunk without
// weakening the binding.
//
// Two transforms are applied by the minimal policy:
//
//   - Snapshot: a fail-closed allowlist that is enforced at every level —
//     measurement type, subtype, AND data key. Only enumerated types,
//     subtypes, and keys survive; a new type, subtype, or key a future
//     collector adds is dropped until explicitly allowlisted (there is no
//     keep-all subtype). Node names, provider instance IDs, the raw node
//     label/taint set, kernel/sysctl tuning, loaded modules, and systemd
//     service config are dropped.
//   - CTRF: per-test Stdout and Message (free-form log text that can leak IPs,
//     DNS names, secret/cert names, internal URLs) are omitted; the pass/fail
//     signal (name, status, duration, suite, summary counts) is preserved. The
//     structured per-test Extra map is rebuilt against a fail-closed key AND
//     value allowlist (ctrfExtraAllowlist): only enumerated low-cardinality
//     keys survive, and each surviving value must additionally pass its key's
//     validator (non-negative decimal count, or a closed set of known skip
//     codes) — so an identifier smuggled under an allowed key (e.g. an IP in
//     nodesTotal, a hostname or unminted code in skipReason) is dropped, not
//     published. This is
//     the publication boundary: emission-side validation alone is bypassable
//     via raw prefixed stdout. A signed bundle can still distinguish a
//     1-of-2-node pass from 2-of-2 and preserve a skip reason without shipping
//     free-form log text. An Extra map that retains nothing is dropped rather
//     than shipped as an empty object.
//
// Both functions are pure and non-mutating: they build fresh structures and
// never alter their inputs, so the full (unredacted) artifacts remain
// available for the --full emit path and for computing the predicate
// fingerprint from the raw snapshot.
package redact

import (
	"regexp"
	"strconv"

	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

const (
	// PolicyName identifies the redaction policy recorded in the predicate.
	PolicyName = "minimal"

	// PolicyVersion is the allowlist/scrub-rule version. Bump on any change
	// to what survives redaction so verifiers can tell which rules ran.
	//
	// v2 added the per-test CTRF Extra allowlist (ctrfExtraAllowlist):
	// allowlisted structured keys whose values match the key's canonical shape
	// (count / enum code) now survive minimal redaction.
	PolicyVersion = "v2"
)

// headerMetadataAllowlist is the fail-closed set of snapshot header metadata
// keys safe to publish. The collecting node's name (`source-node`) and any key
// a future writer adds are dropped unless listed here.
var headerMetadataAllowlist = map[string]struct{}{
	"timestamp": {},
	"version":   {},
}

// subtypePolicy is the allowlist of data keys that survive within a kept
// subtype. Every kept subtype is key-constrained — there is no keep-all
// escape hatch — so a key a future collector attaches to an allowlisted
// subtype is dropped by default until added here (fail-closed at key level).
type subtypePolicy struct {
	keys map[string]struct{}
}

func keep(keys ...string) subtypePolicy {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return subtypePolicy{keys: set}
}

// snapshotAllowlist is the fail-closed allowlist: only the measurement types,
// subtypes, and data keys listed here survive. A type not present is dropped
// entirely; a subtype not present for a kept type is dropped entirely; and a
// data key not present in its subtype's policy is dropped. Every subtype is
// key-constrained (no keep-all), so the guarantee holds at the key level too.
//
// The subtype names and data keys mirror the literals the collectors author
// (pkg/collector/{k8s,os,gpu,topology}) — the same names pkg/fingerprint keys
// off. They are not shared constants, so a collector that renames a subtype or
// key must update this table too; otherwise the renamed field is silently
// dropped from the minimal snapshot (fail-closed, but lossy). Bump PolicyVersion
// when the allowlist changes.
var snapshotAllowlist = map[measurement.Type]map[string]subtypePolicy{
	measurement.TypeK8s: {
		"server": keep("version", "platform", "goVersion"),
		"node": keep(
			"provider",
			"kubelet-version",
			"kernel-version",
			"operating-system",
			"os-image",
			"container-runtime-name",
			"container-runtime-version",
		), // drops source-node, provider-id, container-runtime-id
	},
	measurement.TypeGPU: {
		"hardware": keep(
			"gpu-present",
			"gpu-count",
			"driver-loaded",
			"detection-source",
			"model",
		),
	},
	measurement.TypeOS: {
		// /etc/os-release distro identity. Key-constrained because the
		// collector ships the whole file verbatim, so a non-standard distro
		// could inject arbitrary (operator-influenced) keys otherwise.
		"release": keep(
			"ID",
			"ID_LIKE",
			"NAME",
			"PRETTY_NAME",
			"VERSION",
			"VERSION_ID",
			"VERSION_CODENAME",
		),
		// grub, sysctl, kmod intentionally absent → dropped (tuning/hardening posture)
	},
	measurement.TypeNodeTopology: {
		"summary": keep("node-count", "taint-count", "label-count"), // counts, not identifiers
		// label, taint intentionally absent → dropped (node names + custom labels)
	},
	// TypeSystemD intentionally absent → entire measurement dropped.
}

// snapshotAppliedRules is the static, sorted description of what the minimal
// policy removes from a snapshot. Static (rather than input-derived) so the
// recorded redaction provenance stays byte-stable across runs.
var snapshotAppliedRules = []string{
	"snapshot.header.allowlist",
	"snapshot.measurements.allowlist",
}

// ctrfExtraValidator reports whether v is a safe published value for its key.
// It is the value half of the fail-closed contract: an allowlisted key alone is
// not enough — the value must also match the key's canonical shape, so a value
// that structurally looks like an identifier (IP, hostname, FQDN, free text)
// never survives under an allowed key.
type ctrfExtraValidator func(v string) bool

// maxCountDigits bounds a published count value. Five digits (up to 99,999)
// comfortably covers realistic node/GPU counts while keeping the attacker-
// chosen numeric channel narrow — the premise is that only LOW-cardinality
// counts cross the publication boundary, and EmitExtra itself does no value
// validation, so this allowlist regex is the defense.
const maxCountDigits = 5

// ctrfCountValue matches a bare non-negative decimal count — no sign, dot, or
// separator — so an IP or instance id smuggled into a count key (e.g.
// "10.0.0.5") fails closed. The digit bound (maxCountDigits) caps the value.
var ctrfCountValue = regexp.MustCompile(`^[0-9]{1,` + strconv.Itoa(maxCountDigits) + `}$`)

func isCountValue(v string) bool { return ctrfCountValue.MatchString(v) }

// ctrfSkipReasons is the CLOSED set of skipReason codes safe to publish. A
// regex on kebab-case shape is not enough — it would still pass an arbitrary
// low-cardinality identifier like "customer-prod-cluster". Only codes minted by
// a check (see validators/deployment/nvidia_smi.go's skipReason* constants) are
// listed; any other value, including a well-formed but unlisted code, is dropped
// fail-closed. A new skip code must be added here in the same change that emits
// it — same discipline as the key allowlist.
var ctrfSkipReasons = map[string]struct{}{
	"no-gpu-nodes":             {}, // cluster has no GPU nodes at all
	"no-schedulable-gpu-nodes": {}, // GPU nodes exist but all cordoned/unschedulable
	"nodes-busy":               {}, // schedulable GPU nodes exist but are busy with workloads
}

func isSkipReason(v string) bool { _, ok := ctrfSkipReasons[v]; return ok }

// ctrfExtraAllowlist is the fail-closed set of TestResult.Extra keys safe to
// publish in a minimal (default) evidence bundle, each paired with the
// validator its value must pass. Every key carries only low-cardinality counts
// or a closed-set enum code — never node names, IPs, or other operator-
// identifying text — because unlike Stdout/Message these values are NOT stripped
// by default. A key a future check adds is dropped until listed here; a value
// that fails its validator is dropped even under an allowed key. Keep the map
// keys mirrored in the ctrf godoc and docs/contributor/validator.md.
var ctrfExtraAllowlist = map[string]ctrfExtraValidator{
	"nodesValidated": isCountValue, // count of nodes a coverage check actually verified
	"nodesTotal":     isCountValue, // count of candidate nodes (validated + skipped/cordoned)
	"skipReason":     isSkipReason, // closed-set code for why a check skipped
}

// ctrfAppliedRules is the static, sorted description of the CTRF scrub.
var ctrfAppliedRules = []string{
	"ctrf.tests.extra.allowlist",
	"ctrf.tests.omit:message",
	"ctrf.tests.omit:stdout",
}

// Snapshot returns a redacted deep copy of in and the sorted list of applied
// rule identifiers. It never mutates in. Returns (nil, nil) when in is nil.
func Snapshot(in *snapshotter.Snapshot) (*snapshotter.Snapshot, []string) {
	if in == nil {
		return nil, nil
	}

	out := &snapshotter.Snapshot{
		Header: redactHeader(in.Header),
		// The advisory fingerprint is kept as-is: the same structured
		// fingerprint is already computed and signed into the predicate, so
		// retaining it here is not new disclosure. It is an immutable value,
		// safe to share with the input.
		Fingerprint: in.Fingerprint,
	}

	for _, m := range in.Measurements {
		if rm := redactMeasurement(m); rm != nil {
			out.Measurements = append(out.Measurements, rm)
		}
	}

	return out, append([]string(nil), snapshotAppliedRules...)
}

// redactMeasurement returns an allowlisted copy of m, or nil if the whole
// measurement is dropped (unlisted type, or no subtype survives).
func redactMeasurement(m *measurement.Measurement) *measurement.Measurement {
	if m == nil {
		return nil
	}
	subPolicies, ok := snapshotAllowlist[m.Type]
	if !ok {
		return nil
	}
	out := &measurement.Measurement{Type: m.Type}
	for i := range m.Subtypes {
		st := &m.Subtypes[i]
		pol, ok := subPolicies[st.Name]
		if !ok {
			continue
		}
		cp := copySubtype(st, pol)
		if len(cp.Data) == 0 {
			// A key-constrained subtype that retained nothing is dropped
			// rather than shipped as an empty `data: {}` — that would be a
			// fail-open hole and would also fail measurement.Subtype.Validate
			// for any downstream consumer that re-reads the snapshot.
			continue
		}
		out.Subtypes = append(out.Subtypes, cp)
	}
	if len(out.Subtypes) == 0 {
		return nil
	}
	return out
}

// copySubtype copies st, retaining only the data keys in pol — every other
// key (including any a future collector adds to this subtype) is dropped,
// fail-closed at the key level. Reading values are immutable wrappers, so
// sharing them is safe. The subtype's Context is intentionally NOT carried
// over: it is not allowlisted and carries no conformance signal, so passing
// it through would be a fail-open path as collectors start attaching context.
func copySubtype(st *measurement.Subtype, pol subtypePolicy) measurement.Subtype {
	data := make(map[string]measurement.Reading, len(pol.keys)) // upper bound on survivors
	for k, v := range st.Data {
		if _, allowed := pol.keys[k]; allowed {
			data[k] = v
		}
	}
	return measurement.Subtype{Name: st.Name, Data: data}
}

func redactHeader(h header.Header) header.Header {
	out := header.Header{Kind: h.Kind, APIVersion: h.APIVersion}
	md := make(map[string]string, len(headerMetadataAllowlist))
	for k, v := range h.Metadata {
		if _, ok := headerMetadataAllowlist[k]; ok {
			md[k] = v
		}
	}
	if len(md) > 0 {
		out.Metadata = md
	}
	return out
}

// CTRF returns a redacted deep copy of in with per-test Stdout and Message
// omitted and each per-test Extra map rebuilt against ctrfExtraAllowlist, plus
// the sorted list of applied rule identifiers. It never mutates in. Returns
// (nil, nil) when in is nil.
func CTRF(in *ctrf.Report) (*ctrf.Report, []string) {
	if in == nil {
		return nil, nil
	}

	// out := *in copies every field by value, including the Results struct
	// (Tool, Summary, and the shared Environment pointer — none sensitive).
	// Only Results.Tests is rebuilt below so the input is never mutated.
	out := *in

	if in.Results.Tests != nil {
		tests := make([]ctrf.TestResult, len(in.Results.Tests))
		for i, tr := range in.Results.Tests {
			tr.Stdout = nil
			tr.Message = ""
			tr.Extra = allowlistExtra(tr.Extra)
			tests[i] = tr
		}
		out.Results.Tests = tests
	}

	return &out, append([]string(nil), ctrfAppliedRules...)
}

// allowlistExtra returns a fresh map containing only the ctrfExtraAllowlist
// entries of in whose value also passes the key's validator — every other key
// (including any a future check adds) and every ill-shaped value (an identifier
// smuggled under an allowed key) is dropped fail-closed. Returns nil when in is
// empty or nothing survives, so no empty `extra: {}` ships. It never mutates in.
func allowlistExtra(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	var out map[string]string
	for k, v := range in {
		validate, ok := ctrfExtraAllowlist[k]
		if !ok || !validate(v) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(ctrfExtraAllowlist))
		}
		out[k] = v
	}
	return out
}
