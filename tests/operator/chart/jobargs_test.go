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

package chart

import (
	"os"
	"path/filepath"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/resolver"
)

const runID = "0123456789ab"

// supportedAICRVersions are the AICR releases .settings.yaml pins for the
// operator: its default and every release Operator E2E runs against. The
// arguments the operator builds depend on the release.
func supportedAICRVersions(t *testing.T) []string {
	t.Helper()
	settings := struct {
		Operator struct {
			Version string `json:"aicr_version"`
			E2E     []struct {
				Version string `json:"version"`
			} `json:"aicr_e2e"`
		} `json:"operator"`
	}{}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".settings.yaml"))
	if err == nil {
		err = yaml.Unmarshal(raw, &settings)
	}
	if err != nil {
		t.Fatal(err)
	}
	versions := make([]string, 0, 1+len(settings.Operator.E2E))
	versions = append(versions, settings.Operator.Version)
	for _, e := range settings.Operator.E2E {
		versions = append(versions, e.Version)
	}
	if settings.Operator.Version == "" || len(settings.Operator.E2E) == 0 {
		t.Fatalf("no operator AICR versions in .settings.yaml: %+v", settings.Operator)
	}
	return versions
}

// fullSpec sets every runtime field that adds an AICR flag.
func fullSpec() *v1alpha1.ClusterRuntimeSpec {
	placement := v1alpha1.NodeGroupPlacement{
		NodeSelector: map[string]v1alpha1.LabelValue{"nodeGroup": "gpu", "zone": "a"},
		Tolerations: []corev1.Toleration{
			{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gpu", Effect: corev1.TaintEffectNoExecute},
		},
	}
	return &v1alpha1.ClusterRuntimeSpec{
		Intent:   "training",
		Platform: "kubeflow",
		Profile:  "gpuStack=operator-managed",
		Criteria: &v1alpha1.DeclaredCriteria{Service: "gke", Accelerator: "h100", OS: "cos"},
		Configuration: &v1alpha1.RecipeConfiguration{GKE: &v1alpha1.GKEConfiguration{
			TCPXOInterfaces: []v1alpha1.NetworkInterfaceMapping{{InterfaceName: "eth1", Network: "gpu-net-1"}},
		}},
		Placement: v1alpha1.Placement{System: placement, Accelerated: placement},
		Overrides: []v1alpha1.Override{
			{Component: "gpu-operator", Path: "driver.version", Value: apiextensionsv1.JSON{Raw: []byte(`"580.95.05"`)}},
			{Component: "nvsentinel", Path: "enabled", Value: apiextensionsv1.JSON{Raw: []byte(`false`)}},
		},
	}
}

// operatorJobArgs returns, per stage, every argument list the operator
// builds for an AICR version: each optional flag set, and each branch of
// evidence signing.
func operatorJobArgs(t *testing.T, version string) map[aicrjob.Stage][][]string {
	t.Helper()
	spec := fullSpec()
	request := func(stage aicrjob.Stage) *aicrjob.Request {
		return &aicrjob.Request{Stage: stage, RunID: runID,
			Namespaces: aicrjob.Namespaces{Jobs: jobsNS, Discovery: discoveryNS, Validation: validationNS}}
	}
	must := func(args []string, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return args
	}
	render := func(stage aicrjob.Stage, s *v1alpha1.ClusterRuntimeSpec, plainHTTP bool) []string {
		req := request(stage)
		args := must(resolver.RenderArgs(s, aicrjob.InputURI(req, aicrjob.InputTo), "oci://registry/bundles:"+runID, plainHTTP))
		return append(args, aicrjob.ImageRefsArgs(version)...)
	}
	recipeReq := request(aicrjob.StageRecipe)
	snapshotReq := request(aicrjob.StageSnapshot)
	validationReq := request(aicrjob.StageValidation)
	derived := map[string]string{"service": "gke", "accelerator": "h100", "os": "cos"}
	validation := func(signingKey string, tlog, plainHTTP bool) []string {
		return aicrjob.ValidationArgs(validationReq, &aicrjob.Validation{
			AICRVersion: version, Evidence: "oci://registry/evidence:" + runID, PlainHTTP: plainHTTP,
			ValidatorServiceAccount: "aicr-validator", SigningKey: signingKey, TransparencyLog: tlog,
		})
	}
	return map[aicrjob.Stage][][]string{
		aicrjob.StageRecipe: {
			resolver.RecipeArgs(spec, derived, "cm://"+discoveryNS+"/snapshot", aicrjob.OutputURI(recipeReq)),
			resolver.RecipeArgs(&v1alpha1.ClusterRuntimeSpec{Intent: "training"}, derived, "", aicrjob.OutputURI(recipeReq)),
		},
		aicrjob.StageBaseRender: {render(aicrjob.StageBaseRender, resolver.BaseSpec(spec), false)},
		aicrjob.StageRender: {
			render(aicrjob.StageRender, spec, true),
			render(aicrjob.StageRender, &v1alpha1.ClusterRuntimeSpec{Intent: "training"}, false),
		},
		aicrjob.StageAssessment: {aicrjob.AssessmentArgs(request(aicrjob.StageAssessment), "argocd")},
		aicrjob.StageSnapshot: {
			must(resolver.SnapshotArgs(&spec.Placement, discoveryNS, "aicr-snapshot-agent", aicrImage, "cpu=10m,memory=64Mi",
				aicrjob.OutputURI(snapshotReq))),
			must(resolver.SnapshotArgs(&v1alpha1.Placement{}, discoveryNS, "aicr-snapshot-agent", aicrImage, "",
				aicrjob.OutputURI(snapshotReq))),
		},
		aicrjob.StageValidation: {
			validation("", false, false),
			validation("awskms://alias/aicr", true, true),
			validation("awskms://alias/aicr", false, false),
		},
	}
}

// jobWithArgs builds the operator's Job for a stage with the given AICR
// arguments.
func jobWithArgs(t *testing.T, stage aicrjob.Stage, serviceAccount string, args []string) *batchv1.Job {
	t.Helper()
	j, err := aicrjob.Build(&aicrjob.Request{
		Stage: stage, RunID: runID, RuntimeName: v1alpha1.SingletonName, RuntimeUID: runtimeUID, Image: aicrImage,
		Args:           args,
		Namespaces:     aicrjob.Namespaces{Jobs: jobsNS, Discovery: discoveryNS, Validation: validationNS},
		ServiceAccount: serviceAccount, CredentialSecret: "aicr-run-" + runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// stageServiceAccounts are the identities each stage's Jobs run as.
var stageServiceAccounts = map[aicrjob.Stage]string{
	aicrjob.StageRecipe:     "aicr-jobs",
	aicrjob.StageBaseRender: "aicr-jobs",
	aicrjob.StageRender:     "aicr-jobs",
	aicrjob.StageAssessment: "aicr-jobs",
	aicrjob.StageSnapshot:   "aicr-snapshot",
	aicrjob.StageValidation: "aicr-validation",
}

// operatorJobFlags checks the flag allowlist for every subcommand: a flag the
// operator never builds, a known flag in a form it never writes, and an
// argument that breaks the flag-value pairing are each denied.
func operatorJobFlags(t *testing.T, create func(*batchv1.Job) error) {
	t.Helper()
	const flags = "only the flags the operator builds"
	cases := []struct {
		name string
		args []string
	}{
		{"a configuration file", []string{"--config", "/tmp/aicr.yaml"}},
		{"a data directory", []string{"--data", "/tmp/catalog"}},
		{"a data directory, joined", []string{"--data=/tmp/catalog"}},
		{"a flag a later release could add", []string{"--catalog-image", "busybox@" + digestA}},
		{"a known flag, single dash", []string{"-output", "cm://y"}},
		{"a short alias", []string{"-o", "cm://y"}},
		{"a known flag, joined", []string{"--output=cm://y"}},
		{"a switch the operator never joins", []string{"--plain-http=true"}},
		{"a known flag with a trailing newline", []string{"--output\n", "cm://y"}},
		{"a value flag last", []string{"--output"}},
		{"a value flag followed by a flag", []string{"--output", "--output", "cm://y"}},
		{"a positional argument", []string{"extra"}},
		{"the end of flags", []string{"--", "--data", "/tmp/catalog"}},
	}
	for _, stage := range []aicrjob.Stage{aicrjob.StageRecipe, aicrjob.StageRender, aicrjob.StageAssessment,
		aicrjob.StageSnapshot, aicrjob.StageValidation} {
		for _, tc := range cases {
			args := append([]string{"--output", "cm://x"}, tc.args...)
			denied(t, string(stage)+": "+tc.name, create(jobWithArgs(t, stage, stageServiceAccounts[stage], args)), flags)
		}
	}
	denied(t, "bundle: a switch given a value", create(jobWithArgs(t, aicrjob.StageRender, "aicr-jobs",
		[]string{"--plain-http", "true", "--output", "cm://x"})), flags)
	// The CLI trims an argument before reading it as a flag; the policy does
	// not, so a padded flag must not land where a value is admitted.
	denied(t, "bundle: a flag padded with a space", create(jobWithArgs(t, aicrjob.StageRender, "aicr-jobs",
		[]string{"--plain-http", " --data", "/tmp/catalog", "--output", "cm://x"})), flags)
	denied(t, "validate: the fail-on-error the operator does not use", create(jobWithArgs(t, aicrjob.StageValidation,
		"aicr-validation", []string{"--fail-on-error=false", "--output", "cm://x"})), flags)
	denied(t, "upgrade-check: a validate flag", create(jobWithArgs(t, aicrjob.StageAssessment, "aicr-jobs",
		[]string{"--phase", "deployment", "--output", "cm://x"})), flags)
}
