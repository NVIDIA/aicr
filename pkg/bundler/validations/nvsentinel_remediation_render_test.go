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

package validations

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	aicrhelm "github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// remediationDeployments are the pipeline Deployments, in pipeline order.
var remediationDeployments = []string{"fault-quarantine", "node-drainer", "fault-remediation", "janitor", "janitor-provider"}

// remediationStepRender is what each step must render. Images are the ones
// docs/user/container-images.md discloses for the step, except the reboot
// image, which is an env var and asserted by assertRenderedRebootImage.
var remediationStepRender = []struct {
	mixin       string
	dryRun      string
	deployments []string
	images      []string
	// maxAttempts is the rendered maxRemediationAttempts; empty means the
	// chart omits the key and fault-remediation retries without limit.
	maxAttempts string
}{
	{
		mixin:       "nvsentinel-observe",
		dryRun:      "true",
		deployments: []string{"fault-quarantine", "node-drainer", "fault-remediation"},
		images: append([]string{
			"ghcr.io/nvidia/nvsentinel/fault-quarantine:v1.25.0",
			"ghcr.io/nvidia/nvsentinel/node-drainer:v1.25.0",
			"ghcr.io/nvidia/nvsentinel/fault-remediation:v1.25.0",
		}, perconaSetupImage),
	},
	{
		mixin:       "nvsentinel-quarantine",
		dryRun:      "false",
		deployments: []string{"fault-quarantine", "node-drainer"},
		images: append([]string{
			"ghcr.io/nvidia/nvsentinel/fault-quarantine:v1.25.0",
			"ghcr.io/nvidia/nvsentinel/node-drainer:v1.25.0",
		}, perconaSetupImage),
	},
	{
		mixin:       "nvsentinel-remediation",
		dryRun:      "false",
		deployments: remediationDeployments,
		maxAttempts: "3",
		images: append([]string{
			"ghcr.io/nvidia/nvsentinel/fault-quarantine:v1.25.0",
			"ghcr.io/nvidia/nvsentinel/node-drainer:v1.25.0",
			"ghcr.io/nvidia/nvsentinel/fault-remediation:v1.25.0",
			"ghcr.io/nvidia/nvsentinel/janitor:v1.25.0",
			"ghcr.io/nvidia/nvsentinel/janitor-provider:v1.25.0",
			"ghcr.io/nvidia/nvsentinel/gpu-reset:v1.25.0",
		}, perconaSetupImage),
	},
}

// renderedSetupJob matches NVSentinel's external-datastore setup Job under
// aicrhelm.RenderChart's release name (release-<component>). The chart
// suffixes the name with the collection TTL and a hash of the setup script.
var renderedSetupJob = regexp.MustCompile(`^release-nvsentinel-external-mongodb-setup-[a-z0-9-]+$`)

// perconaSetupImage runs NVSentinel's external-datastore setup Job: the
// mongod image, which ships mongosh.
const perconaSetupImage = "docker.io/percona/percona-server-mongodb:8.0.17-6@sha256:8698ffa8c0a3cb1902160e75599adb9d8b2e448e922a145fcf55915ef2fd556a"

// perconaImages are every image the separately bundled datastore renders,
// pinned in recipes/components/{psmdb-operator,nvsentinel-mongodb}.
var perconaImages = []string{
	"docker.io/percona/percona-server-mongodb-operator:1.21.2@sha256:4f8be902b46ae8375e852aa37e384d0f68dcc9f00c0ebde0485d59d535b408d3",
	perconaSetupImage,
	"docker.io/percona/mongodb_exporter:0.40.0@sha256:d66daa6aff0513860d1577cee3b55ab82fde43394f8319d7b4674411b9153cce",
	"docker.io/percona/percona-backup-mongodb:2.11.0@sha256:4e3156800f08b8cfab8086cc41667a697afeb3166db242b3fe6317c8b2288da9",
	"docker.io/percona/fluentbit:4.0.1@sha256:dd584776ba987d77c5d1848ad98d31ae807c3781bc78b305ffc4088a4575fbae",
	"docker.io/percona/pmm-client:3.5.0@sha256:82b36789edc633ea97a0f2433dd9ef472e11811d1798de01fb700b78455059e1",
}

// genericRebootImage is the image janitor-provider's generic reboot Job runs.
// The chart passes it as an env var, which aicr mirror cannot discover.
const genericRebootImage = "docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e"

// chartDefaultRuleSets are fault-quarantine's rule sets at chart v1.25.0, in
// evaluation order. AICR overrides none of them.
var chartDefaultRuleSets = []string{
	"GPU fatal error ruleset",
	"CSP health monitor fatal error ruleset",
	"Syslog fatal error ruleset",
	"NIC health monitor fatal error ruleset",
	"Kubernetes object monitor fatal error ruleset",
	"NVCRE certification monitor failure ruleset",
	"Preflight check fatal error ruleset",
	"MaintenanceRequest ruleset",
}

// taintOnlyRuleSets quarantine with a NoSchedule taint instead of a cordon:
// NVCRE skips a cordoned node, so a cordon would block re-certification.
var taintOnlyRuleSets = map[string]string{
	"NVCRE certification monitor failure ruleset": "nvsentinel.dgxc.nvidia.com/nvcre-cert-failed",
}

// remediationActions are the fault-remediation actions AICR relies on:
// COMPONENT_RESET as AICR overrides it, the others as the chart ships them.
var remediationActions = []struct {
	action   string
	kind     string
	template string
	lines    []string
	impact   string
}{
	{
		action:   "COMPONENT_RESET",
		kind:     "GPUReset",
		template: "gpureset",
		lines: []string{
			`impactedEntityScope = "GPU_UUID"`, `completeConditionType = "Complete"`,
			`equivalenceGroup = "reset"`, `supersedingEquivalenceGroups = ["restart"]`,
		},
		impact: "a recoverable GPU fault would not resolve to an in-place GPU reset",
	},
	{
		action:   "RESTART_VM",
		kind:     "RebootNode",
		template: "nvidia-reboot.yaml",
		lines:    []string{`completeConditionType = "NodeReady"`},
		impact:   "a node-restart fault would not reboot the node",
	},
	{
		action:   "RESTART_BM",
		kind:     "RebootNode",
		template: "nvidia-reboot.yaml",
		lines:    []string{`completeConditionType = "NodeReady"`},
		impact:   "a bare-metal restart fault would not reboot the node",
	},
	{
		action:   "REPLACE_VM",
		kind:     "TerminateNode",
		template: "terminate-node.yaml",
		lines:    []string{`completeConditionType = "NodeTerminated"`},
		impact:   "a node-replace fault would not terminate the node",
	},
}

var (
	ruleSetName          = regexp.MustCompile(`(?m)^\s*name = "([^"]*)"`)
	drainerUserNamespace = regexp.MustCompile(`\[\[userNamespaces\]\]\s*name = "([^"]*)"\s*mode = "([^"]*)"`)
)

// remediationActionBlock returns the body of the rendered
// [remediationActions."<action>"] TOML table, up to the next table header.
func remediationActionBlock(config, action string) (string, bool) {
	m := regexp.MustCompile(`(?s)\[remediationActions\."` + regexp.QuoteMeta(action) + `"\](.*?)(?:\n\s*\[|\z)`).FindStringSubmatch(config)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// configMapFile returns the body of a block-scalar data key in a rendered
// ConfigMap.
func configMapFile(cm, key string) (string, bool) {
	m := regexp.MustCompile(`(?ms)^  ` + regexp.QuoteMeta(key) + `: \|\n(.*?)(?:^  \S|\z)`).FindStringSubmatch(cm)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// renderedDocument returns the single rendered manifest of the given kind and
// name, so an assertion cannot be satisfied by a sibling that shares
// argument names.
func renderedDocument(rendered, kind, name string) (string, bool) {
	return renderedDocumentMatching(rendered, kind, regexp.MustCompile(`^`+regexp.QuoteMeta(name)+`$`))
}

func renderedDocumentMatching(rendered, kind string, name *regexp.Regexp) (string, bool) {
	metaName := regexp.MustCompile(`(?m)^  name: (\S+)$`)
	for _, doc := range strings.Split(rendered, "\n---") {
		if !regexp.MustCompile(`(?m)^kind: ` + kind + `$`).MatchString(doc) {
			continue
		}
		for _, m := range metaName.FindAllStringSubmatch(doc, -1) {
			if name.MatchString(m[1]) {
				return doc, true
			}
		}
	}
	return "", false
}

// TestNVSentinelRemediationChartRender renders the pinned chart with each
// remediation step composed and asserts what reaches the cluster: which
// pipeline Deployments render, that each runs with the step's --dry-run, the
// fault-quarantine rule sets and node-drainer mode the steps take from the
// chart, fault-remediation's action map, janitor-provider's reboot image, and
// every image the step pulls.
//
// The values test pins the map AICR sets; this one pins what the chart builds
// from it and from its own defaults, so a chart bump that renamed a key,
// changed a default or changed how maps merge fails here instead of quietly
// changing what a fault does to a node.
//
// Pulls the chart over the network, so -short skips it; the weekly
// .github/workflows/nvsentinel-observability-render-check.yaml (NVSentinel
// Mixin Render Checks) runs it.
func TestNVSentinelRemediationChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}

	for _, step := range remediationStepRender {
		t.Run(step.mixin, func(t *testing.T) {
			out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, step.mixin))

			for _, name := range remediationDeployments {
				doc, found := renderedDocument(out, "Deployment", name)
				want := slices.Contains(step.deployments, name)
				if found != want {
					t.Errorf("Deployment %s rendered = %v, want %v", name, found, want)
					continue
				}
				if !found || name == "janitor" || name == "janitor-provider" {
					continue
				}
				if !strings.Contains(doc, `"--dry-run=`+step.dryRun+`"`) {
					t.Errorf("Deployment %s does not run with --dry-run=%s", name, step.dryRun)
				}
			}
			assertRenderedExternalDatastore(t, out)

			if slices.Contains(step.deployments, "fault-quarantine") {
				assertRenderedRuleSets(t, out)
			}
			if slices.Contains(step.deployments, "node-drainer") {
				assertRenderedDrainMode(t, out)
			}
			if slices.Contains(step.deployments, "fault-remediation") {
				assertRenderedRemediationActions(t, out)
				assertRenderedMaxRemediationAttempts(t, out, step.maxAttempts)
			}
			if slices.Contains(step.deployments, "janitor-provider") {
				assertRenderedRebootImage(t, out)
			}

			for _, image := range step.images {
				if !strings.Contains(out, image) {
					t.Errorf("render does not carry %s; docs/user/container-images.md claims %s pulls it", image, step.mixin)
				}
			}
		})
	}
}

// assertRenderedExternalDatastore checks NVSentinel is wired to the Percona
// datastore rather than its embedded store: no in-chart MongoDB, the setup
// Job present, and the pipeline reading the URI Secret and mounting the
// client certificate.
func assertRenderedExternalDatastore(t *testing.T, rendered string) {
	t.Helper()
	for _, kind := range []string{"StatefulSet", "PerconaServerMongoDB"} {
		if _, found := renderedDocument(rendered, kind, "mongodb"); found {
			t.Errorf("NVSentinel rendered its embedded %s; the steps bundle Percona separately", kind)
		}
	}
	if job, ok := renderedDocumentMatching(rendered, "Job", renderedSetupJob); !ok {
		t.Error("the external-datastore setup Job did not render; nothing creates the collections and indexes")
	} else if !strings.Contains(job, perconaSetupImage) {
		t.Errorf("the setup Job does not run %s", perconaSetupImage)
	}
	doc, ok := renderedDocument(rendered, "Deployment", "fault-quarantine")
	if !ok {
		return
	}
	for _, ref := range []string{"nvsentinel-mongodb-uri", "nvsentinel-mongodb-app-client"} {
		if !strings.Contains(doc, ref) {
			t.Errorf("fault-quarantine does not reference %s", ref)
		}
	}
}

// renderRegistryComponent renders a registry component's pinned chart with
// its AICR values file.
func renderRegistryComponent(t *testing.T, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get(name)
	if comp == nil {
		t.Fatalf("%s not found in component registry", name)
	}
	values, err := recipe.GetComponentValuesWithContext(ctx, nil,
		&recipe.ComponentRef{Name: name, ValuesFile: "components/" + name + "/values.yaml"})
	if err != nil {
		t.Fatalf("values for %s: %v", name, err)
	}
	rendered, err := aicrhelm.RenderChart(ctx, aicrhelm.ChartInput{
		Name:       name,
		Chart:      comp.Helm.DefaultChart,
		Repository: comp.Helm.DefaultRepository,
		Version:    comp.Helm.DefaultVersion,
		Namespace:  comp.Helm.DefaultNamespace,
		Values:     values,
	})
	if err != nil {
		t.Fatalf("helm template %s failed: %v\noutput:\n%s", name, err, rendered)
	}
	return string(rendered)
}

// TestNVSentinelMongoDBChartRender renders the two Percona components from
// their pins and values and asserts what NVSentinel and the operator depend
// on: a namespaced operator with telemetry off, a requireTLS replica set of
// three with the x509 app user, members spread by preference, and exactly
// the pinned images. A Percona bump that renames a key fails here.
func TestNVSentinelMongoDBChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	op := renderRegistryComponent(t, "psmdb-operator")
	dep, ok := renderedDocument(op, "Deployment", "release-psmdb-operator")
	if !ok {
		t.Fatal("psmdb-operator Deployment did not render")
	}
	for _, want := range []string{"name: WATCH_NAMESPACE", `value: "nvsentinel"`, "name: DISABLE_TELEMETRY", `value: "true"`} {
		if !strings.Contains(dep, want) {
			t.Errorf("psmdb-operator Deployment lacks %s", want)
		}
	}
	if regexp.MustCompile(`(?m)^kind: ClusterRole$`).MatchString(op) {
		t.Error("psmdb-operator renders a ClusterRole; it must watch only its own namespace")
	}

	db := renderRegistryComponent(t, "nvsentinel-mongodb")
	cr, ok := renderedDocument(db, "PerconaServerMongoDB", "nvsentinel-mongodb")
	if !ok {
		t.Fatal("PerconaServerMongoDB nvsentinel-mongodb did not render")
	}
	for _, want := range []string{
		"mode: requireTLS",
		"db: $external",
		"CN=mongo-user-client,OU=DGXC,O=Nvidia,L=SantaClara,ST=California,C=US",
		"size: 3",
		"preferredDuringSchedulingIgnoredDuringExecution",
		"role: nvsentinelTTLIndex",
		"- collMod",
		"name: nvsentinelTTLIndex",
	} {
		if !strings.Contains(cr, want) {
			t.Errorf("PerconaServerMongoDB lacks %q", want)
		}
	}
	if regexp.MustCompile(`(?m)^\s+sharding:\n\s+enabled: true`).MatchString(cr) {
		t.Error("sharding is enabled; NVSentinel is wired to the rs0 replica set")
	}
	// Helm 4's server-side apply sends a null field, and the CRD rejects it.
	if nulls := regexp.MustCompile(`(?m)^.*:\s*(?:\n\s*)?null$`).FindAllString(cr, -1); len(nulls) > 0 {
		t.Errorf("PerconaServerMongoDB renders null fields the CRD rejects: %q", nulls)
	}

	all := op + db
	for _, image := range perconaImages {
		if !strings.Contains(all, image) {
			t.Errorf("render does not carry %s; the inventory claims the datastore pulls it", image)
		}
	}
	for _, line := range regexp.MustCompile(`(?m)^\s*(?:- )?image: "?([^"\s]+)`).FindAllStringSubmatch(all, -1) {
		if !slices.Contains(perconaImages, line[1]) {
			t.Errorf("render carries %s, which is not in the pinned inventory", line[1])
		}
	}
}

func assertRenderedRuleSets(t *testing.T, rendered string) {
	t.Helper()
	cm, ok := renderedDocument(rendered, "ConfigMap", "fault-quarantine")
	if !ok {
		t.Fatal("fault-quarantine ConfigMap not rendered")
	}
	var names []string
	for _, rs := range strings.Split(cm, "[[rule-sets]]")[1:] {
		m := ruleSetName.FindStringSubmatch(rs)
		if m == nil {
			t.Errorf("rule set without a name in fault-quarantine config:\n%s", rs)
			continue
		}
		names = append(names, m[1])
		if !strings.Contains(rs, "enabled = true") {
			t.Errorf("rule set %q is not enabled; its faults would not quarantine the node", m[1])
		}
		if taint, ok := taintOnlyRuleSets[m[1]]; ok {
			if !strings.Contains(rs, "shouldCordon = false") || !strings.Contains(rs, taint) {
				t.Errorf("rule set %q does not quarantine with taint %s instead of a cordon", m[1], taint)
			}
		} else if !strings.Contains(rs, "shouldCordon = true") {
			t.Errorf("rule set %q is not set to cordon; its faults would not quarantine the node", m[1])
		}
	}
	if !slices.Equal(names, chartDefaultRuleSets) {
		t.Errorf("fault-quarantine rule sets = %q, want the chart defaults %q", names, chartDefaultRuleSets)
	}
}

func assertRenderedDrainMode(t *testing.T, rendered string) {
	t.Helper()
	cm, ok := renderedDocument(rendered, "ConfigMap", "node-drainer")
	if !ok {
		t.Fatal("node-drainer ConfigMap not rendered")
	}
	matches := drainerUserNamespace.FindAllStringSubmatch(cm, -1)
	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, m[1]+"="+m[2])
	}
	if strings.Count(cm, "[[userNamespaces]]") != 1 || len(got) != 1 || got[0] != "*=AllowCompletion" {
		t.Errorf("node-drainer userNamespaces = %q, want only *=AllowCompletion; the documented drain (wait for pods to finish, never evict) no longer holds", got)
	}
}

func assertRenderedRemediationActions(t *testing.T, rendered string) {
	t.Helper()
	cm, ok := renderedDocument(rendered, "ConfigMap", "fault-remediation")
	if !ok {
		t.Fatal("fault-remediation ConfigMap not rendered")
	}
	for _, a := range remediationActions {
		block, ok := remediationActionBlock(cm, a.action)
		if !ok {
			t.Errorf(`config.toml has no [remediationActions.%q] table; the event would be skipped as unsupported`, a.action)
			continue
		}
		want := append([]string{
			`kind = "` + a.kind + `"`,
			`templateFileName = "` + a.template + `"`,
		}, a.lines...)
		for _, line := range want {
			if !strings.Contains(block, line) {
				t.Errorf("%s table missing %s; %s", a.action, line, a.impact)
			}
		}
		tmpl, ok := configMapFile(cm, a.template)
		if !ok || !strings.Contains(tmpl, "kind: "+a.kind) {
			t.Errorf("%s with kind %s is not in the ConfigMap; fault-remediation refuses to start without the template an action names", a.template, a.kind)
		}
	}
}

func assertRenderedMaxRemediationAttempts(t *testing.T, rendered, want string) {
	t.Helper()
	cm, ok := renderedDocument(rendered, "ConfigMap", "fault-remediation")
	if !ok {
		t.Fatal("fault-remediation ConfigMap not rendered")
	}
	// The key is read only above the first [section] header; below it the
	// loader treats it as a sub-key and silently ignores it.
	top, _, _ := strings.Cut(cm, "[template]")
	m := regexp.MustCompile(`(?m)^\s*maxRemediationAttempts = (\d+)$`).FindStringSubmatch(top)
	got := ""
	if m != nil {
		got = m[1]
	}
	if got != want {
		t.Errorf("top-level maxRemediationAttempts = %q, want %q; a failing repair would not stop where the step documents", got, want)
	}
}

func assertRenderedRebootImage(t *testing.T, rendered string) {
	t.Helper()
	doc, ok := renderedDocument(rendered, "Deployment", "janitor-provider")
	if !ok {
		t.Fatal("janitor-provider Deployment not rendered")
	}
	var w k8sWorkload
	if err := yaml.Unmarshal([]byte(doc), &w); err != nil {
		t.Fatalf("decoding janitor-provider Deployment: %v", err)
	}
	for _, c := range w.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name != "GENERIC_REBOOT_IMAGE" {
				continue
			}
			if e.Value != genericRebootImage {
				t.Errorf("GENERIC_REBOOT_IMAGE = %q, want %q; the reboot Job would pull an image AICR does not document or scan", e.Value, genericRebootImage)
			}
			return
		}
	}
	t.Error("janitor-provider has no GENERIC_REBOOT_IMAGE env; the generic provider is not the one rebooting nodes")
}

// TestNVSentinelRemediationAbsentWithoutMixin is the negative control: the
// shipped shape renders none of the pipeline or its datastore.
func TestNVSentinelRemediationAbsentWithoutMixin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, ""))

	for _, name := range remediationDeployments {
		if _, found := renderedDocument(out, "Deployment", name); found {
			t.Errorf("Deployment %s rendered without a remediation mixin", name)
		}
	}
	if _, found := renderedDocumentMatching(out, "Job", renderedSetupJob); found {
		t.Error("the datastore setup Job rendered without a remediation mixin")
	}
	for _, image := range remediationStepRender[2].images {
		if strings.Contains(out, image) {
			t.Errorf("%s rendered without a remediation mixin", image)
		}
	}
}
