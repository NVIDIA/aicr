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

package recipe

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// concreteValues returns the registry's values for a dimension with the
// "any" wildcard filtered out, so the union test exercises only
// CoordinateFor's required concrete inputs.
func concreteValues(all []string) []string {
	out := make([]string, 0, len(all))
	for _, v := range all {
		if v == CriteriaAnyValue {
			continue
		}
		out = append(out, v)
	}
	return out
}

func TestCoordinateFor(t *testing.T) {
	tests := []struct {
		name    string
		c       *Criteria
		want    string // Coordinate.Path()
		wantErr bool
	}{
		{
			name: "eks h100 ubuntu training kubeflow",
			c: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKubeflow},
			want: "eks/h100-ubuntu/training-kubeflow",
		},
		{
			name: "gke h100 cos inference dynamo",
			c: &Criteria{Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorH100,
				OS: CriteriaOSCOS, Intent: CriteriaIntentInference, Platform: CriteriaPlatformDynamo},
			want: "gke/h100-cos/inference-dynamo",
		},
		{
			name: "bare intent when platform any",
			c: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining, Platform: CriteriaPlatformAny},
			want: "eks/h100-ubuntu/training",
		},
		{
			name: "bare intent when platform empty",
			c: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
				OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining},
			want: "eks/h100-ubuntu/training",
		},
		{name: "nil criteria", c: nil, wantErr: true},
		{name: "service any", c: &Criteria{Service: CriteriaServiceAny, Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining}, wantErr: true},
		{name: "service empty", c: &Criteria{Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining}, wantErr: true},
		{name: "accelerator any", c: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorAny, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining}, wantErr: true},
		{name: "os any", c: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSAny, Intent: CriteriaIntentTraining}, wantErr: true},
		{name: "intent any", c: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentAny}, wantErr: true},
		{name: "service contains slash", c: &Criteria{Service: CriteriaServiceType("acme/ncp"), Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining}, wantErr: true},
		{name: "platform contains slash", c: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining, Platform: CriteriaPlatformType("a/b")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CoordinateFor(tt.c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if got.Path() != tt.want {
				t.Errorf("Path() = %q, want %q", got.Path(), tt.want)
			}
			if got.String() != got.Path() {
				t.Errorf("String() = %q, want == Path() %q", got.String(), got.Path())
			}
		})
	}
}

// TestCoordinatePathFormatLock pins the exact canonical string. The five
// downstream consumers (TG2/TG3/TG4a/RQ1/GP4) hardcode the
// "<group>/<dashboard>/<tab>" form, so changing the delimiter or segment order
// is a breaking contract change and must fail here — even if every other test's
// want-strings were updated in lockstep.
func TestCoordinatePathFormatLock(t *testing.T) {
	got, err := CoordinateFor(&Criteria{
		Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
		OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKubeflow,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = "eks/h100-ubuntu/training-kubeflow"
	if got.Path() != want {
		t.Errorf("canonical path = %q, want %q — this string is a frozen contract; do not change it", got.Path(), want)
	}
}

// TestCoordinateForErrorNamesDimension verifies the fail-closed message names
// the offending dimension, as the doc comment and ADR-012 promise.
func TestCoordinateForErrorNamesDimension(t *testing.T) {
	_, err := CoordinateFor(&Criteria{
		Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorAny,
		OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining,
	})
	if err == nil {
		t.Fatal("expected error for \"any\" accelerator")
	}
	if !strings.Contains(err.Error(), "accelerator") {
		t.Errorf("error %q does not name the offending dimension %q", err.Error(), "accelerator")
	}
}

func TestCoordinateForRegistryUnionConcrete(t *testing.T) {
	r := NewCriteriaRegistry()

	services := concreteValues(r.AllServiceTypes())
	accelerators := concreteValues(r.AllAcceleratorTypes())
	oses := concreteValues(r.AllOSTypes())
	intents := concreteValues(r.AllIntentTypes())
	platforms := concreteValues(r.AllPlatformTypes())

	if len(services) == 0 {
		t.Fatal("no concrete service types in registry")
	}
	if len(accelerators) == 0 {
		t.Fatal("no concrete accelerator types in registry")
	}
	if len(oses) == 0 {
		t.Fatal("no concrete os types in registry")
	}
	if len(intents) == 0 {
		t.Fatal("no concrete intent types in registry")
	}
	if len(platforms) == 0 {
		t.Fatal("no concrete platform types in registry")
	}

	// concreteValues defends against a --data-seeded registry that surfaces the
	// "any" wildcard among real values; today's static lists never do. Prove the
	// wildcard still fails closed when it reaches CoordinateFor through this same
	// registry-driven path (the literal per-dimension cases in TestCoordinateFor
	// own the rest of that coverage).
	if _, err := CoordinateFor(&Criteria{
		Service: CriteriaServiceType(CriteriaAnyValue), Accelerator: CriteriaAcceleratorH100,
		OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining,
	}); err == nil {
		t.Fatal("wildcard service must fail closed")
	}

	// Cross-product of concrete service x accelerator x os x intent
	// (platform left unset) must always resolve and echo the inputs.
	for _, service := range services {
		for _, accelerator := range accelerators {
			for _, os := range oses {
				for _, intent := range intents {
					c := &Criteria{
						Service:     CriteriaServiceType(service),
						Accelerator: CriteriaAcceleratorType(accelerator),
						OS:          CriteriaOSType(os),
						Intent:      CriteriaIntentType(intent),
					}
					got, err := CoordinateFor(c)
					if err != nil {
						t.Fatalf("CoordinateFor(%s/%s/%s/%s) error = %v", service, accelerator, os, intent, err)
					}
					if got.Group != service {
						t.Errorf("Group = %q, want %q", got.Group, service)
					}
					if want := accelerator + "-" + os; got.Dashboard != want {
						t.Errorf("Dashboard = %q, want %q", got.Dashboard, want)
					}
					if got.Tab != intent {
						t.Errorf("Tab = %q, want %q", got.Tab, intent)
					}
				}
			}
		}
	}

	// Each concrete platform produces an "<intent>-<platform>" tab.
	for _, platform := range platforms {
		c := &Criteria{
			Service:     CriteriaServiceEKS,
			Accelerator: CriteriaAcceleratorH100,
			OS:          CriteriaOSUbuntu,
			Intent:      CriteriaIntentTraining,
			Platform:    CriteriaPlatformType(platform),
		}
		got, err := CoordinateFor(c)
		if err != nil {
			t.Fatalf("CoordinateFor(platform=%s) error = %v", platform, err)
		}
		if want := "training-" + platform; got.Tab != want {
			t.Errorf("Tab = %q, want %q", got.Tab, want)
		}
	}
}
