// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

// Package discovery is the library-friendly entry point for l8k's cluster
// discovery: it walks the cluster, bootstraps the nic-configuration daemon,
// and populates a LaunchKitConfig with discovered hardware (per-group PFs,
// capabilities, kernel modules, machine/GPU types, fabric type, recommended
// firmware).
//
// The package is intentionally helm-clean — importing it must not pull in
// the Helm Go SDK or any of its transitive deps. The deploy path
// (network-operator install/upgrade, manifest validation) lives in the
// parent pkg/networkoperatorplugin package and is reachable from there for
// callers that need it; library consumers like aicr that only need
// discovery should depend on this sub-package alone.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-logr/logr"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"gopkg.in/yaml.v2"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// Options is the data carrier the discovery package uses internally. Library
// callers normally construct it indirectly by passing DiscoverOption values
// to Discover; it is exported so the parent networkoperatorplugin package's
// thin DiscoverClusterConfig wrapper can populate it without going through
// the functional-option API.
type Options struct {
	// NodeSelector restricts discovery to nodes matching this label
	// selector. The default empty selector considers every node that
	// publishes a NicDevice CR. The selector is also persisted to the
	// returned LaunchKitConfig's per-group nodeSelector when
	// machine/GPU labels are unresolved (legacy fallback path).
	NodeSelector map[string]string

	// KeepNamespace, when true, suppresses teardown of the
	// nicconfigdaemon bootstrap namespace at the end of discovery. Useful
	// for debugging; not recommended for production.
	KeepNamespace bool

	// CollapseNicRails reduces multi-PF NICs to a single rail per NIC
	// (master-PF-only) for non-dual-port adapters; dual-port NICs keep
	// every port (one rail per port). Default true to match the CLI's
	// --collapse-nic-rails flag — library callers get the recommended
	// one-rail-per-NIC topology unless they explicitly opt out.
	CollapseNicRails bool

	// Logger overrides controller-runtime's global logger for the
	// duration of the discovery call so internal log.Log.Info / V(1)
	// lines flow into the caller's logger. Without this, callers who
	// never called ctrllog.SetLogger themselves see a one-time
	// "log.SetLogger(...) was never called" warning on stderr and lose
	// l8k's structured discovery output.
	//
	// Pointer-typed so a nil value means "no override" rather than the
	// zero logr.Logger which would discard everything.
	Logger *logr.Logger

	// Release, when non-empty, is the MAJOR.MINOR Network Operator
	// release line to pin into cfg before discovery runs. The supplied
	// value is resolved against the embedded release catalog; the
	// resolved version, repositories, helm-repo URL, and DOCA-driver
	// version are written into cfg. Empty means "leave whatever is
	// already in cfg" — the default config ships with the currently
	// recommended line baked in.
	Release string
}

// DiscoverOption configures a Discover call. Functional-options seam for
// future toggles (timeouts, namespace overrides, additional filters) without
// breaking the Discover signature.
type DiscoverOption func(*Options)

// WithNodeSelector restricts discovery to nodes matching the supplied label
// selector. Default: empty selector — all nodes that publish a NicDevice CR
// are considered.
func WithNodeSelector(sel map[string]string) DiscoverOption {
	return func(o *Options) { o.NodeSelector = sel }
}

// WithKeepNamespace, when true, leaves the bootstrap
// nic-configuration-daemon namespace in place after discovery completes.
// Default: tear it down on a clean exit. Useful for debugging a failed
// discovery; not recommended for production callers.
func WithKeepNamespace(keep bool) DiscoverOption {
	return func(o *Options) { o.KeepNamespace = keep }
}

// WithCollapseNicRails overrides the rail-collapse policy. Default
// behavior matches the CLI's `--collapse-nic-rails` (true): one rail per
// NIC for single-port NICs, one rail per port for VPD-confirmed dual-port
// NICs. Setting false restores the legacy one-rail-per-PF mode.
func WithCollapseNicRails(collapse bool) DiscoverOption {
	return func(o *Options) { o.CollapseNicRails = collapse }
}

// WithLogger registers a logr.Logger as the controller-runtime global so
// l8k's internal log.Log.Info/V(1) lines flow into the caller's logger
// for the duration of the Discover call. Without this option, callers
// who never called ctrllog.SetLogger themselves see a one-time
// "log.SetLogger(...) was never called" warning on stderr and lose
// l8k's structured discovery output.
//
// External library consumers wiring l8k into a slog-based application
// can pass `logr.FromSlogHandler(slog.Default().Handler())` here. The
// l8k CLI does NOT need this option — it configures its own logger
// during startup and would override that choice if Discover set one.
//
// Note: ctrllog.SetLogger mutates a process-wide global. Calls from
// concurrent goroutines race; the option is intended for single-shot
// integration at the call site that owns logging policy.
func WithLogger(logger logr.Logger) DiscoverOption {
	return func(o *Options) { o.Logger = &logger }
}

// WithRelease pins the Network Operator release line that discovery
// writes into the supplied cfg. The value (e.g. "26.4") is resolved
// against l8k's embedded release catalog (see
// pkg/networkoperatorplugin/releases) and the resolved version,
// componentVersion, repositories, helm-repo URL, and DOCA-driver version
// are written into cfg.NetworkOperator / cfg.DOCADriver before discovery
// starts. When unset, cfg's existing fields are preserved — the embedded
// default config (config.DefaultLaunchKitConfig) ships with the currently
// recommended release baked in, so callers who don't care about pinning
// can skip this option.
//
// Use releases.SupportedReleases() and releases.LookupRelease(...) for
// catalog enumeration / validation outside Discover.
func WithRelease(release string) DiscoverOption {
	return func(o *Options) { o.Release = release }
}

// Discover walks the cluster and populates cfg with the discovered
// hardware topology. cfg is required and is mutated in place — the
// returned pointer is the same one passed in (returned for chaining
// convenience). With no DiscoverOption supplied, discovery uses cfg's
// existing release / daemon-image / selector fields verbatim; the
// embedded default config ships with the currently-recommended Network
// Operator release line, so callers who want l8k's defaults can pass
// config.DefaultLaunchKitConfig() and call it a day.
//
// Side effects: discovery is NOT read-only. It writes the
// nvidia.kubernetes-launch-kit.machine and .gpu labels to every node in
// a resolved group, and patches the existing NicClusterPolicy (if any)
// via server-side apply with field owner "l8k-discovery". Callers that
// need a side-effect-free probe should treat this as out of scope for
// the current library API.
//
// Inputs:
//   - ctx: bounds the discovery. The DaemonSet/pod waits below have
//     their own timeouts but honor ctx cancellation.
//   - kubeClient: a controller-runtime client. Required.
//   - restConfig: a REST config for the same cluster. Required for the
//     pod-exec probes that populate kernel-module lists, fabric type,
//     GPU topology, and machine/GPU types. Passing nil keeps discovery
//     working for the group-bucketing and label-write phases but
//     silently skips the probes (cfg will be less populated).
//   - cfg: the LaunchKitConfig to mutate. Required, non-nil. Must
//     carry a NetworkOperator section (Repository, ComponentVersion,
//     ImagePullSecrets) — used to construct the bootstrap daemon image
//     reference. Use config.DefaultLaunchKitConfig() if you don't have
//     a user-supplied cluster-config.yaml.
//
// kubeclient.New returns both a controller-runtime client and a *rest.Config
// suitable for these parameters.
func Discover(
	ctx context.Context,
	kubeClient client.Client,
	restConfig *rest.Config,
	cfg *config.LaunchKitConfig,
	opts ...DiscoverOption,
) (*config.LaunchKitConfig, error) {
	if kubeClient == nil {
		return nil, errors.New("Discover: kubeClient must not be nil")
	}
	if cfg == nil {
		return nil, errors.New("Discover: cfg must not be nil")
	}

	o := Options{CollapseNicRails: true}
	for _, opt := range opts {
		opt(&o)
	}

	if o.Logger != nil {
		ctrllog.SetLogger(*o.Logger)
	}

	if o.Release != "" {
		if err := applyRelease(cfg, o.Release); err != nil {
			return nil, err
		}
	}

	if err := DiscoverClusterConfig(ctx, kubeClient, restConfig, cfg, o); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ParseClusterConfig reads an l8k cluster-config.yaml from r and returns the
// parsed LaunchKitConfig. This is the library-mode equivalent of
// config.LoadFullConfig for the case where the YAML bytes are already in
// memory (no filesystem hop) — useful for callers loading the config from
// a ConfigMap, a Secret, an HTTP response, or any other in-memory source.
//
// Behavior parity with LoadFullConfig:
//   - Unknown fields are silently ignored (lenient unmarshal).
//   - When the config has explicit nvIpam.subnets entries, the reserved
//     first/last IP ranges are finalized into each subnet's Exclusions
//     list via ApplyReservedExclusions. This MUST happen at load time
//     because l8k never rewrites nvIpam back to disk — a downstream
//     generate/deploy that uses the parsed config will otherwise allocate
//     IPPool entries from addresses that the user asked to be reserved.
//
// Callers that need stricter validation should call ValidateClusterConfig
// on the returned value.
func ParseClusterConfig(r io.Reader) (*config.LaunchKitConfig, error) {
	if r == nil {
		return nil, errors.New("ParseClusterConfig: reader must not be nil")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("ParseClusterConfig: read failed: %w", err)
	}
	var cfg config.LaunchKitConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("ParseClusterConfig: yaml unmarshal failed: %w", err)
	}
	if cfg.NvIpam != nil && len(cfg.NvIpam.Subnets) > 0 {
		if err := config.ApplyReservedExclusions(
			cfg.NvIpam.Subnets, cfg.NvIpam.ReserveFirstIPs, cfg.NvIpam.ReserveLastIPs); err != nil {
			return nil, fmt.Errorf("ParseClusterConfig: invalid nvIpam config: %w", err)
		}
	}
	return &cfg, nil
}

// applyRelease resolves the supplied release key against the embedded
// catalog and writes the resolved fields into cfg. Mirrors
// pkg/networkoperatorplugin.ApplyNetworkOperatorRelease, minus the CLI
// Options-struct dependency.
func applyRelease(cfg *config.LaunchKitConfig, release string) error {
	rel, ok := releases.LookupRelease(release)
	if !ok {
		return fmt.Errorf("unsupported network operator release %q; supported: %v",
			release, releases.SupportedReleases())
	}
	if cfg.NetworkOperator == nil {
		cfg.NetworkOperator = &config.NetworkOperatorConfig{}
	}
	cfg.NetworkOperator.SelectedRelease = release
	cfg.NetworkOperator.Version = rel.NetworkOperator.Version
	cfg.NetworkOperator.ComponentVersion = rel.NetworkOperator.ComponentVersion
	cfg.NetworkOperator.Repository = rel.NetworkOperator.Repository
	cfg.NetworkOperator.OperatorRepository = rel.NetworkOperator.OperatorRepository
	cfg.NetworkOperator.HelmRepoURL = rel.NetworkOperator.HelmRepoURL
	if cfg.DOCADriver == nil {
		cfg.DOCADriver = &config.DOCADriverConfig{
			UnloadStorageModules:        true,
			UnloadThirdPartyRDMAModules: true,
			SkipPreflightChecks:         false,
		}
	}
	cfg.DOCADriver.Version = rel.DOCADriver.Version
	return nil
}

