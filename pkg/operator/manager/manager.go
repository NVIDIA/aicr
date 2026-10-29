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

// Package manager builds the controller-runtime manager that hosts the
// operator's controllers.
package manager

import (
	"flag"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Options configures the manager process.
type Options struct {
	MetricsBindAddress      string
	HealthProbeBindAddress  string
	LeaderElection          bool
	LeaderElectionNamespace string
	GracefulShutdownTimeout time.Duration
}

// DefaultOptions returns the options used when no flag overrides them.
// Leader election is on: a second replica must never act as leader.
func DefaultOptions() Options {
	return Options{
		MetricsBindAddress:      defaults.MetricsBindAddress,
		HealthProbeBindAddress:  defaults.HealthProbeBindAddress,
		LeaderElection:          true,
		GracefulShutdownTimeout: defaults.GracefulShutdownTimeout,
	}
}

// BindFlags registers the options on fs, using the current values as defaults.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.MetricsBindAddress, "metrics-bind-address", o.MetricsBindAddress, "Address the metrics endpoint binds to; 0 disables it.")
	fs.StringVar(&o.HealthProbeBindAddress, "health-probe-bind-address", o.HealthProbeBindAddress, "Address the health probe endpoint binds to.")
	fs.BoolVar(&o.LeaderElection, "leader-elect", o.LeaderElection, "Enable leader election; disable only for a single local replica.")
	fs.StringVar(&o.LeaderElectionNamespace, "leader-election-namespace", o.LeaderElectionNamespace, "Namespace of the leader-election Lease; defaults to the pod's namespace.")
	fs.DurationVar(&o.GracefulShutdownTimeout, "graceful-shutdown-timeout", o.GracefulShutdownTimeout, "How long runnables get to stop after shutdown starts.")
}

// Validate rejects options that would leave the process unbounded or
// unobservable.
func (o Options) Validate() error {
	if o.HealthProbeBindAddress == "" || o.HealthProbeBindAddress == "0" {
		return operrors.New(operrors.ErrCodeInvalidRequest, "health probe address is required")
	}
	if o.GracefulShutdownTimeout <= 0 {
		return operrors.New(operrors.ErrCodeInvalidRequest, "graceful shutdown timeout must be positive")
	}
	return nil
}

// NewScheme returns the scheme with every type the operator reads or writes.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "register client-go types", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "register operator types", err)
	}
	return scheme, nil
}

// controllerOptions maps Options onto controller-runtime's manager options.
func (o Options) controllerOptions(scheme *runtime.Scheme, cacheOptions cache.Options) ctrl.Options {
	leaseDuration := defaults.LeaseDuration
	renewDeadline := defaults.RenewDeadline
	retryPeriod := defaults.RetryPeriod
	shutdown := o.GracefulShutdownTimeout
	return ctrl.Options{
		Scheme:                  scheme,
		Cache:                   cacheOptions,
		Metrics:                 metricsserver.Options{BindAddress: o.MetricsBindAddress},
		HealthProbeBindAddress:  o.HealthProbeBindAddress,
		LeaderElection:          o.LeaderElection,
		LeaderElectionID:        defaults.LeaderElectionID,
		LeaderElectionNamespace: o.LeaderElectionNamespace,
		// Release the lease on graceful shutdown so a successor need not wait
		// out LeaseDuration. Safe because the process exits as soon as the
		// manager stops; on lease loss Start returns an error and the process
		// exits too.
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &leaseDuration,
		RenewDeadline:                 &renewDeadline,
		RetryPeriod:                   &retryPeriod,
		GracefulShutdownTimeout:       &shutdown,
	}
}

// New builds a manager with health and readiness checks registered. The
// controllers supply cacheOptions, which scope informers to their RBAC.
func New(cfg *rest.Config, o Options, scheme *runtime.Scheme, cacheOptions cache.Options) (ctrlmanager.Manager, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(cfg, o.controllerOptions(scheme, cacheOptions))
	if err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "create manager", err)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "add health check", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeInternal, "add readiness check", err)
	}
	return mgr, nil
}

// String renders options for the startup log.
func (o Options) String() string {
	return fmt.Sprintf("metrics=%s health=%s leaderElection=%t leaderElectionNamespace=%q shutdown=%s",
		o.MetricsBindAddress, o.HealthProbeBindAddress, o.LeaderElection, o.LeaderElectionNamespace, o.GracefulShutdownTimeout)
}
