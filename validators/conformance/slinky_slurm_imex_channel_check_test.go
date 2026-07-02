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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestCheckSlinkySlurmIMEXChannelSkipsWithoutEnabledSlinkyComponent(t *testing.T) {
	tests := []struct {
		name          string
		componentRefs []recipe.ComponentRef
	}{
		{
			name:          "component absent",
			componentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}},
		},
		{
			name: "component disabled",
			componentRefs: []recipe.ComponentRef{{
				Name:      slinkySlurmComponent,
				Overrides: map[string]any{"enabled": false},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &validators.Context{
				Ctx:        context.Background(),
				Clientset:  k8sfake.NewSimpleClientset(),
				RESTConfig: &rest.Config{Host: "https://example.test"},
				ValidationInput: &v1.ValidationInput{
					ComponentRefs: tt.componentRefs,
				},
			}

			err := CheckSlinkySlurmIMEXChannel(ctx)
			if !isSkipLike(err, "slinky-slurm") {
				t.Fatalf("error = %v, want skip mentioning slinky-slurm", err)
			}
		})
	}
}

func TestCheckSlinkySlurmIMEXChannel(t *testing.T) {
	// Channel numbers are arbitrary; only distinct, well-formed paths matter.
	tests := []struct {
		name    string
		stdout  string
		result  podExecResult
		execErr error
		wantErr string
	}{
		{
			name: "distinct channels pass",
			stdout: "IMEX_CHANNEL=/dev/nvidia-caps-imex-channels/channel2\n" +
				"IMEX_CHANNEL=/dev/nvidia-caps-imex-channels/channel3\n",
		},
		{
			name:    "missing channel fails",
			stdout:  "IMEX_CHANNEL=/dev/nvidia-caps-imex-channels/channel2\n",
			wantErr: "expected two IMEX channels",
		},
		{
			name: "duplicate channel fails",
			stdout: "IMEX_CHANNEL=/dev/nvidia-caps-imex-channels/channel2\n" +
				"IMEX_CHANNEL=/dev/nvidia-caps-imex-channels/channel2\n",
			wantErr: "same IMEX channel",
		},
		{
			name:    "nonzero command fails",
			result:  podExecResult{ExitCode: 1, Stderr: "srun failed"},
			wantErr: "exit code 1",
		},
		{
			name:    "exec error fails",
			execErr: errors.New(errors.ErrCodeInternal, "exec failed"),
			wantErr: "exec failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := slinkyIMEXTestContext(t, true, true)
			restore := replaceSlinkyExecForTest(func(
				context.Context,
				*validators.Context,
				string,
				string,
				[]string,
				podExecOptions,
			) (podExecResult, error) {

				result := tt.result
				if result.Stdout == "" {
					result.Stdout = tt.stdout
				}
				return result, tt.execErr
			})
			defer restore()

			err := CheckSlinkySlurmIMEXChannel(ctx)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckSlinkySlurmIMEXChannelRequiresFixedResources(t *testing.T) {
	tests := []struct {
		name                         string
		includeComputeDomain         bool
		includeResourceClaimTemplate bool
		wantErr                      string
	}{
		{
			name:                         "missing ComputeDomain",
			includeResourceClaimTemplate: true,
			wantErr:                      "ComputeDomain slurm/slinky-slurm-imex",
		},
		{
			name:                 "missing ResourceClaimTemplate",
			includeComputeDomain: true,
			wantErr:              "ResourceClaimTemplate slurm/slinky-slurm-imex-channels",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := slinkyIMEXTestContext(t, tt.includeComputeDomain, tt.includeResourceClaimTemplate)
			execCalled := false
			restore := replaceSlinkyExecForTest(func(
				context.Context,
				*validators.Context,
				string,
				string,
				[]string,
				podExecOptions,
			) (podExecResult, error) {

				execCalled = true
				return podExecResult{
					Stdout: "IMEX_CHANNEL=/dev/nvidia-caps-imex-channels/channel2\n" +
						"IMEX_CHANNEL=/dev/nvidia-caps-imex-channels/channel3\n",
				}, nil
			})
			defer restore()

			err := CheckSlinkySlurmIMEXChannel(ctx)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if execCalled {
				t.Fatal("Slurm exec was called before fixed IMEX resources were verified")
			}
		})
	}
}

func TestCheckSlinkySlurmIMEXChannelSkipsKWOKBeforeRequiringFixedResources(t *testing.T) {
	ctx := slurmReadyTestContext(t, true)
	restore := replaceSlinkyExecForTest(func(
		context.Context,
		*validators.Context,
		string,
		string,
		[]string,
		podExecOptions,
	) (podExecResult, error) {

		t.Fatal("exec should not run when all NodeSet pods are on KWOK nodes")
		return podExecResult{}, nil
	})
	defer restore()

	err := CheckSlinkySlurmIMEXChannel(ctx)
	if !isSkipLike(err, "KWOK") {
		t.Fatalf("error = %v, want KWOK skip", err)
	}
}

func TestCheckSlinkySlurmIMEXChannelRequiresContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  *validators.Context
		want string
	}{
		{
			name: "missing client",
			ctx: &validators.Context{
				Ctx:             context.Background(),
				RESTConfig:      &rest.Config{Host: "https://example.test"},
				ValidationInput: &v1.ValidationInput{},
			},
			want: "kubernetes client",
		},
		{
			name: "missing rest config",
			ctx: &validators.Context{
				Ctx:             context.Background(),
				Clientset:       k8sfake.NewSimpleClientset(),
				ValidationInput: &v1.ValidationInput{},
			},
			want: "RESTConfig",
		},
		{
			name: "missing validation",
			ctx: &validators.Context{
				Ctx:        context.Background(),
				Clientset:  k8sfake.NewSimpleClientset(),
				RESTConfig: &rest.Config{Host: "https://example.test"},
			},
			want: "validation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSlinkySlurmIMEXChannel(tt.ctx)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseSlinkySlurmIMEXChannelsRejectsMalformedPath(t *testing.T) {
	for _, channel := range []string{
		"/dev/nvidia-caps-imex-channels/channel",
		"/dev/wrong",
		"/dev/nvidia-caps-imex-channels/channel2/extra",
	} {
		t.Run(channel, func(t *testing.T) {
			_, err := parseSlinkySlurmIMEXChannels("IMEX_CHANNEL=" + channel)
			if err == nil || !strings.Contains(err.Error(), "invalid IMEX channel path") {
				t.Fatalf("error = %v, want invalid IMEX channel path", err)
			}
		})
	}
}

func TestSlinkySlurmIMEXCommandIsBoundedAndResourceScoped(t *testing.T) {
	for _, want := range []string{
		"--immediate=30",
		"--time=1:00",
		"--nodes=1",
		"--ntasks=1",
		"--cpus-per-task=1",
		"--mem=128M",
		"--gres=gpu:1",
		"IMEX_CHANNEL_ERROR=find failed",
		"channel_count=$(printf \"%s\\n\" \"$channel\" | wc -l)",
		"IMEX_CHANNEL_COUNT=%s",
		"IMEX_CHANNEL_CANDIDATES:",
		"IMEX_CHANNEL_ERROR=expected exactly one channel",
		"test \"$channel_count\" -ne 1",
		"sleep 40",
		"test \"$first_rc\" -eq 0 && test \"$second_rc\" -eq 0",
	} {
		if !strings.Contains(slinkySlurmIMEXChannelShell, want) {
			t.Fatalf("IMEX command is missing %q: %s", want, slinkySlurmIMEXChannelShell)
		}
	}
	if strings.Contains(slinkySlurmIMEXChannelShell, "printf '%s\\n'") {
		t.Fatal("single-quoted printf format breaks the surrounding single-quoted srun script")
	}
}

func slinkyIMEXTestContext(
	t *testing.T,
	includeComputeDomain bool,
	includeResourceClaimTemplate bool,
) *validators.Context {

	t.Helper()

	ctx := slurmReadyTestContext(t, false)
	objects := []runtime.Object{defaultLoginSet(), defaultNodeSet()}
	if includeComputeDomain {
		computeDomain := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "resource.nvidia.com/v1beta1",
				"kind":       "ComputeDomain",
				"metadata": map[string]any{
					"name":      "slinky-slurm-imex",
					"namespace": slinkySlurmNamespace,
				},
			},
		}
		computeDomain.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "resource.nvidia.com", Version: "v1beta1", Kind: "ComputeDomain",
		})
		objects = append(objects, computeDomain)
	}
	ctx.DynamicClient = newSlinkyDynamicClient(t, objects...)

	if includeResourceClaimTemplate {
		_, err := ctx.Clientset.ResourceV1().ResourceClaimTemplates(slinkySlurmNamespace).Create(
			ctx.Ctx,
			&resourcev1.ResourceClaimTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "slinky-slurm-imex-channels",
					Namespace: slinkySlurmNamespace,
				},
			},
			metav1.CreateOptions{},
		)
		if err != nil {
			t.Fatalf("create ResourceClaimTemplate fixture: %v", err)
		}
	}
	return ctx
}
