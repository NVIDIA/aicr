// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package cli

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/urfave/cli/v3"

	bundlercfg "github.com/NVIDIA/aicr/pkg/bundler/config"
	appcfg "github.com/NVIDIA/aicr/pkg/config"
)

// === resolveDeployer ===

func TestResolveDeployer_FlagSetWinsOverConfig(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "deployer"}}
	runWith(t, flags, []string{"--deployer", "argocd"}, func(c *cli.Command) {
		got, err := resolveDeployer(c, bundlercfg.DeployerHelm)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != bundlercfg.DeployerArgoCD {
			t.Errorf("got %q, want argocd", got)
		}
	})
}

func TestResolveDeployer_NoFlagUsesConfigFallback(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "deployer"}}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveDeployer(c, bundlercfg.DeployerArgoCD)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != bundlercfg.DeployerArgoCD {
			t.Errorf("got %q, want argocd from config", got)
		}
	})
}

func TestResolveDeployer_NoFlagNoConfigDefaultsToHelm(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "deployer"}}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveDeployer(c, bundlercfg.DeployerType(""))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != bundlercfg.DeployerHelm {
			t.Errorf("got %q, want helm default", got)
		}
	})
}

func TestResolveDeployer_InvalidFlagReturnsError(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "deployer"}}
	runWith(t, flags, []string{"--deployer", "fluxcd"}, func(c *cli.Command) {
		_, err := resolveDeployer(c, bundlercfg.DeployerType(""))
		if err == nil {
			t.Fatal("expected error for invalid deployer")
		}
		if !strings.Contains(err.Error(), "invalid --deployer") {
			t.Errorf("error %q must mention --deployer", err.Error())
		}
	})
}

func TestResolveDeployer_FlagSetEmptyDefaultsToHelm(t *testing.T) {
	// `--deployer ""` matches pre-refactor behavior: empty CLI string
	// masks config and falls through to the Helm default rather than
	// being passed to ParseDeployerType (which rejects "").
	flags := []cli.Flag{&cli.StringFlag{Name: "deployer"}}
	runWith(t, flags, []string{"--deployer", ""}, func(c *cli.Command) {
		got, err := resolveDeployer(c, bundlercfg.DeployerArgoCD)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != bundlercfg.DeployerHelm {
			t.Errorf("got %q, want helm default", got)
		}
	})
}

// === resolveComponentPaths ===

func TestResolveComponentPaths_FlagSetParsesCLI(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "set"}}
	runWith(t, flags, []string{"--set", "gpuoperator:driver.version=570.0.0"},
		func(c *cli.Command) {
			got, err := resolveComponentPaths(c, "set", nil, bundlercfg.ParseValueOverrides)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 {
				t.Errorf("got %d entries, want 1", len(got))
			}
		})
}

func TestResolveComponentPaths_NoFlagReturnsFallback(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "set"}}
	fallback := []bundlercfg.ComponentPath{{Component: "x"}}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveComponentPaths(c, "set", fallback, bundlercfg.ParseValueOverrides)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Component != "x" {
			t.Errorf("expected fallback returned, got %v", got)
		}
	})
}

func TestResolveComponentPaths_NoFlagNilFallbackReturnsNil(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "set"}}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveComponentPaths(c, "set", nil, bundlercfg.ParseValueOverrides)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
}

func TestResolveComponentPaths_InvalidFlagReturnsError(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "set"}}
	runWith(t, flags, []string{"--set", "no-equals-sign"}, func(c *cli.Command) {
		_, err := resolveComponentPaths(c, "set", nil, bundlercfg.ParseValueOverrides)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "invalid --set") {
			t.Errorf("error %q must mention --set", err.Error())
		}
	})
}

func TestResolveComponentPaths_FlagOverridesNonEmptyConfig(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "set"}}
	fallback := []bundlercfg.ComponentPath{{Component: "old"}}
	runWith(t, flags, []string{"--set", "gpuoperator:driver.version=570.0.0"},
		func(c *cli.Command) {
			got, err := resolveComponentPaths(c, "set", fallback, bundlercfg.ParseValueOverrides)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 || got[0].Component == "old" {
				t.Errorf("expected CLI to replace fallback, got %v", got)
			}
		})
}

// === resolveTolerations ===

func TestResolveTolerations_FlagSetParsesCLI(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "tol"}}
	runWith(t, flags, []string{"--tol", "k=v:NoSchedule"}, func(c *cli.Command) {
		got, err := resolveTolerations(c, "tol", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Key != "k" {
			t.Errorf("got %v", got)
		}
	})
}

func TestResolveTolerations_NoFlagNilFallbackUsesDefault(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "tol"}}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveTolerations(c, "tol", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// DefaultTolerations is a single Toleration{Op: Exists}.
		if len(got) != 1 || got[0].Operator != corev1.TolerationOpExists {
			t.Errorf("expected DefaultTolerations, got %v", got)
		}
	})
}

func TestResolveTolerations_NoFlagNonNilFallbackReturnsFallback(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "tol"}}
	fallback := []corev1.Toleration{{Key: "from-config"}}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveTolerations(c, "tol", fallback)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Key != "from-config" {
			t.Errorf("expected fallback, got %v", got)
		}
	})
}

func TestResolveTolerations_NoFlagEmptyNonNilFallbackReturnsFallback(t *testing.T) {
	// Explicitly empty (non-nil) fallback round-trips as an empty
	// (non-nil) slice — the parser is not invoked when the flag is unset.
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "tol"}}
	fallback := []corev1.Toleration{}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveTolerations(c, "tol", fallback)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil empty, got nil")
		}
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})
}

func TestResolveTolerations_InvalidFlagReturnsError(t *testing.T) {
	flags := []cli.Flag{&cli.StringSliceFlag{Name: "tol"}}
	runWith(t, flags, []string{"--tol", "malformed"}, func(c *cli.Command) {
		_, err := resolveTolerations(c, "tol", nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "invalid --tol") {
			t.Errorf("error %q must mention --tol", err.Error())
		}
	})
}

// === resolveTaint ===

func TestResolveTaint_FlagSetParsesCLI(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "gate"}}
	runWith(t, flags, []string{"--gate", "k=v:NoSchedule"}, func(c *cli.Command) {
		got, err := resolveTaint(c, "gate", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.Key != "k" {
			t.Errorf("got %+v", got)
		}
	})
}

func TestResolveTaint_FlagSetEmptyMasksFallback(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "gate"}}
	fallback := &corev1.Taint{Key: "from-config"}
	// Explicit empty CLI value masks the config fallback and yields nil.
	// This preserves pre-refactor behavior where stringFlagOrConfig
	// surfaced "" and the caller skipped parsing entirely.
	runWith(t, flags, []string{"--gate", ""}, func(c *cli.Command) {
		got, err := resolveTaint(c, "gate", fallback)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil (CLI empty masks fallback), got %+v", got)
		}
	})
}

func TestResolveTaint_NoFlagReturnsFallback(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "gate"}}
	fallback := &corev1.Taint{Key: "from-config"}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveTaint(c, "gate", fallback)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.Key != "from-config" {
			t.Errorf("expected fallback, got %+v", got)
		}
	})
}

func TestResolveTaint_NoFlagNilFallbackReturnsNil(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "gate"}}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		got, err := resolveTaint(c, "gate", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})
}

func TestResolveTaint_FlagOverridesFallbackLogsOverride(t *testing.T) {
	// Just exercise the override branch; we don't assert the log.
	flags := []cli.Flag{&cli.StringFlag{Name: "gate"}}
	fallback := &corev1.Taint{Key: "old"}
	runWith(t, flags, []string{"--gate", "new=v:NoSchedule"}, func(c *cli.Command) {
		got, err := resolveTaint(c, "gate", fallback)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.Key != "new" {
			t.Errorf("expected new taint, got %+v", got)
		}
	})
}

func TestResolveTaint_InvalidFlagReturnsError(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "gate"}}
	runWith(t, flags, []string{"--gate", "no-effect"}, func(c *cli.Command) {
		_, err := resolveTaint(c, "gate", nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "invalid --gate") {
			t.Errorf("error %q must mention --gate", err.Error())
		}
	})
}

// === resolveOutputTarget ===

func TestResolveOutputTarget_FlagSetParsesCLI(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "output"}}
	resolved := &appcfg.BundleResolved{}
	runWith(t, flags, []string{"--output", "./mybundle"}, func(c *cli.Command) {
		ref, err := resolveOutputTarget(c, resolved)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ref == nil || ref.IsOCI {
			t.Errorf("expected local ref, got %+v", ref)
		}
	})
}

func TestResolveOutputTarget_NoFlagUsesResolvedTarget(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "output"}}
	b := &appcfg.BundleSpec{Output: &appcfg.BundleOutputSpec{Target: "./from-config"}}
	resolved, err := b.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		ref, err := resolveOutputTarget(c, resolved)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ref == nil {
			t.Fatal("expected ref, got nil")
		}
		if ref.LocalPath == "" {
			t.Errorf("expected LocalPath populated, got %+v", ref)
		}
	})
}

func TestResolveOutputTarget_NoFlagNoResolvedDefaultsToCurrentDir(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "output"}}
	resolved := &appcfg.BundleResolved{}
	runWith(t, flags, []string{}, func(c *cli.Command) {
		ref, err := resolveOutputTarget(c, resolved)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ref == nil || ref.IsOCI {
			t.Errorf("expected local ref defaulting to '.', got %+v", ref)
		}
	})
}

func TestResolveOutputTarget_InvalidFlagReturnsError(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "output"}}
	resolved := &appcfg.BundleResolved{}
	runWith(t, flags, []string{"--output", "oci://"}, func(c *cli.Command) {
		_, err := resolveOutputTarget(c, resolved)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "invalid --output") {
			t.Errorf("error %q must mention --output", err.Error())
		}
	})
}

func TestResolveOutputTarget_FlagSetEmptyDefaultsToCurrentDir(t *testing.T) {
	// `--output ""` matches pre-refactor behavior where the empty
	// string was substituted with "." before parsing rather than
	// passed through (which would have produced LocalPath: "").
	flags := []cli.Flag{&cli.StringFlag{Name: "output"}}
	b := &appcfg.BundleSpec{Output: &appcfg.BundleOutputSpec{Target: "./from-config"}}
	resolved, err := b.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	runWith(t, flags, []string{"--output", ""}, func(c *cli.Command) {
		ref, err := resolveOutputTarget(c, resolved)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ref == nil || ref.IsOCI {
			t.Errorf("expected local ref defaulting to '.', got %+v", ref)
		}
		if ref.LocalPath != "." {
			t.Errorf("expected LocalPath '.', got %q", ref.LocalPath)
		}
	})
}

func TestResolveOutputTarget_FlagOverridesNonEmptyConfigLogs(t *testing.T) {
	flags := []cli.Flag{&cli.StringFlag{Name: "output"}}
	b := &appcfg.BundleSpec{Output: &appcfg.BundleOutputSpec{Target: "./old"}}
	resolved, err := b.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	runWith(t, flags, []string{"--output", "./new"}, func(c *cli.Command) {
		ref, err := resolveOutputTarget(c, resolved)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ref == nil {
			t.Fatal("expected ref")
		}
		if !strings.HasSuffix(ref.LocalPath, "new") {
			t.Errorf("expected ./new override, got %+v", ref)
		}
	})
}
