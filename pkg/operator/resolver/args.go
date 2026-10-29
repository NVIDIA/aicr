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

package resolver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// MaxOverrideValueBytes bounds one encoded override value.
const MaxOverrideValueBytes = 4 << 10

// enabledPath is AICR's component toggle; AICR accepts it only through --set.
const enabledPath = "enabled"

// Condition reasons for spec fields AICR cannot express.
const (
	ReasonInvalidOverride  = "InvalidOverride"
	ReasonInvalidPlacement = "InvalidPlacement"
)

// RecipeArgs returns the recipe Job arguments: the runtime's intent,
// platform and profile, for service, accelerator and OS the declared
// criteria or else the snapshot's, and its recipe configuration,
// with JSON output to outputURI. Stating the snapshot's dimensions keeps AICR
// from relaxing one no recipe covers (at v1.0.0, os=amazonlinux becomes any)
// into a generic recipe: AICR fails instead with INVALID_REQUEST.
func RecipeArgs(spec *v1alpha1.ClusterRuntimeSpec, derived map[string]string, snapshotURI, outputURI string) []string {
	var args []string
	if snapshotURI != "" {
		args = append(args, "--snapshot", snapshotURI)
	}
	args = append(args, "--intent", spec.Intent)
	for _, f := range []struct{ flag, value string }{
		{"--platform", spec.Platform},
		{"--profile", spec.Profile},
	} {
		if f.value != "" {
			args = append(args, f.flag, f.value)
		}
	}
	declared := v1alpha1.DeclaredCriteria{}
	if spec.Criteria != nil {
		declared = *spec.Criteria
	}
	for _, f := range []struct{ flag, declared, derived string }{
		{"--service", declared.Service, derived["service"]},
		{"--accelerator", declared.Accelerator, derived["accelerator"]},
		{"--os", declared.OS, derived["os"]},
	} {
		switch {
		case f.declared != "":
			args = append(args, f.flag, f.declared)
		case f.derived != "":
			args = append(args, f.flag, f.derived)
		}
	}
	if c := spec.Configuration; c != nil && c.GKE != nil && len(c.GKE.TCPXOInterfaces) > 0 {
		args = append(args, "--gke-tcpxo-interfaces", tcpxoInterfacesFlag(c.GKE.TCPXOInterfaces))
	}
	return append(args, "--format", "json", "--output", outputURI)
}

// tcpxoInterfacesFlag renders the mapping as AICR's
// eth1=<network>,...,eth8=<network>, keeping the administrator's pair order,
// which AICR records as given.
func tcpxoInterfacesFlag(mapping []v1alpha1.NetworkInterfaceMapping) string {
	pairs := make([]string, 0, len(mapping))
	for _, m := range mapping {
		pairs = append(pairs, m.InterfaceName+"="+m.Network)
	}
	return strings.Join(pairs, ",")
}

// SnapshotArgs returns the snapshot Job arguments: AICR's agent
// runs in exact-ServiceAccount mode, pinned to the accelerated group's
// selector and tolerations, because AICR's default selector matches no node
// before GPU feature discovery runs. requests, when set, overrides the
// agent's resource requests, for test clusters.
func SnapshotArgs(p *v1alpha1.Placement, namespace, agentAccount, image, requests, outputURI string) ([]string, error) {
	args := []string{"--namespace", namespace, "--service-account-name", agentAccount, "--image", image}
	keys := make([]string, 0, len(p.Accelerated.NodeSelector))
	for k := range p.Accelerated.NodeSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--node-selector", k+"="+string(p.Accelerated.NodeSelector[k]))
	}
	for i := range p.Accelerated.Tolerations {
		flag, err := tolerationFlag(&p.Accelerated.Tolerations[i])
		if err != nil {
			return nil, operrors.New(operrors.ErrCodeInvalidRequest,
				fmt.Sprintf("placement.accelerated.tolerations[%d]: %v", i, err)).WithReason(ReasonInvalidPlacement)
		}
		args = append(args, "--toleration", flag)
	}
	if requests != "" {
		args = append(args, "--requests", requests)
	}
	return append(args, "--timeout", defaults.SnapshotAgentTimeout.String(), "--format", "json", "--output", outputURI), nil
}

// RenderArgs returns the bundle Job arguments: the recipe, the render flags
// and a push to outputRef.
func RenderArgs(spec *v1alpha1.ClusterRuntimeSpec, recipeURI, outputRef string, plainHTTP bool) ([]string, error) {
	flags, err := RenderFlags(spec)
	if err != nil {
		return nil, err
	}
	args := append([]string{"--recipe", recipeURI}, flags...)
	args = append(args, "--output", outputRef)
	if plainHTTP {
		args = append(args, "--plain-http")
	}
	return args, nil
}

// RenderFlags returns the `aicr bundle` flags that shape a render: the
// engine-neutral Helm layout with readiness gates and the runtime's placement
// and overrides. The plan records them, so the AICR CLI can render the same
// bundle from the plan's recipe (`kubectl aicr reproduce`).
func RenderFlags(spec *v1alpha1.ClusterRuntimeSpec) ([]string, error) {
	args := []string{"--deployer", "helm", "--readiness-hooks"}
	for _, g := range []struct {
		name  string
		group v1alpha1.NodeGroupPlacement
	}{{"system", spec.Placement.System}, {"accelerated", spec.Placement.Accelerated}} {
		keys := make([]string, 0, len(g.group.NodeSelector))
		for k := range g.group.NodeSelector {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--"+g.name+"-node-selector", k+"="+string(g.group.NodeSelector[k]))
		}
		for i := range g.group.Tolerations {
			flag, err := tolerationFlag(&g.group.Tolerations[i])
			if err != nil {
				return nil, operrors.New(operrors.ErrCodeInvalidRequest,
					fmt.Sprintf("placement.%s.tolerations[%d]: %v", g.name, i, err)).
					WithReason(ReasonInvalidPlacement)
			}
			args = append(args, "--"+g.name+"-node-toleration", flag)
		}
	}
	for i := range spec.Overrides {
		o := &spec.Overrides[i]
		flag, value, err := overrideFlag(o)
		if err != nil {
			return nil, operrors.New(operrors.ErrCodeInvalidRequest,
				fmt.Sprintf("override %s:%s: %v", o.Component, o.Path, err)).
				WithReason(ReasonInvalidOverride)
		}
		args = append(args, flag, value)
	}
	return args, nil
}

// tolerationFlag renders a toleration in AICR's key=value:effect form. AICR
// cannot express a toleration without an effect (other than tolerate-all),
// nor tolerationSeconds, so those are rejected rather than altered.
func tolerationFlag(t *corev1.Toleration) (string, error) {
	switch {
	case t.TolerationSeconds != nil:
		return "", fmt.Errorf("tolerationSeconds is not supported by AICR")
	case t.Operator == corev1.TolerationOpExists && t.Key == "" && t.Effect == "":
		return "*", nil
	case t.Effect == "" || t.Key == "":
		return "", fmt.Errorf("AICR requires a key and an effect")
	case t.Operator == corev1.TolerationOpExists:
		if t.Value != "" {
			return "", fmt.Errorf("operator Exists takes no value")
		}
		return t.Key + ":" + string(t.Effect), nil
	case t.Value == "":
		return "", fmt.Errorf("operator Equal needs a value")
	default:
		return t.Key + "=" + t.Value + ":" + string(t.Effect), nil
	}
}

// overrideFlag maps an override onto --set-json, except the component toggle,
// which AICR accepts only as a boolean through --set.
func overrideFlag(o *v1alpha1.Override) (flag, value string, err error) {
	raw := bytes.TrimSpace(o.Value.Raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return "", "", fmt.Errorf("value is not JSON")
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, raw); err != nil {
		return "", "", fmt.Errorf("value is not JSON")
	}
	if compact.Len() > MaxOverrideValueBytes {
		return "", "", fmt.Errorf("encoded value is %d bytes, over %d", compact.Len(), MaxOverrideValueBytes)
	}
	if o.Path == enabledPath {
		on, isBool := map[string]bool{"true": true, "false": false}[compact.String()]
		if !isBool {
			return "", "", fmt.Errorf("the enabled toggle takes true or false")
		}
		return "--set", fmt.Sprintf("%s:%s=%t", o.Component, enabledPath, on), nil
	}
	return "--set-json", fmt.Sprintf("%s:%s=%s", o.Component, o.Path, compact.String()), nil
}
