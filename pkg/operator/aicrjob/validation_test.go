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

package aicrjob

import (
	"slices"
	"testing"
)

// Validation reads run-scoped copies in its own namespace, runs only the
// deployment phase, pushes unsigned evidence to the run's tag and never
// allows mutable validator tags; the pre-provisioned validator identity is
// passed only from AICR v1.1.0.
func TestValidationArgs(t *testing.T) {
	t.Parallel()
	r := request(StageValidation)
	v := &Validation{AICRVersion: "v1.0.0", Evidence: "oci://registry.test/aicr/cluster/evidence/cluster:run-" + runID,
		PlainHTTP: true, ValidatorServiceAccount: "aicr-validator"}
	want := []string{
		"--recipe", "cm://aicr-validation/aicr-validation-" + runID + "-to",
		"--snapshot", "cm://aicr-validation/aicr-validation-" + runID + "-snapshot",
		"--namespace", "aicr-validation", "--phase", "deployment", "--fail-on-error=true",
		"--output", "/scratch/report.json", "--emit-attestation", "/scratch/evidence",
		"--push", "oci://registry.test/aicr/cluster/evidence/cluster:run-" + runID, "--no-sign", "--plain-http",
	}
	if got := ValidationArgs(r, v); !slices.Equal(got, want) {
		t.Fatalf("ValidationArgs() = %v\nwant %v", got, want)
	}
	// A signing key is ignored on v1.0.x: config validation refuses it there.
	v.SigningKey = "awskms:///alias/aicr-evidence"
	if got := ValidationArgs(r, v); !slices.Equal(got, want) {
		t.Fatalf("v1.0.0 ValidationArgs() with a key = %v", got)
	}
	v.AICRVersion, v.PlainHTTP, v.SigningKey = "1.1.0", false, ""
	v11 := []string{"--no-sign", "--validator-service-account-name", "aicr-validator", "--image-refs", "/dev/termination-log"}
	got := ValidationArgs(r, v)
	if !slices.Equal(got[len(got)-len(v11):], v11) || slices.Contains(got, "--plain-http") {
		t.Fatalf("v1.1.0 ValidationArgs() = %v", got)
	}
	for _, tc := range []struct {
		tlog bool
		want []string
	}{
		{true, []string{"--signing-key", "awskms:///alias/aicr-evidence", "--validator-service-account-name"}},
		{false, []string{"--signing-key", "awskms:///alias/aicr-evidence", "--tlog-upload=false", "--validator-service-account-name"}},
	} {
		v.SigningKey, v.TransparencyLog = "awskms:///alias/aicr-evidence", tc.tlog
		signed := ValidationArgs(r, v)
		if i := slices.Index(signed, "--signing-key"); i < 0 || !slices.Equal(signed[i:i+len(tc.want)], tc.want) || slices.Contains(signed, "--no-sign") {
			t.Fatalf("signed ValidationArgs() (tlog %v) = %v", tc.tlog, signed)
		}
	}
	for _, args := range [][]string{want, got} {
		if slices.Contains(args, "--allow-mutable-validator-tags") {
			t.Fatalf("mutable validator tags allowed: %v", args)
		}
	}
	job, err := Build(&Request{
		Stage: StageValidation, RunID: runID, RuntimeName: "cluster", RuntimeUID: uid, Image: image,
		Namespaces: namespaces, ServiceAccount: "aicr-validation", CredentialSecret: "creds", Args: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Namespace != "aicr-validation" || job.Spec.Template.Spec.ServiceAccountName != "aicr-validation" ||
		!slices.Equal(job.Spec.Template.Spec.Containers[0].Args[:1], []string{"validate"}) {

		t.Fatalf("validation Job = %s/%s %v", job.Namespace, job.Name, job.Spec.Template.Spec.Containers[0].Args)
	}
}
