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

package recipe

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	nfdPubNFD             = "nfd"
	nfdPubNetworkOperator = "network-operator"
	nfdPubMellanoxLabel   = "feature.node.kubernetes.io/pci-15b3.present"
	nfdPubMellanoxBare    = "pci-15b3.present"
	nfdPubNetOpRuleKey    = "network-operator nfd.deployNodeFeatureRules"
	nfdPubAKSRuleManifest = "components/network-operator/manifests/nfd-network-rule.yaml"
)

// nfdPubChartRule is a NodeFeatureRule that a registry chart renders itself,
// outside recipes/components/*/manifests. nfd-master evaluates it against the
// features nfd-worker publishes, so the feature it reads must not appear in
// worker.config.core.noPublishFeatures.
type nfdPubChartRule struct {
	component    string
	enablePath   []string
	chartDefault bool
	feature      string
}

// nfdPubChartRules comes from the chart templates at the registry pins on
// 2026-10-06: network-operator 26.4.1 templates/nodefeaturerules.yaml,
// gpu-operator v26.7.1 templates/nodefeaturerules.yaml, k8s-nim-operator 3.1.0
// templates/node-feature-rule.yaml. Re-check it when one of those pins moves.
var nfdPubChartRules = []nfdPubChartRule{
	{component: "network-operator", enablePath: []string{"nfd", "deployNodeFeatureRules"}, chartDefault: true, feature: "pci.device"},
	{component: "gpu-operator", enablePath: []string{"nfd", "nodefeaturerules"}, chartDefault: false, feature: "kernel.loadedmodule"},
	{component: "k8s-nim-operator", enablePath: []string{"nfd", "nodeFeatureRules", "deviceID"}, chartDefault: false, feature: "pci.device"},
	{component: "k8s-nim-operator-ocp", enablePath: []string{"nfd", "nodeFeatureRules", "deviceID"}, chartDefault: false, feature: "pci.device"},
}

// nfdPubTransitional is the expand phase of the #3038 pci-15b3.present
// handoff: for one release the cluster-side NIC rules ship beside the
// nfd-worker rule, so the label does not drop while nfd-worker rolls (except
// under Flux on AKS, docs/user/component-catalog.md), and once a node's worker
// strips pci.* they match nothing. Each key is a chart rule
// (component and enable path) or a manifest path; the value is the feature it
// reads, and a rule is skipped only when both match, on a leaf that ships
// network-operator. Invariant 8 requires both rules in this release, and an
// entry that no leaf uses fails the test. Delete the entries and invariant 8
// together with the rules in the contract change. That change must still
// protect clusters upgrading from releases before this one, for example by
// keeping the rules until those releases are out of support or by documenting
// a minimum upgrade source, because a cluster that skips this release meets
// the upgrade race again.
var nfdPubTransitional = map[string]string{
	nfdPubNetOpRuleKey:    "pci.device",
	nfdPubAKSRuleManifest: "pci.device",
}

// nfdPubStripList is the #3038 nfd-master memory fix and must change together
// with worker.config.core.noPublishFeatures in recipes/components/nfd/values.yaml.
var nfdPubStripList = []string{
	"cpu.*", "kernel.config", "kernel.enabledmodule", "kernel.kvm", "kernel.selinux", "kernel.version",
	"local.*", "memory.*", "network.*", "pci.*", "storage.*", "system.*", "usb.*",
}

// nfdPubTemplateLineRe matches the Helm template actions that open a line,
// which nfdPubManifestRuleFeatures drops; nfdPubTemplateActionRe matches any
// other action, which it replaces with nfdPubTemplatePlaceholder so the rest
// of the document still decodes. An action ends at its first }}, so literal
// YAML between two actions is kept.
var (
	nfdPubTemplateLineRe   = regexp.MustCompile(`(?m)^([ \t]*)(?:\{\{(?:[^}]|\}[^}])*\}\}[ \t]*)+`)
	nfdPubTemplateActionRe = regexp.MustCompile(`\{\{(?:[^}]|\}[^}])*\}\}`)
)

// nfdPubTemplatePlaceholder is the scalar a mid-line template action becomes.
// No NFD feature name contains it, so a feature that does was templated.
const nfdPubTemplatePlaceholder = "AICRTEMPLATEACTION"

// TestNFDFeaturePublishingContract resolves every shipped leaf and checks, on
// merged effective values:
//
//  1. A recipe with network-operator has exactly one nfd-worker sources.custom
//     rule producing pci-15b3.present="true" (the NicClusterPolicy
//     nodeAffinity and the RDMA readiness gate cohort), matching vendor 15b3
//     and device 101c/101e on AKS, vendor 15b3 and class 0200/0207 elsewhere.
//  2. A recipe without network-operator has no such rule, whatever its value:
//     the GPU Operator validator waits for the network-operator MOFED driver
//     on labeled nodes when GPUDirect RDMA is on and useHostMofed is false.
//  3. No NodeFeatureRule or NodeFeatureGroup the recipe renders (chart-shipped
//     or a manifest) reads a feature that nfd-worker strips, except the two
//     transitional NIC rules on a leaf with network-operator (see
//     nfdPubTransitional).
//  4. A recipe with nfd strips exactly nfdPubStripList once worker.extraArgs
//     overrides apply, and does not set noPublish: the nfd-master memory fix
//     for #3038.
//  5. A recipe with nfd has no worker.config.sources.pci that emits
//     pci-15b3.present, a second producer invariants 1 and 2 cannot see.
//  6. A recipe with network-operator leaves the rule in 1 running: nothing
//     disables the custom label source or the pci feature source, and no
//     label whitelist filters labels.
//  7. A recipe with nfd has no sources.custom rule whose labelsTemplate reads
//     pci.device: invariants 1 and 2 count static labels only.
//  8. A recipe with network-operator ships the transitional NIC rule: the
//     chart rule (nfd.deployNodeFeatureRules) off AKS, and on AKS
//     nfd-network-rule.yaml with the chart rule off.
//
// A rule label counts with or without the feature.node.kubernetes.io/ prefix,
// because nfd-master adds that prefix to an un-namespaced label.
func TestNFDFeaturePublishingContract(t *testing.T) {
	ctx := context.Background()
	store, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("buildMetadataStore: %v", err)
	}

	// The subtests write ran and transitionalUsed without a lock, so they must
	// not call t.Parallel.
	leaves, ran := 0, 0
	transitionalUsed := map[string]bool{}
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		leaves++
		criteria := overlay.Spec.Criteria
		t.Run(name, func(t *testing.T) {
			ran++
			result, err := store.BuildRecipeResult(ctx, criteria)
			if err != nil {
				t.Fatalf("resolve %s: %v", name, err)
			}
			if !slices.Contains(result.Metadata.AppliedOverlays, name) {
				t.Fatalf("%s: criteria resolved to %v, which does not include it", name, result.Metadata.AppliedOverlays)
			}

			hasNFD := nfdPubEnabled(result.ComponentRefs, nfdPubNFD)
			hasNetOp := nfdPubEnabled(result.ComponentRefs, nfdPubNetworkOperator)

			var patterns []string
			var rules []map[string]any
			if hasNFD {
				values, err := result.GetValuesForComponentWithContext(ctx, nfdPubNFD)
				if err != nil {
					t.Fatalf("%s: nfd values: %v", name, err)
				}
				var noPublish bool
				patterns, noPublish, err = nfdPubWorkerPublishing(values)
				if err != nil {
					t.Errorf("%s: nfd worker.extraArgs: %v, so nfd-worker exits at startup", name, err)
				}
				if noPublish {
					t.Errorf("%s: nfd-worker runs with noPublish (worker.config.core.noPublish, or -options or -no-publish in "+
						"worker.extraArgs), so it publishes no NodeFeature and nfd-master labels nothing from it", name)
				}
				rules = nfdPubMellanoxRules(values)
				for _, rule := range nfdPubTemplatePCIRules(values) {
					t.Errorf("%s: nfd worker.config.sources.custom rule %s has a labelsTemplate reading pci.device, which may "+
						"emit %s where invariants 1 and 2 cannot count it (NFD v0.19.0 source/custom/api/types.go:32, "+
						"pkg/apis/nfd/nodefeaturerule/rule.go:101 and :126); use static labels", name, rule, nfdPubMellanoxBare)
				}
				if whitelist, fields, ok := nfdPubPCISourceNIC(values); ok {
					t.Errorf("%s: nfd worker.config.sources.pci labels Mellanox NICs %s (effective deviceLabelFields %q, "+
						"deviceClassWhitelist %q): a second producer bypasses invariants 1 and 2, and on AKS labels the "+
						"accelerated-networking Ethernet VFs", name, nfdPubMellanoxLabel, fields, whitelist)
				}
				if hasNetOp {
					for _, problem := range nfdPubWorkerRuleProblems(values) {
						t.Errorf("%s: %s, so the nfd-worker rule labeling %s never runs", name, problem, nfdPubMellanoxLabel)
					}
				}
			}

			switch {
			case hasNetOp && !hasNFD:
				t.Fatalf("%s ships network-operator without the nfd component, so nothing labels %s", name, nfdPubMellanoxLabel)
			case hasNetOp:
				if len(rules) != 1 {
					t.Errorf("%s ships network-operator: want exactly one nfd-worker sources.custom rule labeling %s (or %s), got %d; "+
						"set the nfd componentRef valuesFile to components/nfd/values-nvidia-nics.yaml (values-nvidia-nics-aks.yaml on AKS)",
						name, nfdPubMellanoxLabel, nfdPubMellanoxBare, len(rules))
				} else {
					for _, problem := range nfdPubNICRuleProblems(rules[0], criteria.Service == CriteriaServiceAKS) {
						t.Errorf("%s: %s", name, problem)
					}
				}
			default:
				if len(rules) != 0 {
					t.Errorf("%s has no network-operator but nfd-worker labels %s (%d rules): the GPU Operator validator would wait "+
						"for a MOFED driver nothing installs", name, nfdPubMellanoxLabel, len(rules))
				}
			}

			if hasNFD && !slices.Equal(patterns, nfdPubStripList) {
				file := "recipes/components/nfd/values.yaml"
				if ref, _ := findComponentRefByName(result.ComponentRefs, nfdPubNFD); ref.ValuesFile != "" && ref.ValuesFile != "components/nfd/values.yaml" {
					file += ", or recipes/" + ref.ValuesFile + " if its list replaced it"
				}
				t.Errorf("%s: nfd-worker strips %q (worker.config.core.noPublishFeatures, then -options and "+
					"-no-publish-features in worker.extraArgs), want %q (nfdPubStripList); change %s",
					name, patterns, nfdPubStripList, file)
			}

			if hasNetOp {
				values, err := result.GetValuesForComponentWithContext(ctx, nfdPubNetworkOperator)
				if err != nil {
					t.Fatalf("%s: %s values: %v", name, nfdPubNetworkOperator, err)
				}
				chartRule := nfdPubChartRules[slices.IndexFunc(nfdPubChartRules, func(cr nfdPubChartRule) bool {
					return cr.component == nfdPubNetworkOperator
				})]
				ref, _ := findComponentRefByName(result.ComponentRefs, nfdPubNetworkOperator)
				aks := criteria.Service == CriteriaServiceAKS
				if !aks && !chartRule.on(values) {
					t.Errorf("%s: network-operator nfd.deployNodeFeatureRules is false: this release is the expand phase of "+
						"#3038, and removing the chart NIC rule before the contract change reopens the upgrade race "+
						"(see nfdPubTransitional)", name)
				}
				if aks && !slices.Contains(ref.ManifestFiles, nfdPubAKSRuleManifest) {
					t.Errorf("%s: network-operator manifestFiles lacks %s: this release is the expand phase of #3038, and "+
						"removing the rule before the contract change reopens the upgrade race (see nfdPubTransitional)",
						name, nfdPubAKSRuleManifest)
				}
				if aks && chartRule.on(values) {
					t.Errorf("%s: network-operator nfd.deployNodeFeatureRules is true on AKS, where its class 0200/0207 rule "+
						"also labels the accelerated-networking Ethernet VFs; AKS uses %s instead", name, nfdPubAKSRuleManifest)
				}
			}

			if len(patterns) == 0 {
				return
			}
			for _, cr := range nfdPubChartRules {
				if !nfdPubEnabled(result.ComponentRefs, cr.component) {
					continue
				}
				values, err := result.GetValuesForComponentWithContext(ctx, cr.component)
				if err != nil {
					t.Fatalf("%s: %s values: %v", name, cr.component, err)
				}
				if !cr.on(values) || !nfdPubStripped(cr.feature, patterns) {
					continue
				}
				if key := cr.component + " " + strings.Join(cr.enablePath, "."); hasNetOp && nfdPubTransitional[key] == cr.feature {
					transitionalUsed[key] = true
					continue
				}
				t.Errorf("%s: %s renders a NodeFeatureRule (%s) reading %s, which nfd-worker strips",
					name, cr.component, strings.Join(cr.enablePath, "."), cr.feature)
			}
			for _, ref := range result.ComponentRefs {
				if !ref.IsEnabled() {
					continue
				}
				for _, p := range slices.Concat(ref.PreManifestFiles, ref.ManifestFiles) {
					content, err := GetManifestContentWithContext(ctx, nil, p)
					if err != nil {
						t.Fatalf("%s: read %s: %v", name, p, err)
					}
					features, err := nfdPubManifestRuleFeatures(string(content))
					if err != nil {
						t.Errorf("%s: manifest %s does not decode as YAML once its template actions are replaced: %v", name, p, err)
					}
					for _, f := range features {
						if strings.Contains(f, nfdPubTemplatePlaceholder) {
							t.Errorf("%s: manifest %s has a NodeFeatureRule or NodeFeatureGroup feature set by a template action (%q): "+
								"the guard cannot see a templated feature; write it literally", name, p, f)
							continue
						}
						if !nfdPubStripped(f, patterns) {
							continue
						}
						if hasNetOp && nfdPubTransitional[p] == f {
							transitionalUsed[p] = true
							continue
						}
						t.Errorf("%s: manifest %s has a NodeFeatureRule or NodeFeatureGroup reading %s, which nfd-worker strips", name, p, f)
					}
				}
			}
		})
	}
	if leaves == 0 {
		t.Fatal("no leaf overlays found; the overlay walker is broken")
	}
	if ran != leaves {
		t.Logf("%d of %d leaves ran; the unused nfdPubTransitional entry check needs them all", ran, leaves)
		return
	}
	for _, key := range slices.Sorted(maps.Keys(nfdPubTransitional)) {
		if !transitionalUsed[key] {
			t.Errorf("nfdPubTransitional[%q] matched no rendered rule on any leaf: delete the entry in the contract change of #3038", key)
		}
	}
}

// TestNFDPubStripped pins the pattern semantics to nfd-worker v0.19.0
// patternMatches: exact key, or prefix when the pattern ends in "*".
func TestNFDPubStripped(t *testing.T) {
	tests := []struct {
		name     string
		feature  string
		patterns []string
		want     bool
	}{
		{"exact match", "kernel.config", []string{"kernel.config"}, true},
		{"exact pattern does not prefix-match", "kernel.configs", []string{"kernel.config"}, false},
		{"prefix pattern", "pci.device", []string{"pci.*"}, true},
		{"sibling key kept", "kernel.loadedmodule", []string{"kernel.config", "kernel.enabledmodule"}, false},
		{"no patterns", "pci.device", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nfdPubStripped(tt.feature, tt.patterns); got != tt.want {
				t.Errorf("nfdPubStripped(%q, %v) = %v, want %v", tt.feature, tt.patterns, got, tt.want)
			}
		})
	}
}

// TestNFDPubWorkerPublishing pins the effective strip list and noPublish
// state to the nfd-worker v0.19.0 override order: config file, then -options,
// then the -no-publish-features and -no-publish flags.
func TestNFDPubWorkerPublishing(t *testing.T) {
	tests := []struct {
		name          string
		worker        string
		wantStrip     []string
		wantNoPublish bool
		wantErr       bool
	}{
		{"config list", `{config: {core: {noPublishFeatures: [pci.*, usb.*]}}}`, []string{"pci.*", "usb.*"}, false, false},
		{"nothing set", `{}`, nil, false, false},
		{"flag replaces config", `{config: {core: {noPublishFeatures: [pci.*]}}, extraArgs: ["-no-publish-features=pci.device"]}`,
			[]string{"pci.device"}, false, false},
		{"two-token double-dash flag, entries trimmed", `{extraArgs: ["--no-publish-features", "pci.*, kernel.config"]}`,
			[]string{"pci.*", "kernel.config"}, false, false},
		{"empty flag value strips nothing", `{config: {core: {noPublishFeatures: [pci.*]}}, extraArgs: ["-no-publish-features="]}`,
			nil, false, false},
		{"last flag wins", `{extraArgs: ["-no-publish-features=a", "-no-publish-features=b"]}`, []string{"b"}, false, false},
		{"options replace config", `{config: {core: {noPublishFeatures: [pci.*]}}, extraArgs: ["-options={core: {noPublishFeatures: [usb.*]}}"]}`,
			[]string{"usb.*"}, false, false},
		{"options without the key keep config", `{config: {core: {noPublishFeatures: [pci.*]}}, extraArgs: ["-options={core: {sleepInterval: 30s}}"]}`,
			[]string{"pci.*"}, false, false},
		{"flag beats options in either order", `{extraArgs: ["-no-publish-features=a", "-options={core: {noPublishFeatures: [b]}}"]}`,
			[]string{"a"}, false, false},
		{"non-boolean flag consumes the next token", `{config: {core: {noPublishFeatures: [pci.*]}}, extraArgs: ["-feature-sources", "-no-publish-features=x"]}`,
			[]string{"pci.*"}, false, false},
		{"unrelated flags", `{config: {core: {noPublishFeatures: [pci.*]}}, extraArgs: ["-v=3", "--port=8081"]}`,
			[]string{"pci.*"}, false, false},
		{"config noPublish", `{config: {core: {noPublish: true}}}`, nil, true, false},
		{"bare -no-publish", `{extraArgs: ["-no-publish"]}`, nil, true, false},
		{"--no-publish=true", `{extraArgs: ["--no-publish=true"]}`, nil, true, false},
		{"-no-publish=false beats config", `{config: {core: {noPublish: true}}, extraArgs: ["-no-publish=false"]}`, nil, false, false},
		{"options noPublish, two-token", `{extraArgs: ["-options", "{core: {noPublish: true}}"]}`, nil, true, false},
		{"options that do not parse", `{extraArgs: ["-options={core: ["]}`, nil, false, true},
		{"-no-publish value that does not parse", `{extraArgs: ["-no-publish=maybe"]}`, nil, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			strip, noPublish, err := nfdPubWorkerPublishing(nfdPubValues(t, "worker: "+tt.worker))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(strip, tt.wantStrip) || noPublish != tt.wantNoPublish {
				t.Errorf("got strip %q noPublish %v, want strip %q noPublish %v", strip, noPublish, tt.wantStrip, tt.wantNoPublish)
			}
		})
	}
}

// TestNFDPubPCISourceNIC pins the sources.pci replay to source/pci/pci.go.
func TestNFDPubPCISourceNIC(t *testing.T) {
	tests := []struct {
		name       string
		pci        string
		wantFields []string
		want       bool
	}{
		{"defaults", `{}`, []string{"class", "vendor"}, false},
		{"vendor field, whitelist 02", `{deviceLabelFields: [vendor], deviceClassWhitelist: ["02"]}`, []string{"vendor"}, true},
		{"class and vendor, whitelist 02", `{deviceLabelFields: [class, vendor], deviceClassWhitelist: ["02"]}`, []string{"class", "vendor"}, false},
		{"vendor field, default whitelist", `{deviceLabelFields: [vendor]}`, []string{"vendor"}, false},
		{"unknown field dropped", `{deviceLabelFields: [bogus, vendor], deviceClassWhitelist: ["0207"]}`, []string{"vendor"}, true},
		{"only unknown fields fall back to defaults", `{deviceLabelFields: [bogus], deviceClassWhitelist: ["02"]}`, []string{"class", "vendor"}, false},
		{"fields kept in mandatory order", `{deviceLabelFields: [device, vendor], deviceClassWhitelist: ["02"]}`, []string{"vendor", "device"}, false},
		{"whitelist matches neither NIC class", `{deviceLabelFields: [vendor], deviceClassWhitelist: ["0201", "03"]}`, []string{"vendor"}, false},
		{"empty whitelist", `{deviceLabelFields: [vendor], deviceClassWhitelist: []}`, []string{"vendor"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, fields, got := nfdPubPCISourceNIC(nfdPubValues(t, "worker: {config: {sources: {pci: "+tt.pci+"}}}"))
			if got != tt.want || !slices.Equal(fields, tt.wantFields) {
				t.Errorf("got labels %v fields %q, want labels %v fields %q", got, fields, tt.want, tt.wantFields)
			}
		})
	}
}

// TestNFDPubWorkerRuleProblems pins which merged nfd values stop the
// nfd-worker sources.custom rule from labeling a node.
func TestNFDPubWorkerRuleProblems(t *testing.T) {
	tests := []struct {
		name   string
		worker string
		want   []string
	}{
		{"nothing set", `{}`, nil},
		{"labelSources all then -custom", `{config: {core: {labelSources: [all, -custom]}}}`,
			[]string{"nfd worker.config.core.labelSources [all -custom] disables the custom label source"}},
		{"labelSources -custom then all", `{config: {core: {labelSources: [-custom, all]}}}`, nil},
		{"featureSources -pci then all", `{config: {core: {featureSources: [-pci, all]}}}`, nil},
		{"featureSources all then -pci", `{config: {core: {featureSources: [all, -pci]}}}`,
			[]string{"nfd worker.config.core.featureSources [all -pci] disables the pci feature source"}},
		{"deprecated core.sources", `{config: {core: {sources: [custom]}}}`,
			[]string{"nfd worker.config.core.sources is [custom] (deprecated; it replaces labelSources)"}},
		{"label whitelist", `{config: {core: {labelWhiteList: "^foo"}}}`,
			[]string{`nfd worker.config.core.labelWhiteList is "^foo"`}},
		{"empty label whitelist", `{config: {core: {labelWhiteList: ""}}}`, nil},
		{"two-token -feature-sources", `{extraArgs: ["-feature-sources", "pci"]}`,
			[]string{`nfd worker.extraArgs passes "-feature-sources", which overrides the config file`}},
		{"double-dash -label-sources", `{extraArgs: ["--label-sources=all"]}`,
			[]string{`nfd worker.extraArgs passes "--label-sources=all", which overrides the config file`}},
		{"unrelated flag", `{extraArgs: ["-v=3"]}`, nil},
		{"value token without a dash", `{extraArgs: ["feature-sources"]}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nfdPubWorkerRuleProblems(nfdPubValues(t, "worker: "+tt.worker))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNFDPubSourceEnabled pins source-list processing to nfd-worker.go
// configureCore.
func TestNFDPubSourceEnabled(t *testing.T) {
	tests := []struct {
		name string
		list []string
		want bool
	}{
		{"all", []string{"all"}, true},
		{"all then -custom", []string{"all", "-custom"}, false},
		{"-custom then all", []string{"-custom", "all"}, true},
		{"custom then -custom", []string{"custom", "-custom"}, false},
		{"named alone", []string{"custom"}, true},
		{"another source only", []string{"pci"}, false},
		{"empty list", []string{}, false},
		{"-all is an unknown source", []string{"all", "-all"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nfdPubSourceEnabled(tt.list, "custom"); got != tt.want {
				t.Errorf("nfdPubSourceEnabled(%q, custom) = %v, want %v", tt.list, got, tt.want)
			}
		})
	}
}

// TestNFDPubNICRuleProblems pins the values-nvidia-nics*.yaml rule contract.
func TestNFDPubNICRuleProblems(t *testing.T) {
	const (
		generic = `{name: nics, labels: {feature.node.kubernetes.io/pci-15b3.present: "true"}, matchFeatures: [{feature: pci.device, ` +
			`matchExpressions: {vendor: {op: In, value: ["15b3"]}, class: {op: In, value: ["0200", "0207"]}}}]}`
		aksRule = `{name: nics, labels: {pci-15b3.present: "true"}, matchFeatures: [{feature: pci.device, ` +
			`matchExpressions: {vendor: {op: In, value: ["15b3"]}, device: {op: In, value: ["101c", "101e"]}}}]}`
	)
	tests := []struct {
		name string
		rule string
		aks  bool
		want []string
	}{
		{"generic rule", generic, false, nil},
		{"AKS rule", aksRule, true, nil},
		{"generic rule on AKS", generic, true, []string{
			`rule nics pci.device device: got no expression, want op "In" value ["101c" "101e"]`,
			`rule nics pci.device class: got an expression, want none (aks=true)`,
		}},
		{"AKS rule elsewhere", aksRule, false, []string{
			`rule nics pci.device class: got no expression, want op "In" value ["0200" "0207"]`,
			`rule nics pci.device device: got an expression, want none (aks=false)`,
		}},
		{"boolean label value", `{name: nics, labels: {pci-15b3.present: true}, matchFeatures: [{feature: pci.device, ` +
			`matchExpressions: {vendor: {op: In, value: ["15b3"]}, class: {op: In, value: ["0200", "0207"]}}}]}`, false, []string{
			`rule nics label pci-15b3.present: got true, want the string "true"`,
		}},
		{"one class only", `{name: nics, labels: {pci-15b3.present: "true"}, matchFeatures: [{feature: pci.device, ` +
			`matchExpressions: {vendor: {op: In, value: ["15b3"]}, class: {op: In, value: ["0200"]}}}]}`, false, []string{
			`rule nics pci.device class: got op "In" value ["0200"], want op "In" value ["0200" "0207"]`,
		}},
		{"NotIn vendor, integer class", `{name: nics, labels: {pci-15b3.present: "true"}, matchFeatures: [{feature: pci.device, ` +
			`matchExpressions: {vendor: {op: NotIn, value: ["15b3"]}, class: {op: In, value: [200, "0207"]}}}]}`, false, []string{
			`rule nics pci.device vendor: got op "NotIn" value ["15b3"], want op "In" value ["15b3"]`,
			`rule nics pci.device class: got op "In" value ["int(200)" "0207"], want op "In" value ["0200" "0207"]`,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := nfdPubValues(t, "rule: "+tt.rule)["rule"].(map[string]any)
			got := nfdPubNICRuleProblems(rule, tt.aks)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNFDPubTemplatePCIRules pins which sources.custom rules a labelsTemplate
// makes opaque to the producer count.
func TestNFDPubTemplatePCIRules(t *testing.T) {
	tests := []struct {
		name   string
		custom string
		want   []string
	}{
		{"template reading pci.device", `[{name: t, labelsTemplate: "{{ range .pci.device }}pci-{{ .vendor }}.present=true{{ end }}", ` +
			`matchFeatures: [{feature: pci.device, matchExpressions: {vendor: {op: In, value: ["15b3"]}}}]}]`, []string{"t"}},
		{"template reading pci.device under matchAny", `[{name: any, labelsTemplate: "x=y", ` +
			`matchAny: [{matchFeatures: [{feature: kernel.loadedmodule}]}, {matchFeatures: [{feature: pci.device}]}]}]`, []string{"any"}},
		{"template reading another feature", `[{name: mod, labelsTemplate: "x=y", matchFeatures: [{feature: kernel.loadedmodule}]}]`, nil},
		{"static labels reading pci.device", `[{name: static, labels: {pci-15b3.present: "true"}, matchFeatures: [{feature: pci.device}]}]`, nil},
		{"empty template", `[{name: empty, labelsTemplate: "", matchFeatures: [{feature: pci.device}]}]`, nil},
		{"two rules, one flagged", `[{name: a, labelsTemplate: "x=y", matchFeatures: [{feature: usb.device}]}, ` +
			`{name: b, labelsTemplate: "x=y", matchFeatures: [{feature: pci.device}]}]`, []string{"b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nfdPubTemplatePCIRules(nfdPubValues(t, "worker: {config: {sources: {custom: "+tt.custom+"}}}"))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNFDPubManifestRuleFeatures pins the manifest feature extractor: YAML
// parsing of Helm-templated NodeFeatureRule and NodeFeatureGroup documents.
func TestNFDPubManifestRuleFeatures(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
		wantErr bool
	}{
		{"trailing comment", `
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeatureRule
spec:
  rules:
  - name: r
    matchFeatures:
    - feature: kernel.config  # IB VF
`, []string{"kernel.config"}, false},
		{"flow style", `
kind: NodeFeatureRule
spec:
  rules:
  - name: r
    matchFeatures:
    - {feature: kernel.config, matchExpressions: {X: {op: Exists}}}
`, []string{"kernel.config"}, false},
		{"Helm template actions", `
# comment document
---
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeatureRule
metadata:
  name: r
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | trunc 63 }}
    {{- with .Values.extraLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  rules:
  - name: "{{ .Release.Name }}-r"
    matchFeatures:
    - feature: pci.device
`, []string{"pci.device"}, false},
		{"matchAny and a second document", `
kind: NodeFeatureRule
spec:
  rules:
  - name: r
    matchAny:
    - matchFeatures:
      - feature: kernel.loadedmodule
    - matchFeatures:
      - feature: usb.device
---
kind: NodeFeatureRule
spec:
  rules:
  - name: s
    matchFeatures:
    - feature: cpu.model
`, []string{"kernel.loadedmodule", "usb.device", "cpu.model"}, false},
		{"NodeFeatureGroup", `
kind: NodeFeatureGroup
spec:
  featureGroupRules:
  - name: g
    matchFeatures:
    - feature: system.name
    matchAny:
    - matchFeatures:
      - feature: memory.numa
`, []string{"system.name", "memory.numa"}, false},
		{"other kinds ignored", `
kind: ConfigMap
data:
  rules: |
    - feature: pci.device
spec:
  rules:
  - matchFeatures:
    - feature: pci.device
`, nil, false},
		{"if/else branches in another kind", `
kind: DaemonSet
spec:
  template:
    spec:
      {{- if .Values.gpu }}
      tolerations: [{operator: Exists}]
      {{- else }}
      tolerations: []
      {{- end }}
`, nil, false},
		{"duplicate key in a rule", `
kind: NodeFeatureRule
spec:
  rules:
  - name: r
    {{- if .Values.a }}
    matchFeatures: [{feature: pci.device}]
    {{- else }}
    matchFeatures: [{feature: usb.device}]
    {{- end }}
`, nil, true},
		{"single-line conditional", `
kind: NodeFeatureRule
spec:
  rules:
  - name: r
    matchFeatures:
    {{ if .Values.ib }}- feature: kernel.config{{ end }}
`, []string{"kernel.configAICRTEMPLATEACTION"}, false},
		{"templated feature", `
kind: NodeFeatureRule
spec:
  rules:
  - name: r
    matchFeatures:
    - feature: {{ .Values.f }}
`, []string{"AICRTEMPLATEACTION"}, false},
		{"literal feature", `
kind: NodeFeatureRule
spec:
  rules:
  - name: r
    matchFeatures:
    - feature: kernel.config
`, []string{"kernel.config"}, false},
		{"does not decode", `
kind: NodeFeatureRule
spec:
  rules:
  - name: r
    matchFeatures: [
`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nfdPubManifestRuleFeatures(tt.content)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func nfdPubValues(t *testing.T, doc string) map[string]any {
	t.Helper()
	var values map[string]any
	if err := yaml.Unmarshal([]byte(doc), &values); err != nil {
		t.Fatalf("test values %q: %v", doc, err)
	}
	return values
}

// on reports whether the chart renders the rule under the merged values.
func (cr nfdPubChartRule) on(values map[string]any) bool {
	if v, ok := nfdPubLookup(values, cr.enablePath...).(bool); ok {
		return v
	}
	return cr.chartDefault
}

func nfdPubEnabled(refs []ComponentRef, name string) bool {
	ref, ok := findComponentRefByName(refs, name)
	return ok && ref.IsEnabled()
}

func nfdPubLookup(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func nfdPubStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func nfdPubStripped(feature string, patterns []string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(feature, prefix) {
				return true
			}
		} else if feature == p {
			return true
		}
	}
	return false
}

func nfdPubMellanoxRules(values map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := nfdPubLookup(values, "worker", "config", "sources", "custom").([]any)
	for _, e := range list {
		rule, ok := e.(map[string]any)
		if !ok {
			continue
		}
		labels, _ := rule["labels"].(map[string]any)
		_, prefixed := labels[nfdPubMellanoxLabel]
		_, bare := labels[nfdPubMellanoxBare]
		if prefixed || bare {
			out = append(out, rule)
		}
	}
	return out
}

// nfdPubPCISourceNIC returns the effective nfd-worker sources.pci lists and
// whether they label a Mellanox NIC (class 0200 or 0207) pci-15b3.present.
// Per NFD v0.19.0 source/pci/pci.go: the defaults are whitelist
// ["03","0b40","12"] and fields ["class","vendor"] (:46-51); configured fields
// are kept in mandatoryDevAttrs order (utils.go:31) with unknown names dropped,
// falling back to the defaults when none is left (:95-113); a device is
// labeled when a whitelist entry prefixes its 4-digit class, and the label
// joins the fields with "_" (:116-128), so only ["vendor"] yields 15b3.present.
func nfdPubPCISourceNIC(values map[string]any) (whitelist, fields []string, labels bool) {
	pci, _ := nfdPubLookup(values, "worker", "config", "sources", "pci").(map[string]any)
	whitelist = []string{"03", "0b40", "12"}
	if v := pci["deviceClassWhitelist"]; v != nil {
		whitelist = nfdPubStrings(v)
	}
	configured := []string{"class", "vendor"}
	if v := pci["deviceLabelFields"]; v != nil {
		configured = nfdPubStrings(v)
	}
	for _, attr := range []string{"class", "vendor", "device", "subsystem_vendor", "subsystem_device"} {
		if slices.Contains(configured, attr) {
			fields = append(fields, attr)
		}
	}
	if len(fields) == 0 {
		fields = []string{"class", "vendor"}
	}
	if !slices.Equal(fields, []string{"vendor"}) {
		return whitelist, fields, false
	}
	for _, w := range whitelist {
		for _, class := range []string{"0200", "0207"} {
			if strings.HasPrefix(class, strings.ToLower(w)) {
				return whitelist, fields, true
			}
		}
	}
	return whitelist, fields, false
}

// nfdPubWorkerRuleProblems lists the merged nfd values that would keep the
// nfd-worker sources.custom rule from labeling a node. Per NFD v0.19.0
// pkg/nfd-worker/nfd-worker.go: labelSources and featureSources default to
// ["all"] (:302-303), the custom label source reads features only from
// enabled feature sources (:340, source/source.go:182), the deprecated
// core.sources replaces labelSources (:734-736), and labelWhiteList filters
// every label source (:792-836). The worker flags -label-sources,
// -feature-sources and -options override the config file (:672-691,
// :744-764); cmd/nfd-worker/main.go:106-148 has no -label-whitelist flag.
func nfdPubWorkerRuleProblems(values map[string]any) []string {
	var problems []string
	core, _ := nfdPubLookup(values, "worker", "config", "core").(map[string]any)
	if v := core["sources"]; v != nil {
		problems = append(problems, fmt.Sprintf("nfd worker.config.core.sources is %v (deprecated; it replaces labelSources)", v))
	}
	if v := core["labelSources"]; v != nil && !nfdPubSourceEnabled(nfdPubStrings(v), "custom") {
		problems = append(problems, fmt.Sprintf("nfd worker.config.core.labelSources %v disables the custom label source", v))
	}
	if v := core["featureSources"]; v != nil && !nfdPubSourceEnabled(nfdPubStrings(v), "pci") {
		problems = append(problems, fmt.Sprintf("nfd worker.config.core.featureSources %v disables the pci feature source", v))
	}
	if v := core["labelWhiteList"]; v != nil && v != "" {
		problems = append(problems, fmt.Sprintf("nfd worker.config.core.labelWhiteList is %#v", v))
	}
	args, _ := nfdPubLookup(values, "worker", "extraArgs").([]any)
	for _, a := range args {
		s, _ := a.(string)
		if !strings.HasPrefix(s, "-") {
			continue
		}
		for _, flag := range []string{"label-sources", "feature-sources", "options"} {
			if strings.HasPrefix(strings.TrimLeft(s, "-"), flag) {
				problems = append(problems, fmt.Sprintf("nfd worker.extraArgs passes %q, which overrides the config file", s))
			}
		}
	}
	return problems
}

// nfdPubSourceEnabled replays nfd-worker v0.19.0 source-list processing
// (nfd-worker.go:572-598, :604-630): entries apply in order, "all" enables
// every source (none but the fake source is off by default), "-name"
// disables one.
func nfdPubSourceEnabled(list []string, name string) bool {
	on := false
	for _, s := range list {
		switch s {
		case "all", name:
			on = true
		case "-" + name:
			on = false
		}
	}
	return on
}

// nfdPubNICRuleProblems compares a pci-15b3.present rule against the
// values-nvidia-nics*.yaml contract. On AKS a class match would also label
// the Mellanox Ethernet VFs that Azure accelerated networking attaches, so
// AKS matches the InfiniBand VF device IDs instead.
func nfdPubNICRuleProblems(rule map[string]any, aks bool) []string {
	var problems []string
	labels, _ := rule["labels"].(map[string]any)
	for _, key := range []string{nfdPubMellanoxLabel, nfdPubMellanoxBare} {
		if v, ok := labels[key]; ok && v != "true" {
			problems = append(problems, fmt.Sprintf("rule %v label %s: got %#v, want the string \"true\"", rule["name"], key, v))
		}
	}

	exprs := nfdPubPCIExpressions(rule)
	want := []struct {
		key    string
		values []string
	}{
		{"vendor", []string{"15b3"}},
		{"class", []string{"0200", "0207"}},
	}
	forbidden := "device"
	if aks {
		want[1].key, want[1].values = "device", []string{"101c", "101e"}
		forbidden = "class"
	}
	for _, w := range want {
		op, values, ok := nfdPubExpression(exprs, w.key)
		if !ok {
			problems = append(problems, fmt.Sprintf("rule %v pci.device %s: got no expression, want op \"In\" value %q",
				rule["name"], w.key, w.values))
			continue
		}
		if op != "In" || !slices.Equal(values, w.values) {
			problems = append(problems, fmt.Sprintf("rule %v pci.device %s: got op %q value %q, want op \"In\" value %q",
				rule["name"], w.key, op, values, w.values))
		}
	}
	if _, ok := exprs[forbidden]; ok {
		problems = append(problems, fmt.Sprintf("rule %v pci.device %s: got an expression, want none (aks=%v)",
			rule["name"], forbidden, aks))
	}
	return problems
}

// nfdPubExpression decodes one matchExpressions entry, {op: In, value: [...]}.
// A non-string value element is rendered with its type so it never equals a
// wanted ID.
func nfdPubExpression(exprs map[string]any, key string) (string, []string, bool) {
	e, ok := exprs[key].(map[string]any)
	if !ok {
		return "", nil, false
	}
	op, _ := e["op"].(string)
	list, _ := e["value"].([]any)
	values := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			values = append(values, s)
		} else {
			values = append(values, fmt.Sprintf("%T(%v)", v, v))
		}
	}
	return op, values, true
}

func nfdPubPCIExpressions(rule map[string]any) map[string]any {
	matchers, _ := rule["matchFeatures"].([]any)
	for _, m := range matchers {
		mm, ok := m.(map[string]any)
		if !ok || mm["feature"] != "pci.device" {
			continue
		}
		exprs, _ := mm["matchExpressions"].(map[string]any)
		return exprs
	}
	return nil
}

// nfdPubWorkerPublishing returns the noPublishFeatures list and noPublish
// state nfd-worker v0.19.0 runs with. Per pkg/nfd-worker/nfd-worker.go
// configure (:672-711), the config file is read first, the -options YAML is
// unmarshaled over it (:687), and applyConfigOverrides then applies -no-publish
// (:745-747) and -no-publish-features (:761-763). The flag value is split as
// utils.StringSliceVal.Set does (pkg/utils/flags.go:87-95). A value that does
// not parse is an error: configure returns it and Run exits (:487-490).
func nfdPubWorkerPublishing(values map[string]any) (strip []string, noPublish bool, err error) {
	core, _ := nfdPubLookup(values, "worker", "config", "core").(map[string]any)
	strip = nfdPubStrings(core["noPublishFeatures"])
	noPublish, _ = core["noPublish"].(bool)
	flags := nfdPubWorkerFlags(nfdPubStrings(nfdPubLookup(values, "worker", "extraArgs")))
	if v, ok := flags["options"]; ok {
		var options map[string]any
		if err = yaml.Unmarshal([]byte(v), &options); err != nil {
			return nil, false, fmt.Errorf("-options %q does not parse: %w", v, err)
		}
		optCore, _ := options["core"].(map[string]any)
		if v, ok := optCore["noPublishFeatures"]; ok {
			strip = nfdPubStrings(v)
		}
		if v, ok := optCore["noPublish"]; ok {
			noPublish, _ = v.(bool)
		}
	}
	if v, ok := flags["no-publish-features"]; ok {
		strip = nil
		for s := range strings.SplitSeq(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				strip = append(strip, s)
			}
		}
	}
	if v, ok := flags["no-publish"]; ok {
		if noPublish, err = strconv.ParseBool(v); err != nil {
			return nil, false, fmt.Errorf("-no-publish=%q does not parse: %w", v, err)
		}
	}
	return strip, noPublish, nil
}

// nfdPubWorkerFlags returns the last value given to each nfd-worker flag the
// guard reads, parsed as Go's flag package parses them (cmd/nfd-worker/main.go:78):
// one or two leading dashes, then "name=value", or "name value" for a flag
// that is not boolean. -no-publish (main.go:130) is the only boolean one, and
// alone it means true. flag.Parse stops at the first non-flag token and the
// worker then exits (main.go:79-83); this scan reads every token, so it can
// find more of these flags than the worker would, never fewer.
func nfdPubWorkerFlags(args []string) map[string]string {
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		name, ok := strings.CutPrefix(args[i], "-")
		if !ok {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(name, "-"), "=")
		switch name {
		case "no-publish":
			if !hasValue {
				value = "true"
			}
		case "options", "no-publish-features", "label-sources", "feature-sources":
			if !hasValue && i+1 < len(args) {
				i++
				value = args[i]
			}
		default:
			continue
		}
		flags[name] = value
	}
	return flags
}

// nfdPubTemplatePCIRules names the nfd-worker sources.custom rules with a
// labelsTemplate whose matchFeatures or matchAny read pci.device. NFD v0.19.0
// expands the template from the matched features into the rule's labels
// (source/custom/api/types.go:32, pkg/apis/nfd/nodefeaturerule/rule.go:101
// and :126), so such a rule may emit pci-15b3.present.
func nfdPubTemplatePCIRules(values map[string]any) []string {
	var names []string
	list, _ := nfdPubLookup(values, "worker", "config", "sources", "custom").([]any)
	for _, e := range list {
		rule, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if tmpl, _ := rule["labelsTemplate"].(string); tmpl != "" && slices.Contains(nfdPubMatcherFeatures(rule), "pci.device") {
			names = append(names, fmt.Sprint(rule["name"]))
		}
	}
	return names
}

// nfdPubMatcherFeatures lists the features a rule reads through
// matchFeatures[].feature and matchAny[].matchFeatures[].feature (NFD v0.19.0
// api/nfd/v1alpha1/types.go:248-252, :258, :270; group rules :199-203).
func nfdPubMatcherFeatures(rule map[string]any) []string {
	anyOf, _ := rule["matchAny"].([]any)
	first, _ := rule["matchFeatures"].([]any)
	matchers := append(make([][]any, 0, 1+len(anyOf)), first)
	for _, e := range anyOf {
		elem, _ := e.(map[string]any)
		terms, _ := elem["matchFeatures"].([]any)
		matchers = append(matchers, terms)
	}
	var features []string
	for _, terms := range matchers {
		for _, term := range terms {
			m, _ := term.(map[string]any)
			if f, ok := m["feature"].(string); ok {
				features = append(features, f)
			}
		}
	}
	return features
}

// nfdPubManifestRuleFeatures lists the features every NodeFeatureRule
// (spec.rules, types.go:128) and NodeFeatureGroup (spec.featureGroupRules,
// types.go:152) in a manifest reads. Manifests are Helm templates: actions
// that open a line are dropped and any other action becomes
// nfdPubTemplatePlaceholder, because a bare scalar on its own line breaks the
// enclosing mapping and one before a "- " item turns it into a key. A
// document that still does not decode is an error.
func nfdPubManifestRuleFeatures(content string) ([]string, error) {
	content = nfdPubTemplateLineRe.ReplaceAllString(content, "$1")
	content = nfdPubTemplateActionRe.ReplaceAllString(content, nfdPubTemplatePlaceholder)
	dec := yaml.NewDecoder(strings.NewReader(content))
	var features []string
	for {
		var doc struct {
			Kind string    `yaml:"kind"`
			Spec yaml.Node `yaml:"spec"`
		}
		if err := dec.Decode(&doc); stderrors.Is(err, io.EOF) {
			return features, nil
		} else if err != nil {
			return nil, err
		}
		key := map[string]string{"NodeFeatureRule": "rules", "NodeFeatureGroup": "featureGroupRules"}[doc.Kind]
		if key == "" {
			continue
		}
		var spec map[string]any
		if err := doc.Spec.Decode(&spec); err != nil {
			return nil, fmt.Errorf("%s spec: %w", doc.Kind, err)
		}
		rules, _ := spec[key].([]any)
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			features = append(features, nfdPubMatcherFeatures(rule)...)
		}
	}
}
