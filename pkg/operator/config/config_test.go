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

package config

import (
	"flag"
	"io"
	"slices"
	"testing"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

const image = "ghcr.io/nvidia/aicr:v1.0.0@sha256:e91ba6f4916a5bfcd19d05933b5b4d07a4dd05a738a1ffebfab243a32cbff433"

func valid() Config {
	c := Defaults()
	c.AICRVersion, c.AICRImage, c.RegistryRepository = "v1.0.0", image, "oci://registry.example.com/aicr/cluster"
	return c
}

// parse binds c's flags and parses args over c.
func parse(c *Config, args ...string) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.BindFlags(fs)
	return fs.Parse(args)
}

func TestFlagsOverrideDefaults(t *testing.T) {
	t.Parallel()
	c := Defaults()
	err := parse(&c,
		"--aicr-version=v1.0.0", "--aicr-image="+image, "--registry-repository=oci://r.example.com/x",
		"--registry-plain-http", "--development-bypass-approvals", "--jobs-namespace=jobs", "--engine=ArgoCD",
		"--engine-namespace=gitops", "--engine-project=p", "--development-skip-discovery", "--snapshot-agent-requests=cpu=250m",
		"--allow-prerelease-aicr",
	)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if c.JobsNamespace != "jobs" || !c.RegistryPlainHTTP || !c.BypassApprovals || c.Engine != plan.EngineArgoCD ||
		c.EngineNamespace != "gitops" || c.EngineProject != "p" || c.OperatorNamespace != "aicr-system" ||
		!c.SkipDiscovery || c.SnapshotAgentRequests != "cpu=250m" || c.SnapshotAgentServiceAccount != "aicr-snapshot-agent" ||
		!c.AllowPrereleaseAICR {

		t.Fatalf("config = %+v", c)
	}
	if err = c.Validate(true); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if r := c.Registry(); r.Host != "r.example.com" || r.Path != "x" {
		t.Errorf("Registry() = %+v", r)
	}
	if ns := c.Namespaces(); ns.Jobs != "jobs" || ns.Discovery != "aicr-discovery" {
		t.Errorf("Namespaces() = %+v", ns)
	}
	if c.RestrictNamespaces {
		t.Error("an unset --component-namespaces restricts namespaces")
	}
	for value, want := range map[string][]string{"gpu-operator, cert-manager,": {"gpu-operator", "cert-manager"}, "": nil} {
		c = Defaults()
		if err = parse(&c, "--component-namespaces="+value); err != nil || !c.RestrictNamespaces || !slices.Equal(c.ComponentNamespaces, want) {
			t.Errorf("--component-namespaces=%q: %v %t %v", value, err, c.RestrictNamespaces, c.ComponentNamespaces)
		}
	}
}

// TestValidate checks which configurations Validate accepts. An out-of-range
// AICR version passes, so the resolver can report it as a stalled runtime.
func TestValidate(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		wantErr     bool
		development bool
		mutate      func(*Config)
	}{
		"prerelease without opt-in":  {wantErr: true, mutate: func(c *Config) { c.AICRVersion = "v1.3.0-rc.1" }},
		"prerelease with opt-in":     {wantErr: false, mutate: func(c *Config) { c.AICRVersion, c.AllowPrereleaseAICR = "v1.3.0-rc.1", true }},
		"unprefixed version":         {wantErr: true, mutate: func(c *Config) { c.AICRVersion = "1.0.0" }},
		"out-of-range version":       {wantErr: false, mutate: func(c *Config) { c.AICRVersion = "v2.0.0" }},
		"bypass without development": {wantErr: true, mutate: func(c *Config) { c.BypassApprovals = true }},
		"skip without development":   {wantErr: true, mutate: func(c *Config) { c.SkipDiscovery = true }},
		"development settings in a development build": {wantErr: false, development: true,
			mutate: func(c *Config) { c.BypassApprovals, c.SkipDiscovery = true, true }},
		"no snapshot account":   {wantErr: true, mutate: func(c *Config) { c.SnapshotServiceAccount = "" }},
		"unpinned image":        {wantErr: true, mutate: func(c *Config) { c.AICRImage = "ghcr.io/nvidia/aicr:v1.0.0" }},
		"no AICR version":       {wantErr: true, mutate: func(c *Config) { c.AICRVersion = "" }},
		"Flux":                  {wantErr: true, mutate: func(c *Config) { c.Engine = plan.EngineFlux }},
		"no jobs namespace":     {wantErr: true, mutate: func(c *Config) { c.JobsNamespace = "" }},
		"no project":            {wantErr: true, mutate: func(c *Config) { c.EngineProject = "" }},
		"registry without path": {wantErr: true, mutate: func(c *Config) { c.RegistryRepository = "oci://registry.example.com" }},
		"unknown provider":      {wantErr: true, mutate: func(c *Config) { c.RegistryAuthProvider = "gcp" }},
		"aws without ECR":       {wantErr: true, mutate: func(c *Config) { c.RegistryAuthProvider = "aws" }},
		"aws with a static Secret": {wantErr: true, mutate: func(c *Config) {
			c.RegistryAuthProvider, c.RegistryCredentialsSecret = "aws", "registry-auth"
			c.RegistryRepository = "oci://123456789012.dkr.ecr.us-west-2.amazonaws.com/aicr/cluster"
		}},
	}
	for name, tt := range tests {
		c := valid()
		tt.mutate(&c)
		err := c.Validate(tt.development)
		if (err != nil) != tt.wantErr || (err != nil && operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest) {
			t.Errorf("%s: Validate() = %v, wantErr %t", name, err, tt.wantErr)
		}
	}
}

const validator = "ghcr.io/nvidia/aicr-validators/deployment:v1.0.0@sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790"

// With validation enabled, every validator image is pinned by tag and
// digest, once per repository.
func TestValidateValidation(t *testing.T) {
	t.Parallel()
	c := valid()
	conformance := "ghcr.io/nvidia/aicr-validators/conformance:v1.0.0@sha256:e091d51f600255c2166e5c1b42afd556880d498f2a9bb913a32da38fa28e038b"
	if err := parse(&c, "--validation-enabled", "--validator-images="+validator+", "+conformance); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(false); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	pins := c.ValidatorPins()
	if len(pins) != 2 || pins["ghcr.io/nvidia/aicr-validators/deployment"] != validator ||
		c.ValidationServiceAccount != "aicr-validation" || c.ValidatorServiceAccount != "aicr-validator" {

		t.Fatalf("pins = %v, config = %+v", pins, c)
	}
	for name, images := range map[string][]string{
		"none":          nil,
		"tag only":      {"ghcr.io/nvidia/aicr-validators/deployment:v1.0.0"},
		"digest only":   {"ghcr.io/nvidia/aicr-validators/deployment@sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790"},
		"latest":        {"ghcr.io/nvidia/aicr-validators/deployment:latest"},
		"pinned twice":  {validator, "ghcr.io/nvidia/aicr-validators/deployment:v1.0.1@sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790"},
		"short digest":  {"ghcr.io/nvidia/aicr-validators/deployment:v1.0.0@sha256:3c0a"},
		"no repository": {"deployment:v1.0.0@sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790"},
	} {
		bad := valid()
		bad.ValidationEnabled, bad.ValidatorImages = true, images
		if err := bad.Validate(true); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("%s: Validate() = %v, want INVALID_REQUEST", name, err)
		}
	}
	c = valid()
	c.ValidatorImages = []string{"anything"}
	if err := c.Validate(false); err != nil {
		t.Errorf("validator images are checked with validation disabled: %v", err)
	}
}

// A signing key needs validation and AICR v1.1.0 or later, and must be a KMS
// key URI, so evidence expected signed is never pushed unsigned.
func TestValidateSigning(t *testing.T) {
	t.Parallel()
	c := valid()
	if !c.ValidationTransparencyLog {
		t.Fatal("the transparency log is off by default")
	}
	err := parse(&c, "--aicr-version=v1.1.0", "--validation-enabled", "--validator-images="+validator,
		"--validation-signing-key=awskms:///alias/aicr-evidence", "--validation-transparency-log=false")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Validate(false); err != nil || !c.SignsEvidence() || c.ValidationTransparencyLog {
		t.Fatalf("Validate() = %v, SignsEvidence() = %v, config = %+v", err, c.SignsEvidence(), c)
	}
	for name, mutate := range map[string]func(*Config){
		"validation disabled": func(c *Config) { c.ValidationEnabled = false },
		"AICR v1.0.x":         func(c *Config) { c.AICRVersion = "v1.0.0" },
		"file key":            func(c *Config) { c.ValidationSigningKey = "/etc/cosign.key" },
		"no key path":         func(c *Config) { c.ValidationSigningKey = "gcpkms://" },
	} {
		bad := c
		mutate(&bad)
		if err := bad.Validate(false); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("%s: Validate() = %v, want INVALID_REQUEST", name, err)
		}
	}
	unsigned := c
	unsigned.ValidationSigningKey = ""
	if err := unsigned.Validate(false); err != nil || unsigned.SignsEvidence() {
		t.Errorf("unsigned: Validate() = %v, SignsEvidence() = %v", err, unsigned.SignsEvidence())
	}
}

func TestValidateAWSProvider(t *testing.T) {
	t.Parallel()
	c := valid()
	c.RegistryAuthProvider = "aws"
	c.RegistryRepository = "oci://123456789012.dkr.ecr.us-west-2.amazonaws.com/aicr/cluster"
	if err := c.Validate(false); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

// AICR Jobs take the placement the chart's aicr.jobs values give, as JSON;
// none by default.
func TestJobPlacementFlags(t *testing.T) {
	c := Defaults()
	err := parse(&c,
		`--job-node-selector={"node-role":"system"}`,
		`--job-tolerations=[{"key":"CriticalAddonsOnly","operator":"Exists","effect":"NoSchedule"}]`,
	)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if c.JobNodeSelector["node-role"] != "system" || len(c.JobTolerations) != 1 || c.JobTolerations[0].Key != "CriticalAddonsOnly" {
		t.Fatalf("placement = %v %v", c.JobNodeSelector, c.JobTolerations)
	}
	if d := Defaults(); len(d.JobNodeSelector) != 0 || len(d.JobTolerations) != 0 {
		t.Fatalf("default placement = %v %v", d.JobNodeSelector, d.JobTolerations)
	}
	for _, bad := range []string{`--job-node-selector=[1]`, `--job-tolerations={}`} {
		fresh := Defaults()
		if err = parse(&fresh, bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
