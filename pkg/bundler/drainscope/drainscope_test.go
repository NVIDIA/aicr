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

package drainscope

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestSystemNamespacesPattern(t *testing.T) {
	pattern := SystemNamespacesPattern([]string{"nvidia-network-operator", "cert-manager", "", "cert-manager", "a.b"})
	want := `^(a\.b|cert-manager|gmp-system|gpu-operator|kube-system|network-operator|nvidia-network-operator|nvsentinel|skyhook)$`
	if pattern != want {
		t.Fatalf("pattern = %s, want %s", pattern, want)
	}
	re := regexp.MustCompile(pattern)
	for _, ns := range append([]string{"cert-manager", "nvidia-network-operator", "a.b"}, ChartSystemNamespaces()...) {
		if !re.MatchString(ns) {
			t.Errorf("%q is not protected", ns)
		}
	}
	// Anchored and escaped: near-misses and user namespaces stay drainable.
	for _, ns := range []string{"default", "cert-manager-x", "x-cert-manager", "axb", "training", "network-operator2"} {
		if re.MatchString(ns) {
			t.Errorf("%q is protected; the pattern must match exact names only", ns)
		}
	}
}

func TestApply(t *testing.T) {
	on := func(drainer map[string]any) map[string]any {
		values := map[string]any{"global": map[string]any{"nodeDrainer": map[string]any{"enabled": true}}}
		if drainer != nil {
			values[NodeDrainerKey] = drainer
		}
		return values
	}
	defaultScope := []any{map[string]any{"name": "*", "mode": DefaultMode}}
	generated := SystemNamespacesPattern([]string{"cert-manager"})

	tests := []struct {
		name             string
		values           map[string]any
		wantUnchanged    bool
		wantSystem       string
		wantUser         any
		wantUserAbsent   bool
		wantErr          bool
		protected, drain []string
	}{
		{name: "node-drainer absent", values: map[string]any{"global": map[string]any{}}, wantUnchanged: true},
		{name: "node-drainer disabled", wantUnchanged: true,
			values: map[string]any{"global": map[string]any{"nodeDrainer": map[string]any{"enabled": false}}}},
		{name: "node-drainer section without enabled", wantUnchanged: true,
			values: map[string]any{"global": map[string]any{"nodeDrainer": map[string]any{}}}},
		{name: "defaults", values: on(nil), wantSystem: generated, wantUser: defaultScope,
			protected: []string{"cert-manager", "nvsentinel"}, drain: []string{"default"}},
		{name: "non-map nodeDrainer section renders", wantSystem: generated, wantUser: defaultScope,
			values: map[string]any{"global": map[string]any{"nodeDrainer": "yes"}}},
		{name: "non-bool enabled renders", wantSystem: generated, wantUser: defaultScope,
			values: map[string]any{"global": map[string]any{"nodeDrainer": map[string]any{"enabled": "true"}}}},
		{name: "operator pattern kept as an alternative",
			values:     on(map[string]any{"systemNamespaces": "^team-.*$"}),
			wantSystem: generated + "|(?:^team-.*$)", wantUser: defaultScope,
			protected: []string{"cert-manager", "team-infra"}, drain: []string{"default", "my-team-x"}},
		{name: "operator pattern equal to the generated one",
			values:     on(map[string]any{"systemNamespaces": generated}),
			wantSystem: generated, wantUser: defaultScope},
		{name: "invalid operator pattern", values: on(map[string]any{"systemNamespaces": "^(a"}), wantErr: true},
		{name: "empty operator pattern", values: on(map[string]any{"systemNamespaces": ""}), wantErr: true},
		{name: "non-string operator pattern", values: on(map[string]any{"systemNamespaces": 1}), wantErr: true},
		{name: "node-drainer not a map", wantErr: true, values: map[string]any{
			"global": map[string]any{"nodeDrainer": map[string]any{"enabled": true}}, NodeDrainerKey: "x"}},
		{name: "explicit userNamespaces kept",
			values:     on(map[string]any{"userNamespaces": []any{map[string]any{"name": "*", "mode": ModeImmediate}}}),
			wantSystem: generated, wantUser: []any{map[string]any{"name": "*", "mode": ModeImmediate}}},
		{name: "explicit empty userNamespaces kept",
			values:     on(map[string]any{"userNamespaces": []any{}}),
			wantSystem: generated, wantUser: []any{}},
		{name: "podDrainPolicies empty userNamespaces, as the chart requires",
			values:     on(map[string]any{"podDrainPolicies": []any{map[string]any{"name": "p", "mode": ModeImmediate}}}),
			wantSystem: generated, wantUser: []any{}},
		{name: "null userNamespaces counts as unset",
			values:     on(map[string]any{"userNamespaces": nil}),
			wantSystem: generated, wantUser: defaultScope},
		{name: "custom drain with a blank nodeSelector owns every node",
			values:     on(map[string]any{"customDrain": map[string]any{"enabled": true, "nodeSelector": "  "}}),
			wantSystem: generated, wantUserAbsent: true},
		{name: "empty podDrainPolicies still get the default",
			values:     on(map[string]any{"podDrainPolicies": []any{}}),
			wantSystem: generated, wantUser: defaultScope},
		{name: "custom drain owning every node",
			values:     on(map[string]any{"customDrain": map[string]any{"enabled": true}}),
			wantSystem: generated, wantUserAbsent: true},
		{name: "custom drain scoped by nodeSelector",
			values:     on(map[string]any{"customDrain": map[string]any{"enabled": true, "nodeSelector": "scheduler=slurm"}}),
			wantSystem: generated, wantUser: defaultScope},
		{name: "custom drain off",
			values:     on(map[string]any{"customDrain": map[string]any{"enabled": false}}),
			wantSystem: generated, wantUser: defaultScope},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := component.DeepCopyMap(tt.values)
			err := Apply(t.Context(), tt.values, []string{"cert-manager"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Apply error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantUnchanged {
				if !reflect.DeepEqual(tt.values, before) {
					t.Fatalf("values changed with node-drainer off: %v", tt.values)
				}
				return
			}
			system, _ := component.GetValueByPath(tt.values, SystemNamespacesPath)
			if system != tt.wantSystem {
				t.Errorf("systemNamespaces = %v, want %v", system, tt.wantSystem)
			}
			user, present := component.GetValueByPath(tt.values, UserNamespacesPath)
			if tt.wantUserAbsent {
				if present {
					t.Errorf("userNamespaces = %v, want absent", user)
				}
			} else if !reflect.DeepEqual(user, tt.wantUser) {
				t.Errorf("userNamespaces = %v, want %v", user, tt.wantUser)
			}
			re := regexp.MustCompile(system.(string))
			for _, ns := range tt.protected {
				if !re.MatchString(ns) {
					t.Errorf("%q is not protected by %s", ns, system)
				}
			}
			for _, ns := range tt.drain {
				if re.MatchString(ns) {
					t.Errorf("%q is protected by %s", ns, system)
				}
			}
		})
	}
}

func TestRecipeNamespaces(t *testing.T) {
	result := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
		{Name: "gpu-operator", Namespace: "privileged-gpu-operator"},
		{Name: "cert-manager"},
	}}
	got, err := RecipeNamespaces(result)
	if err != nil {
		t.Fatalf("RecipeNamespaces: %v", err)
	}
	want := []string{"privileged-gpu-operator", "cert-manager"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RecipeNamespaces = %v, want %v", got, want)
	}

	// The declared union wins over the rendered subset.
	subset := result.WithDeclaredComponents([]recipe.ComponentRef{{Name: "kai-scheduler"}})
	if got, _ := RecipeNamespaces(subset); !reflect.DeepEqual(got, []string{"kai-scheduler"}) {
		t.Errorf("RecipeNamespaces(declared) = %v, want [kai-scheduler]", got)
	}

	unknown := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "not-in-registry"}}}
	if _, err := RecipeNamespaces(unknown); err == nil || !strings.Contains(err.Error(), `"not-in-registry" has no namespace`) {
		t.Errorf("RecipeNamespaces(no namespace) = %v, want an error naming the component", err)
	}
}

func TestValidSystemNamespacesMessages(t *testing.T) {
	tests := []struct {
		raw  any
		want string
	}{
		{"", "protecting no namespace"},
		{"^(a", "fail to start"},
		{".*", "matches every namespace"},
		{"^(team-infra)$|", "matches every namespace"},
		{"^[a-z0-9]", "matches every namespace"},
		{3, "must be a string"},
		{"^(" + strings.Repeat("a|", 5000) + "b)$", "longer than the 8192 AICR validates"},
		// The first alternative matches every namespace; the second outgrows
		// the search, which must fail closed rather than accept.
		{"^.{1,63}$|^.*a.{12}$", "too complex to validate"},
	}
	for _, tt := range tests {
		_, err := ValidSystemNamespaces(t.Context(), tt.raw)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ValidSystemNamespaces(%#v) = %v, want an error containing %q", tt.raw, err, tt.want)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	}
}

func TestModes(t *testing.T) {
	want := []string{ModeImmediate, ModeAllowCompletion, ModeDeleteAfterTimeout}
	got := Modes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Modes() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if Modes()[0] != ModeImmediate {
		t.Error("Modes() returns shared storage; a caller's edit leaked into the next call")
	}
}

func TestValidSystemNamespacesAcceptsNarrowPatterns(t *testing.T) {
	for _, p := range []string{"^(team-infra)$", "^team-.*$", "infra", "^q0-drain$", "drain", "probe", "-", "[0-9]",
		"^(q|z9)$", "^[a-z]", "[a-z]", "^.{2,}$", "^[a-z0-9]{1,62}$", "\\B"} {
		if _, err := ValidSystemNamespaces(t.Context(), p); err != nil {
			t.Errorf("ValidSystemNamespaces(%q) = %v, want accepted", p, err)
		}
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	values := map[string]any{
		"global":       map[string]any{"nodeDrainer": map[string]any{"enabled": true}},
		"node-drainer": map[string]any{"systemNamespaces": "^(team-infra)$"},
	}
	if err := Apply(t.Context(), values, []string{"cert-manager"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	once := values[NodeDrainerKey].(map[string]any)["systemNamespaces"]
	if err := Apply(t.Context(), values, []string{"cert-manager"}); err != nil {
		t.Fatalf("Apply again: %v", err)
	}
	if twice := values[NodeDrainerKey].(map[string]any)["systemNamespaces"]; twice != once {
		t.Errorf("re-applying changed systemNamespaces:\n once  %v\n twice %v", once, twice)
	}
	want := SystemNamespacesPattern([]string{"cert-manager"}) + "|(?:^(team-infra)$)"
	if once != want {
		t.Errorf("systemNamespaces = %v, want %v", once, want)
	}
}

func TestMatchesEveryNamespace(t *testing.T) {
	for _, p := range []string{".*", "^.*$", "", "(?:)", "^(team-infra)$|", "a*", ".", "^.", ".$", "[a-z0-9]",
		"^[a-z0-9-]+$", "(?i)[A-Z0-9]", "^.{1,63}$", "\\b", "^", "$", "\\w", "^(?:[a-m]|[n-z]|[0-9])"} {
		if universal, decided, _ := matchesEveryNamespace(t.Context(), p); !universal || !decided {
			t.Errorf("matchesEveryNamespace(%q) = %v, %v, want true, true", p, universal, decided)
		}
	}
}

// TestMatchesEveryNamespaceAgreesWithRegexp checks every pattern judged
// universal against the real regexp engine on every namespace name of up to
// three characters.
func TestMatchesEveryNamespaceAgreesWithRegexp(t *testing.T) {
	var names []string
	var gen func(prefix string)
	gen = func(prefix string) {
		if prefix != "" && prefix[0] != '-' && prefix[len(prefix)-1] != '-' {
			names = append(names, prefix)
		}
		if len(prefix) == 3 {
			return
		}
		for _, r := range namespaceRunes {
			gen(prefix + string(r))
		}
	}
	gen("")
	for _, p := range []string{".*", "^(q|z9)$", "drain", "[a-z]", "^[a-z0-9]", "\\b", "\\B", "^.{2,}$", "[^-]",
		"[a-z0-9]$|^-", "(?i)Q|[^q]", "^[0-4]|^[5-9]|^[a-z]"} {
		if universal, decided, _ := matchesEveryNamespace(t.Context(), p); !universal || !decided {
			continue
		}
		re := regexp.MustCompile(p)
		for _, n := range names {
			if !re.MatchString(n) {
				t.Errorf("%q is judged to match every namespace but does not match %q", p, n)
				break
			}
		}
	}
}

// TestValidSystemNamespacesBoundsWork covers patterns whose search would
// otherwise run for tens of seconds: they must fail closed, and fast.
func TestValidSystemNamespacesBoundsWork(t *testing.T) {
	for _, p := range []string{
		"^.{1,63}$|^.*a.{12}$|" + strings.Repeat("a?", 300) + "x",
		strings.Repeat("[a-z]{1000}", 20), // short, but compiles past maxProgramInsts
	} {
		start := time.Now()
		_, err := ValidSystemNamespaces(t.Context(), p)
		if err == nil || !strings.Contains(err.Error(), "too complex to validate") {
			t.Errorf("ValidSystemNamespaces(%.40q...) = %v, want too complex to validate", p, err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("ValidSystemNamespaces(%.40q...) took %v", p, elapsed)
		}
	}
}

func TestValidSystemNamespacesHonorsContext(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ValidSystemNamespaces(canceled, ".*")
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)

	expired, cancelExpired := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancelExpired()
	<-expired.Done()
	_, err = ValidSystemNamespaces(expired, "^.{1,63}$|^.*a.{12}$")
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
}
