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

package manager

import (
	"flag"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

func TestDefaultOptionsAreValid(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	if err := o.Validate(); err != nil {
		t.Fatalf("DefaultOptions().Validate() = %v", err)
	}
	if !o.LeaderElection {
		t.Fatal("leader election must be on by default")
	}
}

func TestBindFlagsOverridesDefaults(t *testing.T) {
	t.Parallel()
	o := DefaultOptions()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	o.BindFlags(fs)
	err := fs.Parse([]string{
		"--metrics-bind-address=0",
		"--health-probe-bind-address=:9443",
		"--leader-elect=false",
		"--leader-election-namespace=aicr-system",
		"--graceful-shutdown-timeout=5s",
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	want := Options{
		MetricsBindAddress:      "0",
		HealthProbeBindAddress:  ":9443",
		LeaderElection:          false,
		LeaderElectionNamespace: "aicr-system",
		GracefulShutdownTimeout: 5 * time.Second,
	}
	if o != want {
		t.Fatalf("options = %+v, want %+v", o, want)
	}
}

func TestValidateRejectsUnboundedOrUnobservable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no health probe", func(o *Options) { o.HealthProbeBindAddress = "" }},
		{"disabled health probe", func(o *Options) { o.HealthProbeBindAddress = "0" }},
		{"zero shutdown", func(o *Options) { o.GracefulShutdownTimeout = 0 }},
		{"negative shutdown", func(o *Options) { o.GracefulShutdownTimeout = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := DefaultOptions()
			tt.mutate(&o)
			err := o.Validate()
			if operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
				t.Fatalf("Validate() = %v, want INVALID_REQUEST", err)
			}
		})
	}
}

func TestControllerOptionsMapping(t *testing.T) {
	t.Parallel()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v", err)
	}
	o := DefaultOptions()
	o.LeaderElectionNamespace = "aicr-system"
	co := o.controllerOptions(scheme, cache.Options{})

	if co.Scheme != scheme {
		t.Error("scheme not passed through")
	}
	if co.LeaderElectionID != defaults.LeaderElectionID || co.LeaderElectionNamespace != "aicr-system" || !co.LeaderElection {
		t.Errorf("leader election = %v %q %q", co.LeaderElection, co.LeaderElectionID, co.LeaderElectionNamespace)
	}
	if !co.LeaderElectionReleaseOnCancel {
		t.Error("lease must be released on graceful shutdown")
	}
	if *co.LeaseDuration != defaults.LeaseDuration || *co.RenewDeadline != defaults.RenewDeadline || *co.RetryPeriod != defaults.RetryPeriod {
		t.Errorf("lease timings = %v %v %v", *co.LeaseDuration, *co.RenewDeadline, *co.RetryPeriod)
	}
	if *co.GracefulShutdownTimeout != o.GracefulShutdownTimeout {
		t.Errorf("shutdown = %v", *co.GracefulShutdownTimeout)
	}
	if co.Metrics.BindAddress != o.MetricsBindAddress || co.HealthProbeBindAddress != o.HealthProbeBindAddress {
		t.Errorf("addresses = %q %q", co.Metrics.BindAddress, co.HealthProbeBindAddress)
	}
}

// New must not contact the API server: construction happens before leader
// election, and an unreachable control plane must surface at Start, not here.
func TestNewDoesNotContactAPIServer(t *testing.T) {
	t.Parallel()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v", err)
	}
	o := DefaultOptions()
	o.MetricsBindAddress = "0"
	o.HealthProbeBindAddress = "127.0.0.1:0"
	o.LeaderElection = false

	mgr, err := New(&rest.Config{Host: "https://127.0.0.1:1"}, o, scheme, cache.Options{})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if mgr == nil {
		t.Fatal("New() returned a nil manager")
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v", err)
	}
	o := DefaultOptions()
	o.GracefulShutdownTimeout = 0
	if _, err := New(&rest.Config{Host: "https://127.0.0.1:1"}, o, scheme, cache.Options{}); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("New() = %v, want INVALID_REQUEST", err)
	}
}

func TestOptionsString(t *testing.T) {
	t.Parallel()
	s := DefaultOptions().String()
	for _, want := range []string{"metrics=:8080", "health=:8081", "leaderElection=true", "shutdown=30s"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}
