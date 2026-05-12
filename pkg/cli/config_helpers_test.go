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
	"context"
	"testing"

	"github.com/urfave/cli/v3"
)

// runWith builds a Command with the given Flags and runs it with args, calling
// the supplied capture function inside Action so tests can inspect the parsed
// command (cmd.IsSet, cmd.String, etc.) the same way real handlers do.
func runWith(t *testing.T, flags []cli.Flag, args []string, capture func(*cli.Command)) {
	t.Helper()
	cmd := &cli.Command{
		Name:  "t",
		Flags: flags,
		Action: func(_ context.Context, c *cli.Command) error {
			capture(c)
			return nil
		},
	}
	if err := cmd.Run(context.Background(), append([]string{"t"}, args...)); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestStringFlagOrConfig(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		flagDef  cli.Flag
		fallback string
		want     string
	}{
		{
			name:     "flag not set returns fallback",
			args:     []string{},
			flagDef:  &cli.StringFlag{Name: "x"},
			fallback: "from-config",
			want:     "from-config",
		},
		{
			name:     "flag set wins over fallback",
			args:     []string{"--x", "from-flag"},
			flagDef:  &cli.StringFlag{Name: "x"},
			fallback: "from-config",
			want:     "from-flag",
		},
		{
			name:     "default value does NOT count as set",
			args:     []string{},
			flagDef:  &cli.StringFlag{Name: "x", Value: "default"},
			fallback: "from-config",
			want:     "from-config", // critical: default-only must not mask config
		},
		{
			name:     "explicit value matching default still counts as set",
			args:     []string{"--x", "default"},
			flagDef:  &cli.StringFlag{Name: "x", Value: "default"},
			fallback: "from-config",
			want:     "default",
		},
		{
			name:     "no fallback no flag returns empty",
			args:     []string{},
			flagDef:  &cli.StringFlag{Name: "x"},
			fallback: "",
			want:     "",
		},
		{
			// Regression: when both the CLI flag and the config fallback
			// are empty, the flag's compile-time Value: default must
			// surface — not the empty string. Caught on `aicr validate`:
			// --namespace declares Value: "aicr-validation" but collapsed
			// to "" when --config did not set spec.validate.agent.namespace,
			// which crashed downstream namespace creation with
			// "name: Required value".
			name:     "no fallback no flag returns flag default value",
			args:     []string{},
			flagDef:  &cli.StringFlag{Name: "x", Value: "default"},
			fallback: "",
			want:     "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runWith(t, []cli.Flag{tt.flagDef}, tt.args, func(c *cli.Command) {
				if got := stringFlagOrConfig(c, "x", tt.fallback); got != tt.want {
					t.Errorf("got %q, want %q", got, tt.want)
				}
			})
		})
	}
}

func TestIntFlagOrConfig(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		flagDef  cli.Flag
		fallback int
		want     int
	}{
		{"flag not set returns fallback", []string{}, &cli.IntFlag{Name: "n"}, 5, 5},
		{"flag set overrides fallback", []string{"--n", "9"}, &cli.IntFlag{Name: "n"}, 5, 9},
		{"default zero does not count as set", []string{}, &cli.IntFlag{Name: "n", Value: 0}, 5, 5},
		{"explicit zero counts as set", []string{"--n", "0"}, &cli.IntFlag{Name: "n"}, 5, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runWith(t, []cli.Flag{tt.flagDef}, tt.args, func(c *cli.Command) {
				if got := intFlagOrConfig(c, "n", tt.fallback); got != tt.want {
					t.Errorf("got %d, want %d", got, tt.want)
				}
			})
		})
	}
}

func TestBoolFlagOrConfig(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		fallback bool
		want     bool
	}{
		{"unset returns false fallback", []string{}, false, false},
		{"unset returns true fallback", []string{}, true, true},
		{"explicit true overrides false fallback", []string{"--b"}, false, true},
		{"explicit true matches true fallback", []string{"--b"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runWith(t, []cli.Flag{&cli.BoolFlag{Name: "b"}}, tt.args, func(c *cli.Command) {
				if got := boolFlagOrConfig(c, "b", tt.fallback); got != tt.want {
					t.Errorf("got %v, want %v", got, tt.want)
				}
			})
		})
	}
}

func TestStringSliceFlagOrConfig(t *testing.T) {
	flag := &cli.StringSliceFlag{Name: "s"}

	t.Run("unset returns fallback", func(t *testing.T) {
		runWith(t, []cli.Flag{flag}, nil, func(c *cli.Command) {
			got := stringSliceFlagOrConfig(c, "s", []string{"a", "b"})
			if len(got) != 2 || got[0] != "a" || got[1] != "b" {
				t.Errorf("got %v, want [a b]", got)
			}
		})
	})

	t.Run("CLI replaces fallback", func(t *testing.T) {
		runWith(t, []cli.Flag{flag}, []string{"--s", "x", "--s", "y"}, func(c *cli.Command) {
			got := stringSliceFlagOrConfig(c, "s", []string{"a", "b"})
			if len(got) != 2 || got[0] != "x" || got[1] != "y" {
				t.Errorf("got %v, want [x y] (CLI replaces, not appends)", got)
			}
		})
	})
}

func TestResolveNodeSelector(t *testing.T) {
	flag := &cli.StringSliceFlag{Name: "sel"}

	t.Run("unset returns copy of fallback", func(t *testing.T) {
		fallback := map[string]string{"role": "system"}
		runWith(t, []cli.Flag{flag}, nil, func(c *cli.Command) {
			got, err := resolveNodeSelector(c, "sel", fallback)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got["role"] != "system" {
				t.Errorf("got %v, want role=system", got)
			}
			// Mutating result must not mutate fallback (defensive copy).
			got["role"] = "modified"
			if fallback["role"] != "system" {
				t.Errorf("fallback was mutated")
			}
		})
	})

	t.Run("unset with nil fallback returns nil (preserves unset)", func(t *testing.T) {
		runWith(t, []cli.Flag{flag}, nil, func(c *cli.Command) {
			got, err := resolveNodeSelector(c, "sel", nil)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != nil {
				t.Errorf("got %v, want nil (nil fallback must propagate)", got)
			}
		})
	})

	t.Run("unset with explicitly empty fallback returns non-nil empty map", func(t *testing.T) {
		runWith(t, []cli.Flag{flag}, nil, func(c *cli.Command) {
			got, err := resolveNodeSelector(c, "sel", map[string]string{})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got == nil {
				t.Errorf("got nil, want empty (non-nil) map (explicit-empty preserved)")
			}
			if len(got) != 0 {
				t.Errorf("got %v, want empty map", got)
			}
		})
	})

	t.Run("CLI parses and replaces fallback", func(t *testing.T) {
		runWith(t, []cli.Flag{flag}, []string{"--sel", "k=v"}, func(c *cli.Command) {
			got, err := resolveNodeSelector(c, "sel", map[string]string{"role": "system"})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got["k"] != "v" {
				t.Errorf("missing k=v in %v", got)
			}
			if _, ok := got["role"]; ok {
				t.Errorf("CLI should replace, not merge: %v", got)
			}
		})
	})

	t.Run("malformed CLI selector returns error", func(t *testing.T) {
		runWith(t, []cli.Flag{flag}, []string{"--sel", "no-equals-sign"}, func(c *cli.Command) {
			_, err := resolveNodeSelector(c, "sel", nil)
			if err == nil {
				t.Errorf("expected error for malformed selector")
			}
		})
	})
}
