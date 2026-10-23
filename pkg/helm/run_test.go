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

package helm

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

const fakeHelmEnv = "AICR_FAKE_HELM"

// fakeHelm sets how the fake helm behaves. The first field set among Orphan,
// Hang, StdoutBytes, EchoStdin and Stderr decides, and otherwise it prints
// Stdout.
type fakeHelm struct {
	Stdout        string
	Stderr        string
	StderrPadding int // lines written to stderr before Stderr
	StdoutBytes   int64
	EchoStdin     bool
	Hang          bool
	// Orphan starts a hanging child that inherits stdout and stderr, prints
	// its PID, then hangs if Hang is set and exits otherwise.
	Orphan bool
}

// TestFakeHelmSubprocessHelper acts as the fake helm when fakeCommand starts
// this binary, and does nothing otherwise.
func TestFakeHelmSubprocessHelper(_ *testing.T) {
	raw := os.Getenv(fakeHelmEnv)
	if raw == "" {
		return
	}
	var f fakeHelm
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		fmt.Fprintln(os.Stderr, "decode fake helm:", err)
		os.Exit(2)
	}
	switch {
	case f.Orphan:
		child := exec.Command(os.Args[0], "-test.run=^TestFakeHelmSubprocessHelper$")
		child.Env = append(os.Environ(), fakeHelmEnv+`={"Hang":true}`)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "start child:", err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, child.Process.Pid)
		if f.Hang {
			time.Sleep(time.Minute)
		}
	case f.Hang:
		time.Sleep(time.Minute)
	case f.StdoutBytes > 0:
		_, _ = os.Stdout.Write(make([]byte, f.StdoutBytes))
	case f.EchoStdin:
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case f.Stderr != "":
		for range f.StderrPadding {
			fmt.Fprintln(os.Stderr, "WARNING: padding")
		}
		fmt.Fprintln(os.Stderr, f.Stderr)
		os.Exit(1)
	default:
		fmt.Fprint(os.Stdout, f.Stdout)
	}
	os.Exit(0)
}

// fakeCommand returns command, which starts this test binary as a fake helm
// behaving as f, and calls, which reports the command line of each call to
// command.
func fakeCommand(t *testing.T, f fakeHelm) (command commandFunc, calls func() [][]string) {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("encode fake helm: %v", err)
	}
	var recorded [][]string
	command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		recorded = append(recorded, append([]string{name}, args...))
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFakeHelmSubprocessHelper$")
		cmd.Env = append(os.Environ(), fakeHelmEnv+"="+string(raw))
		return cmd
	}
	return command, func() [][]string { return recorded }
}

func TestRun(t *testing.T) {
	command, calls := fakeCommand(t, fakeHelm{Stdout: `[{"name":"a"}]`})

	out, err := run(t.Context(), command, nil, time.Minute, 1024, "list", "--output", "json")
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if string(out) != `[{"name":"a"}]` {
		t.Errorf("run() = %q, want the fake's stdout", out)
	}
	if got, want := calls(), [][]string{{Binary, "list", "--output", "json"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("helm calls = %q, want %q", got, want)
	}
}

func TestRunNoArgs(t *testing.T) {
	_, err := Run(t.Context(), time.Minute, 1024)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestRunPipesStdin(t *testing.T) {
	command, _ := fakeCommand(t, fakeHelm{EchoStdin: true})

	out, err := run(t.Context(), command, strings.NewReader("replicaCount: 7\n"), time.Minute, 1024, "template")
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if string(out) != "replicaCount: 7\n" {
		t.Errorf("run() = %q, want stdin echoed back", out)
	}
}

func TestRunFailures(t *testing.T) {
	tests := []struct {
		name      string
		fake      fakeHelm
		wantMsg   string
		wantNoMsg string
	}{
		{
			name:    "error line is summarized",
			fake:    fakeHelm{Stderr: "WARNING: retrying\nError: cluster unreachable"},
			wantMsg: "helm list failed: cluster unreachable",
		},
		{
			name:      "stderr without an error line",
			fake:      fakeHelm{Stderr: "WARNING: retrying"},
			wantMsg:   "helm list failed",
			wantNoMsg: "WARNING",
		},
		{
			name:    "output over the limit",
			fake:    fakeHelm{StdoutBytes: 1 << 20},
			wantMsg: "helm list output exceeds size limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, _ := fakeCommand(t, tt.fake)

			_, err := run(t.Context(), command, nil, time.Minute, 1024, "list")
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("run() error = %v, want it to contain %q", err, tt.wantMsg)
			}
			if tt.wantNoMsg != "" && err != nil && strings.Contains(err.Error(), tt.wantNoMsg) {
				t.Errorf("run() error = %v, want it not to contain %q", err, tt.wantNoMsg)
			}
		})
	}
}

func TestRunFailureCarriesStderr(t *testing.T) {
	command, _ := fakeCommand(t, fakeHelm{Stderr: "Error: boom"})

	_, err := run(t.Context(), command, nil, time.Minute, 1024, "get", "values", "x")
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("run() error = %v, want a StructuredError", err)
	}
	if se.Context["stderr"] != "Error: boom" || se.Context["args"] != "get values x" {
		t.Errorf("error context = %v, want stderr and args", se.Context)
	}
}

func TestRunBoundsStderr(t *testing.T) {
	command, _ := fakeCommand(t, fakeHelm{StderrPadding: 10_000, Stderr: "Error: boom"})

	_, err := run(t.Context(), command, nil, time.Minute, 1024, "list")
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("run() error = %v, want a StructuredError", err)
	}
	if !strings.Contains(err.Error(), "helm list failed: boom") {
		t.Errorf("run() error = %v, want the trailing Error: line summarized", err)
	}
	stderr, _ := se.Context["stderr"].(string)
	if len(stderr) > defaults.HelmStderrLimit {
		t.Errorf("stderr context is %d bytes, want at most %d", len(stderr), defaults.HelmStderrLimit)
	}
	if !strings.HasSuffix(stderr, "Error: boom") {
		t.Errorf("stderr context ends %q, want it to keep the Error: line", stderr[max(0, len(stderr)-40):])
	}
}

func TestTailBuffer(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		want   string
	}{
		{"under the limit", []string{"ab", "cd"}, "abcd"},
		{"at the limit", []string{"abcde"}, "abcde"},
		{"one write over", []string{"abcdefg"}, "cdefg"},
		{"spans writes", []string{"abc", "def", "gh"}, "defgh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &tailBuffer{limit: 5}
			for _, w := range tt.writes {
				if n, err := b.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v, want %d, nil", w, n, err, len(w))
				}
			}
			if got := string(b.buf); got != tt.want {
				t.Errorf("buffer = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunDeadContext(t *testing.T) {
	tests := []struct {
		name     string
		ctx      func(t *testing.T) context.Context
		timeout  time.Duration
		wantCode errors.ErrorCode
	}{
		{
			name: "parent deadline",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			timeout:  time.Hour,
			wantCode: errors.ErrCodeTimeout,
		},
		{
			name:     "run timeout",
			ctx:      func(t *testing.T) context.Context { return t.Context() },
			timeout:  100 * time.Millisecond,
			wantCode: errors.ErrCodeTimeout,
		},
		{
			name: "parent canceled",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			timeout:  time.Hour,
			wantCode: errors.ErrCodeCanceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, _ := fakeCommand(t, fakeHelm{Hang: true})

			_, err := run(tt.ctx(t), command, nil, tt.timeout, 1024, "list")
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

func TestRunBoundsInheritedPipes(t *testing.T) {
	tests := []struct {
		name     string
		hang     bool
		timeout  time.Duration
		wantCode errors.ErrorCode
	}{
		{"helm exits", false, time.Minute, errors.ErrCodeInternal},
		{"helm killed at timeout", true, time.Second, errors.ErrCodeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, _ := fakeCommand(t, fakeHelm{Orphan: true, Hang: tt.hang})

			start := time.Now()
			out, err := run(t.Context(), command, nil, tt.timeout, 1024, "list")
			elapsed := time.Since(start)

			pid, perr := strconv.Atoi(strings.TrimSpace(string(out)))
			if perr != nil {
				t.Fatalf("run() stdout = %q, want the child's PID", out)
			}
			if p, ferr := os.FindProcess(pid); ferr == nil {
				_ = p.Kill()
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			// The child hangs for a minute, so only WaitDelay returns run sooner.
			if elapsed > 30*time.Second {
				t.Errorf("run() took %s, want it bounded by defaults.HelmWaitDelay", elapsed)
			}
		})
	}
}

func TestRenderChartPipesValues(t *testing.T) {
	command, calls := fakeCommand(t, fakeHelm{EchoStdin: true})

	out, err := renderChart(t.Context(), command, ChartInput{
		Name: "gpu-operator", Chart: "gpu-operator", Repository: "https://helm.ngc.nvidia.com/nvidia",
		Values: map[string]any{"driver": map[string]any{"enabled": false}},
	})
	if err != nil {
		t.Fatalf("renderChart() error = %v", err)
	}
	if string(out) != "driver:\n    enabled: false\n" {
		t.Errorf("helm stdin = %q, want the marshaled values", out)
	}
	if got := calls(); len(got) != 1 || !slices.Contains(got[0], "-") {
		t.Errorf("helm calls = %q, want one call reading values from stdin", got)
	}
}

func TestTemplateArgs(t *testing.T) {
	tests := []struct {
		name          string
		input         ChartInput
		valuesOnStdin bool
		want          string
	}{
		{
			name: "oci chart",
			input: ChartInput{
				Name: "gpu-operator", Chart: "nvidia/gpu-operator", Repository: "oci://ghcr.io/nvidia/",
				Version: "v25.3.0", Namespace: "gpu-operator", KubeVersion: "1.33.0", APIVersions: []string{},
			},
			want: "template release-gpu-operator oci://ghcr.io/nvidia/gpu-operator --version v25.3.0 " +
				"--namespace gpu-operator --kube-version 1.33.0 --include-crds=false",
		},
		{
			name: "http chart with values file and inline values",
			input: ChartInput{
				Name: "ebs", Chart: "aws-ebs-csi-driver", Repository: "https://charts.example.com",
				KubeVersion: "1.33.0", APIVersions: []string{"a/v1", "b/v1"}, ValuesPath: "/tmp/values.yaml",
			},
			valuesOnStdin: true,
			want: "template release-ebs aws-ebs-csi-driver --repo https://charts.example.com --kube-version 1.33.0 " +
				"--api-versions a/v1 --api-versions b/v1 --values /tmp/values.yaml --values - --include-crds=false",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Join(templateArgs(tt.input, tt.valuesOnStdin), " "); got != tt.want {
				t.Errorf("templateArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}
