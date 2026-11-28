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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The vendored Kubeflow MPI Operator: its component name, the upstream release
// the manifests under mpiOperatorDir come from (bump together with a
// re-vendor), where they live, and the namespace and object name the Run:ai
// operator reads (MpiOperatorNamespace and MpiOperatorDeploymentName in
// runai-cluster 3.0.26's go-operator consts).
const (
	mpiOperatorComponent = "mpi-operator"
	mpiOperatorVersion   = "0.8.2"
	mpiOperatorDir       = "../../recipes/components/mpi-operator/manifests"
	mpiOperatorNamespace = "mpi-operator"
	mpiOperatorCRD       = "mpijobs.kubeflow.org"
	mpiOperatorCRDFile   = "kubeflow.org_mpijobs.yaml"
	mpiOperatorManifest  = "mpi-operator.yaml"
	mpiOperatorCheckFile = "../../recipes/checks/mpi-operator/health-check.yaml"
)

// mpiOperatorUpstreamCRDSHA256 is the SHA-256 of the MPIJob CRD document in
// deploy/v2beta1/mpi-operator.yaml at tag v0.8.2: the text between that file's
// "---" separators, ending in one newline. Recompute it from upstream on a
// re-vendor; never from the vendored file.
const mpiOperatorUpstreamCRDSHA256 = "90851cf759b6e94019ac6511ca8e4d1310cc4970bb8f47611c1f93de895e8b4d"

// mpiOperatorImagePattern matches the whole image line in the vendored
// Deployment: the upstream repository with its registry spelled out, a
// release-shaped tag and a digest of exactly 64 hex characters, with nothing
// after it. Unanchored, it would accept a longer, invalid digest by its
// first 64 characters.
var mpiOperatorImagePattern = regexp.MustCompile(
	`(?m)^\s*image: (docker\.io/mpioperator/mpi-operator):([0-9]+\.[0-9]+\.[0-9]+)@sha256:([0-9a-f]{64})$`)

// TestRunaiLeavesCarryMPIOperator pins what lets a Run:ai leaf run MPI
// workloads: the Kubeflow MPI Operator is present and enabled, as the vendored
// manifest-only component in the namespace the Run:ai operator reads it from,
// and ordered before runai-cluster, next to the Training Operator whose MPI v1
// scheme is off (#3302, Phase 3).
func TestRunaiLeavesCarryMPIOperator(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	cfg := registry.Get(mpiOperatorComponent)
	if cfg == nil {
		t.Fatalf("registry has no %q component", mpiOperatorComponent)
	}
	if cfg.Helm.DefaultRepository != "" || cfg.Helm.DefaultNamespace != mpiOperatorNamespace {
		t.Errorf("%s must be manifest-only (empty defaultRepository) in namespace %s, got %+v",
			mpiOperatorComponent, mpiOperatorNamespace, cfg.Helm)
	}
	want := []string{
		"components/mpi-operator/manifests/" + mpiOperatorCRDFile,
		"components/mpi-operator/manifests/" + mpiOperatorManifest,
	}
	if !slices.Equal(cfg.ManifestFiles, want) {
		t.Errorf("%s manifestFiles = %v, want %v (the CRD, then the operator)",
			mpiOperatorComponent, cfg.ManifestFiles, want)
	}
	if got, want := cfg.HealthCheck.AssertFile, "checks/mpi-operator/health-check.yaml"; got != want {
		t.Errorf("%s assertFile = %q, want %q", mpiOperatorComponent, got, want)
	}

	for leaf, criteria := range runaiLeafCriteria() {
		t.Run(leaf, func(t *testing.T) {
			t.Parallel()

			result, err := NewBuilder().BuildFromCriteria(context.Background(), criteria())
			if err != nil {
				t.Fatalf("BuildFromCriteria: %v", err)
			}
			if !slices.Contains(result.Metadata.AppliedOverlays, leaf) {
				t.Fatalf("criteria resolved to %v, which does not include %s", result.Metadata.AppliedOverlays, leaf)
			}

			ref := result.GetComponentRef(mpiOperatorComponent)
			if ref == nil {
				t.Fatalf("%s is absent from the resolved recipe", mpiOperatorComponent)
			}
			if !ref.IsEnabled() {
				t.Errorf("%s is disabled", mpiOperatorComponent)
			}
			if ref.Source != "" || ref.Version != "" {
				t.Errorf("%s resolved with source %q version %q; a vendored manifest-only component has neither",
					mpiOperatorComponent, ref.Source, ref.Version)
			}
			if ref.Namespace != mpiOperatorNamespace {
				t.Errorf("%s namespace = %q, want %s (the Run:ai operator reads the Deployment there)",
					mpiOperatorComponent, ref.Namespace, mpiOperatorNamespace)
			}
			if ref.HealthCheckAsserts == "" {
				t.Errorf("%s resolved without its health check inlined", mpiOperatorComponent)
			}
			if !slices.Equal(ref.ManifestFiles, cfg.ManifestFiles) {
				t.Errorf("%s resolved with manifestFiles %v, want the registry's %v",
					mpiOperatorComponent, ref.ManifestFiles, cfg.ManifestFiles)
			}
			// The v1 MPI scheme and CRD have to be out of the way: both
			// operators define mpijobs.kubeflow.org.
			if training := result.GetComponentRef(kubeflowTrainingOperatorComponent); training == nil || !training.IsEnabled() {
				t.Errorf("%s is not enabled next to %s; the leaves ship the two together",
					kubeflowTrainingOperatorComponent, mpiOperatorComponent)
			}

			runai := result.GetComponentRef(runaiClusterComponent)
			if runai == nil {
				t.Fatalf("%s is absent from the resolved recipe", runaiClusterComponent)
			}
			if !slices.Contains(runai.DependencyRefs, mpiOperatorComponent) {
				t.Errorf("%s dependencyRefs %v lack %s; the Run:ai operator should find the MPI Operator on its first reconcile",
					runaiClusterComponent, runai.DependencyRefs, mpiOperatorComponent)
			}
			order := result.DeploymentOrder
			mi, ri := slices.Index(order, mpiOperatorComponent), slices.Index(order, runaiClusterComponent)
			if mi < 0 || ri < 0 || mi > ri {
				t.Errorf("deploymentOrder %v does not place %s before %s", order, mpiOperatorComponent, runaiClusterComponent)
			}
		})
	}
}

// TestMPIOperatorVendoredCRD checks that the vendored CRD file defines exactly
// mpijobs.kubeflow.org serving v2beta1 alone, records the upstream tag it was
// taken from, carries the retention guards upstream does not ship and is
// otherwise upstream's text, and that the health check asserts that version.
// Training Operator v1's CRD has the same name and serves v1; a re-vendor from
// the wrong repository, or a check that asserts the name alone, would pass on
// the wrong API.
func TestMPIOperatorVendoredCRD(t *testing.T) {
	t.Parallel()

	file := filepath.Join(mpiOperatorDir, mpiOperatorCRDFile)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	if !strings.Contains(string(data), "v"+mpiOperatorVersion) {
		t.Errorf("%s does not record upstream tag v%s", file, mpiOperatorVersion)
	}

	type crdDoc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name        string            `yaml:"name"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Spec struct {
			Group    string `yaml:"group"`
			Versions []struct {
				Name    string `yaml:"name"`
				Served  bool   `yaml:"served"`
				Storage bool   `yaml:"storage"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	var crds []crdDoc
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	for {
		var doc crdDoc
		if decodeErr := dec.Decode(&doc); decodeErr != nil {
			break
		}
		if doc.Kind == "CustomResourceDefinition" {
			crds = append(crds, doc)
		}
	}
	if len(crds) != 1 || crds[0].Metadata.Name != mpiOperatorCRD {
		t.Fatalf("%s defines %d CRDs, want exactly %s", file, len(crds), mpiOperatorCRD)
	}
	crd := crds[0]
	if crd.Spec.Group != "kubeflow.org" {
		t.Errorf("%s: group = %q, want kubeflow.org", file, crd.Spec.Group)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != "v2beta1" ||
		!crd.Spec.Versions[0].Served || !crd.Spec.Versions[0].Storage {

		t.Errorf("%s: versions = %+v, want v2beta1 alone, served and stored (Run:ai looks for MPIJob at kubeflow.org/v2beta1)",
			file, crd.Spec.Versions)
	}
	// Upstream ships neither guard, so a re-vendor drops them, and
	// uninstalling the release would then delete every MPIJob.
	for key, want := range crdRetentionGuards {
		if got := crd.Metadata.Annotations[key]; got != want {
			t.Errorf("%s: CRD %s has %s=%q, want %q", file, crd.Metadata.Name, key, got, want)
		}
	}

	// The file's header says the two guards are the only change. Hold it to
	// that: a 670 KB schema is where a stray search-and-replace goes unseen.
	_, body, found := strings.Cut(string(data), "\n---\n")
	if !found {
		t.Fatalf("%s has no document separator after its header", file)
	}
	for key, value := range crdRetentionGuards {
		line := "    " + key + ": \"" + value + "\"\n"
		if strings.Count(body, line) != 1 {
			t.Fatalf("%s: want exactly one line %q", file, line)
		}
		body = strings.Replace(body, line, "", 1)
	}
	sum := sha256.Sum256([]byte(body))
	if got := hex.EncodeToString(sum[:]); got != mpiOperatorUpstreamCRDSHA256 {
		t.Errorf("%s without its two retention annotations hashes to %s, want upstream v%s's %s; "+
			"the vendored schema differs from upstream", file, got, mpiOperatorVersion, mpiOperatorUpstreamCRDSHA256)
	}

	check, err := os.ReadFile(mpiOperatorCheckFile)
	if err != nil {
		t.Fatalf("reading health check: %v", err)
	}
	for _, want := range []string{"name: " + mpiOperatorCRD, "name == 'v2beta1' && served == `true`"} {
		if !strings.Contains(string(check), want) {
			t.Errorf("health check does not assert %q", want)
		}
	}
}

// TestMPIOperatorManifestMatchesRunaiLookups pins what the Run:ai operator
// finds the MPI Operator by. It reads the Deployment mpi-operator in the
// release namespace and refuses one with more than a single container; its
// PodMonitor selects pods labeled app: mpi-operator and rewrites the scrape
// address to port 8081 only when the address has no port, which is why the
// container declares none and serves metrics there. Upstream's manifest has
// the names and the single portless container but leaves metrics off.
func TestMPIOperatorManifestMatchesRunaiLookups(t *testing.T) {
	t.Parallel()

	file := filepath.Join(mpiOperatorDir, mpiOperatorManifest)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	manifest := string(data)

	deployment := manifest[strings.LastIndex(manifest, "\nkind: Deployment\n")+1:]
	if !strings.HasPrefix(deployment, "kind: Deployment\n") {
		t.Fatalf("%s has no Deployment", file)
	}
	for _, want := range []string{
		"  name: mpi-operator\n  namespace: {{ .Release.Namespace }}\n",
		"      labels:\n        app: mpi-operator\n",
		"        - --lock-namespace={{ .Release.Namespace }}\n",
		"        - --monitoring-port=8081\n",
	} {
		if !strings.Contains(deployment, want) {
			t.Errorf("%s: the Deployment lacks %q", file, want)
		}
	}
	if n := len(regexp.MustCompile(`(?m)^\s+image:`).FindAllString(deployment, -1)); n != 1 {
		t.Errorf("%s: the Deployment has %d containers with an image, want 1 (Run:ai reads the image of a single-container Deployment only)", file, n)
	}
	if regexp.MustCompile(`(?m)^\s+(-\s+)?(ports|containerPort):`).MatchString(deployment) {
		t.Errorf("%s: the container declares ports; Run:ai's PodMonitor then scrapes the declared port instead of the metrics port", file)
	}
}

// TestMPIOperatorCanResumeSuspendedJobs keeps the one RBAC rule the vendored
// ClusterRole adds to upstream v0.8.2. From v0.8.0 the controller updates the
// launcher Job's status when it resumes a suspended MPIJob, and upstream's
// ClusterRole at v0.8.0 and v0.8.2 does not grant it, so the launcher stays
// suspended and the job never comes back (seen on GKE, 2026-11-25). Run:ai's
// Stop and Run on an MPI workload are this suspend and resume. A re-vendor
// from a release that still lacks the rule would drop it silently.
func TestMPIOperatorCanResumeSuspendedJobs(t *testing.T) {
	t.Parallel()

	file := filepath.Join(mpiOperatorDir, mpiOperatorManifest)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	type clusterRole struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Rules []struct {
			APIGroups []string `yaml:"apiGroups"`
			Resources []string `yaml:"resources"`
			Verbs     []string `yaml:"verbs"`
		} `yaml:"rules"`
	}
	var role *clusterRole
	// The ClusterRole documents carry no template syntax, so each parses as
	// plain YAML; the documents that do are skipped.
	for doc := range strings.SplitSeq(string(data), "\n---\n") {
		var cr clusterRole
		if yaml.Unmarshal([]byte(doc), &cr) != nil {
			continue
		}
		if cr.Kind == "ClusterRole" && cr.Metadata.Name == "mpi-operator" {
			role = &cr
		}
	}
	if role == nil {
		t.Fatalf("%s has no ClusterRole mpi-operator", file)
	}
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "batch") && slices.Contains(rule.Resources, "jobs/status") &&
			(slices.Contains(rule.Verbs, "update") || slices.Contains(rule.Verbs, "*")) {

			return
		}
	}
	t.Errorf("%s: ClusterRole mpi-operator does not grant update on batch jobs/status; "+
		"the controller needs it to resume a suspended MPIJob", file)
}

// TestMPIOperatorImageReadableByRunai pins the vendored image to its digest
// and holds that digest to what the Run:ai operator can read. Run:ai decides
// whether MPI workloads can be suspended from the operator's version, which it
// takes to be the text after the image reference's last colon and requires to
// be 0.4 or later. For a digest-pinned reference that text is the digest's
// hex, and Run:ai has no tag-to-version map for this operator (as it has for
// the Training Operator) to say otherwise, so whether suspend is reported
// available depends on how the hex happens to read as a version.
func TestMPIOperatorImageReadableByRunai(t *testing.T) {
	t.Parallel()

	file := filepath.Join(mpiOperatorDir, mpiOperatorManifest)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	matches := mpiOperatorImagePattern.FindAllStringSubmatch(string(data), -1)
	if len(matches) != 1 {
		t.Fatalf("%s has %d digest-pinned mpi-operator images, want exactly one", file, len(matches))
	}
	tag, digestHex := matches[0][2], matches[0][3]
	if tag != mpiOperatorVersion {
		t.Errorf("%s pins image tag %s, want %s", file, tag, mpiOperatorVersion)
	}
	if !strings.Contains(string(data), "tag v"+mpiOperatorVersion) {
		t.Errorf("%s does not record upstream tag v%s in its provenance header", file, mpiOperatorVersion)
	}

	reference := strings.TrimPrefix(strings.TrimSpace(matches[0][0]), "image: ")
	read := reference[strings.LastIndex(reference, ":")+1:]
	if read != digestHex {
		t.Fatalf("the text after the last colon of %s is %q, want the digest hex", reference, read)
	}
	if !runaiReadsSuspendableMPIOperator(read) {
		t.Errorf("Run:ai would read %q from %s as an MPI Operator older than 0.4 or as no version at all, and report "+
			"MPI workloads not suspendable. Pin a digest whose hex starts with a number from 1 up followed by a letter, "+
			"or drop the digest from this one image until Run:ai strips it before reading the tag", read, reference)
	}
}

// TestMPIOperatorImagePatternIsExact keeps mpiOperatorImagePattern from
// accepting an image line it only partly matches.
func TestMPIOperatorImagePatternIsExact(t *testing.T) {
	t.Parallel()

	const digest = "2740913e1802cbd24c9b16e143fbbfa99f0049e6d2946c8015bfd87c45ba6caa"
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"tag and 64-character digest", "        image: docker.io/mpioperator/mpi-operator:0.8.2@sha256:" + digest, true},
		{"digest one character too long", "        image: docker.io/mpioperator/mpi-operator:0.8.2@sha256:" + digest + "a", false},
		{"digest one character short", "        image: docker.io/mpioperator/mpi-operator:0.8.2@sha256:" + digest[:63], false},
		{"text after the digest", "        image: docker.io/mpioperator/mpi-operator:0.8.2@sha256:" + digest + " # pinned", false},
		{"no digest", "        image: docker.io/mpioperator/mpi-operator:0.8.2", false},
		{"registry left implied", "        image: mpioperator/mpi-operator:0.8.2@sha256:" + digest, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := mpiOperatorImagePattern.MatchString("      containers:\n" + tt.line + "\n        name: mpi-operator\n"); got != tt.want {
				t.Errorf("pattern matches %q = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// runaiReadsSuspendableMPIOperator reports whether Run:ai accepts a digest's
// hex as an MPI Operator version of 0.4 or later. It models, for hex input
// only, what runai-cluster 3.0.26's getSuspendStatus does with
// github.com/hashicorp/go-version: the leading decimal digits are the major
// version and must fit an int64, a letter must follow to start the
// pre-release, and the result compares against 0.4 by major version alone.
func runaiReadsSuspendableMPIOperator(hex string) bool {
	digits := len(hex) - len(strings.TrimLeft(hex, "0123456789"))
	if digits == 0 || digits == len(hex) {
		return false
	}
	major, err := strconv.ParseInt(hex[:digits], 10, 64)
	return err == nil && major >= 1
}

// TestRunaiReadsSuspendableMPIOperator checks the model against outcomes
// observed by running Run:ai 3.0.26's GetImageTag and VersionGreaterEqualThan
// with go-version v1.9.0, the release its go-operator builds with.
func TestRunaiReadsSuspendableMPIOperator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hex  string
		want bool
	}{
		{"digits then a letter reads as a large major version",
			"2740913e1802cbd24c9b16e143fbbfa99f0049e6d2946c8015bfd87c45ba6caa", true},
		{"leading letter is a malformed version",
			"a740913e1802cbd24c9b16e143fbbfa99f0049e6d2946c8015bfd87c45ba6caa", false},
		{"zero then a letter reads as 0.0.0, below 0.4",
			"0e40913e1802cbd24c9b16e143fbbfa99f0049e6d2946c8015bfd87c45ba6caa", false},
		{"all digits overflows the major version",
			"1234567890123456789012345678901234567890123456789012345678901234", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := runaiReadsSuspendableMPIOperator(tt.hex); got != tt.want {
				t.Errorf("runaiReadsSuspendableMPIOperator(%q) = %v, want %v", tt.hex, got, tt.want)
			}
		})
	}
}
