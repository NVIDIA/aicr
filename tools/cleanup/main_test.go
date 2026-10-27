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
	"bytes"
	"context"
	stderrors "errors"
	"flag"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    options
		wantErr bool
	}{
		{"no flags", nil, options{}, false},
		{"single-dash flags", []string{"-dry-run", "-yes"}, options{dryRun: true, assumeYes: true}, false},
		{"short flags", []string{"-n", "-y"}, options{dryRun: true, assumeYes: true}, false},
		{"long flags", []string{"--dry-run", "--yes", "--keep-crds", "--keep-namespaces"},
			options{dryRun: true, assumeYes: true, keepCRDs: true, keepNamespaces: true}, false},
		{"repeated and comma-separated exclusions",
			[]string{"--exclude-ns", "skyhook,gpu-operator", "--exclude-ns", "nvsentinel", "--exclude-crd", "skyhook.nvidia.com"},
			options{excludeNS: []string{"skyhook", "gpu-operator", "nvsentinel"}, excludeCRD: []string{"skyhook.nvidia.com"}}, false},
		{"padded tokens are trimmed and empties dropped",
			[]string{"--exclude-ns", " skyhook , gpu-operator ,,", "--exclude-crd=skyhook.nvidia.com, nodewright.nvidia.com"},
			options{excludeNS: []string{"skyhook", "gpu-operator"}, excludeCRD: []string{"skyhook.nvidia.com", "nodewright.nvidia.com"}}, false},
		{"bundle directory", []string{"--bundle", "./bundles"}, options{bundleDir: "./bundles"}, false},
		{"flag-shaped bundle argument", []string{"--bundle", "--yes"}, options{}, true},
		{"empty bundle argument", []string{"--bundle="}, options{}, true},
		{"missing exclusion argument", []string{"--exclude-ns"}, options{}, true},
		{"flag-shaped exclusion argument", []string{"--exclude-ns", "--yes"}, options{}, true},
		{"flag-shaped crd argument", []string{"--exclude-crd", "-yes"}, options{}, true},
		{"unknown flag", []string{"--force"}, options{}, true},
		{"positional argument", []string{"--yes", "extra"}, options{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOptions(tt.args, io.Discard)
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("parseOptions(%q) error = %v", tt.args, err)
			}
			if got.dryRun != tt.want.dryRun || got.assumeYes != tt.want.assumeYes ||
				got.keepCRDs != tt.want.keepCRDs || got.keepNamespaces != tt.want.keepNamespaces ||
				got.bundleDir != tt.want.bundleDir {

				t.Errorf("parseOptions(%q) flags = %+v, want %+v", tt.args, got, tt.want)
			}
			if !slices.Equal(got.excludeNS, tt.want.excludeNS) {
				t.Errorf("excludeNS = %q, want %q", got.excludeNS, tt.want.excludeNS)
			}
			if !slices.Equal(got.excludeCRD, tt.want.excludeCRD) {
				t.Errorf("excludeCRD = %q, want %q", got.excludeCRD, tt.want.excludeCRD)
			}
		})
	}
}

func TestParseOptionsHelp(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseOptions([]string{"--help"}, &stderr)
	if !stderrors.Is(err, flag.ErrHelp) {
		t.Fatalf("error = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(stderr.String(), "Usage: go run ./tools/cleanup") {
		t.Errorf("help output missing usage:\n%s", stderr.String())
	}
}

func TestRunRejectsBadFlagsBeforeContactingACluster(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"help", []string{"-h"}, errors.ExitSuccess},
		{"flag-shaped argument", []string{"--exclude-ns", "--yes"}, errors.ExitInvalidInput},
		{"positional argument", []string{"now"}, errors.ExitInvalidInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			log := slog.New(slog.NewTextHandler(&stderr, nil))
			if got := run(context.Background(), tt.args, strings.NewReader(""), &stdout, &stderr, log); got != tt.want {
				t.Errorf("run(%q) = %d, want %d\nstderr:\n%s", tt.args, got, tt.want, stderr.String())
			}
		})
	}
}

type kubeConfig = clientcmd.ClientConfig

// unreadableConfig is a kubeconfig that fails to load.
type unreadableConfig struct{ kubeConfig }

func (unreadableConfig) RawConfig() (clientcmdapi.Config, error) {
	return clientcmdapi.Config{}, errors.New(errors.ErrCodeNotFound, "no kubeconfig")
}

func TestContextName(t *testing.T) {
	tests := []struct {
		name string
		cfg  clientcmd.ClientConfig
		want string
	}{
		{"current context set", clientcmd.NewDefaultClientConfig(clientcmdapi.Config{CurrentContext: "staging"}, nil), "staging"},
		{"no current context", clientcmd.NewDefaultClientConfig(clientcmdapi.Config{}, nil), ""},
		{"kubeconfig fails to load", unreadableConfig{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contextName(tt.cfg); got != tt.want {
				t.Errorf("contextName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrompt(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"  y  \n", true},
		{"yes\n", false},
		{"n\n", false},
		{"\n", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(strings.TrimSpace(tt.input), func(t *testing.T) {
			var out bytes.Buffer
			if got := newPrompt(strings.NewReader(tt.input), &out)(context.Background(), "Proceed?"); got != tt.want {
				t.Errorf("prompt(%q) = %v, want %v", tt.input, got, tt.want)
			}
			if out.String() != "Proceed? (y/N): " {
				t.Errorf("prompt wrote %q", out.String())
			}
		})
	}
}

func TestPromptReturnsWhenCanceled(t *testing.T) {
	in, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	ask := newPrompt(in, &out)
	answered := make(chan bool, 1)
	go func() { answered <- ask(ctx, "Proceed?") }()

	cancel()
	select {
	case got := <-answered:
		if got {
			t.Error("canceled prompt answered yes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return after its context was canceled")
	}
	if out.String() != "Proceed? (y/N): \n" {
		t.Errorf("prompt wrote %q", out.String())
	}
}
