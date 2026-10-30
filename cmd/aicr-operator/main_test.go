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

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/operator/config"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// isolate points client config discovery at nothing, so a test can never
// reach a developer's cluster.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
}

func TestRunPrintsVersion(t *testing.T) {
	isolate(t)
	if err := run([]string{"--version"}); err != nil {
		t.Fatalf("run(--version) = %v", err)
	}
}

func TestRunRejectsInvalidLogLevel(t *testing.T) {
	isolate(t)
	err := run([]string{"--log-level=loud"})
	if err == nil || !strings.Contains(err.Error(), "--log-level") {
		t.Fatalf("run(--log-level=loud) = %v, want a log-level error", err)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	isolate(t)
	if err := run([]string{"--no-such-flag"}); err == nil {
		t.Fatal("run(--no-such-flag) = nil, want a flag error")
	}
}

var installFlags = []string{
	"--aicr-version=v1.0.0",
	"--aicr-image=ghcr.io/nvidia/aicr:v1.0.0@sha256:e91ba6f4916a5bfcd19d05933b5b4d07a4dd05a738a1ffebfab243a32cbff433",
	"--registry-repository=oci://registry.example.com/aicr/cluster",
}

// Without client config the manager must refuse to start rather than fall
// back to any ambient cluster.
func TestRunFailsWithoutClientConfig(t *testing.T) {
	isolate(t)
	args := append([]string{"--leader-elect=false", "--metrics-bind-address=0", "--health-probe-bind-address=127.0.0.1:0"}, installFlags...)
	err := run(args)
	if err == nil || !strings.Contains(err.Error(), "client config") {
		t.Fatalf("run() = %v, want a client config error", err)
	}
}

// The installation configuration is checked before anything starts.
func TestRunRejectsInvalidInstallation(t *testing.T) {
	isolate(t)
	err := run([]string{"--aicr-version=v1.0.0", "--aicr-image=ghcr.io/nvidia/aicr:v1.0.0", "--registry-repository=oci://r/x"})
	if err == nil || !strings.Contains(err.Error(), "pinned by digest") {
		t.Fatalf("run() = %v, want an unpinned-image error", err)
	}
}

// Validation binds evidence to the operator's own pod, so it refuses to start
// without the pod name; past that check, startup reaches client config.
func TestRunNeedsPodNameForValidation(t *testing.T) {
	validation := append([]string{"--leader-elect=false", "--metrics-bind-address=0", "--health-probe-bind-address=127.0.0.1:0",
		"--validation-enabled=true",
		"--validator-images=ghcr.io/nvidia/aicr-validators/deployment:v1.0.0@sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790",
	}, installFlags...)
	cases := []struct {
		name, pod, want string
	}{
		{"without POD_NAME", "", "POD_NAME"},
		{"with POD_NAME", "aicr-operator-0", "client config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("POD_NAME", tc.pod)
			if err := run(validation); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run() = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// The static provider needs nothing from AWS; the aws provider derives its
// region from the ECR host and refreshes the configured repo-creds Secret.
func TestCredentialProviders(t *testing.T) {
	isolate(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	cfg := config.Defaults()
	cfg.RegistryRepository = "oci://registry.example.com/aicr/cluster"
	cfg.RegistryCredentialsSecret = "registry-auth"
	provider, refresher, err := credentials(context.Background(), nil, nil, &cfg)
	if err != nil || refresher != nil {
		t.Fatalf("static: %v, refresher %v", err, refresher)
	}
	if p, ok := provider.(*registry.StaticProvider); !ok || p.SecretName != "registry-auth" || p.Namespace != "aicr-system" {
		t.Fatalf("static provider = %+v", provider)
	}

	cfg.RegistryAuthProvider = registry.ProviderAWS
	cfg.RegistryCredentialsSecret = ""
	cfg.RegistryRepository = "oci://123456789012.dkr.ecr.us-west-2.amazonaws.com/aicr/cluster"
	provider, refresher, err = credentials(context.Background(), nil, nil, &cfg)
	if err != nil || refresher == nil || refresher.Name != "aicr-registry" || refresher.Namespace != "argocd" {
		t.Fatalf("aws: %v, refresher %+v", err, refresher)
	}
	if p, ok := provider.(*registry.ECRProvider); !ok || p.Host != "123456789012.dkr.ecr.us-west-2.amazonaws.com" {
		t.Fatalf("aws provider = %+v", provider)
	}
	cfg.RegistryRepository = "oci://registry.example.com/aicr/cluster"
	if _, _, err = credentials(context.Background(), nil, nil, &cfg); err == nil {
		t.Fatal("the aws provider accepted a registry that is not ECR")
	}
}
