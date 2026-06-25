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

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-logr/logr"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"gopkg.in/yaml.v2"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// DiscoverOption configures a Discover call. Functional-options seam for
// future toggles (timeouts, namespace overrides, additional filters) without
// breaking the Discover signature.
type DiscoverOption func(*discoverOptions)

type discoverOptions struct {
	baseConfig       *config.LaunchKitConfig
	nodeSelector     map[string]string
	keepNamespace    bool
	logger           *logr.Logger
	collapseNicRails *bool
}

// WithBaseConfig seeds Discover with a caller-supplied LaunchKitConfig
// instead of the binary's embedded default. Useful when the caller has
// already loaded an l8k cluster-config.yaml (via ParseClusterConfig or
// config.LoadFullConfig) and wants discovery to overlay onto it rather than
// replace it. The supplied config is mutated in place; pass a copy if the
// caller needs to keep the original.
func WithBaseConfig(cfg *config.LaunchKitConfig) DiscoverOption {
	return func(o *discoverOptions) { o.baseConfig = cfg }
}

// WithNodeSelector restricts discovery to nodes matching the supplied label
// selector. Default: empty selector — all nodes that publish a NicDevice CR
// are considered. The selector is also persisted to the returned
// LaunchKitConfig's per-group nodeSelector when machine/GPU labels are
// unresolved (legacy fallback path).
func WithNodeSelector(sel map[string]string) DiscoverOption {
	return func(o *discoverOptions) { o.nodeSelector = sel }
}

// WithKeepNamespace, when true, leaves the bootstrap
// nic-configuration-daemon namespace in place after discovery completes.
// Default: tear it down on a clean exit. Useful for debugging a failed
// discovery; not recommended for production callers.
func WithKeepNamespace(keep bool) DiscoverOption {
	return func(o *discoverOptions) { o.keepNamespace = keep }
}

// WithCollapseNicRails overrides the rail-collapse policy. Default
// behavior matches the CLI's `--collapse-nic-rails` (true): one rail per
// NIC for single-port NICs, one rail per port for VPD-confirmed dual-port
// NICs. Setting false restores the legacy one-rail-per-PF mode. Use only
// when the caller needs to mirror a non-default CLI invocation; library
// consumers tracking the recommended topology should leave this alone.
func WithCollapseNicRails(collapse bool) DiscoverOption {
	return func(o *discoverOptions) { o.collapseNicRails = &collapse }
}

// WithLogger registers a logr.Logger as the controller-runtime global so
// l8k's internal log.Log.Info/V(1) lines flow into the caller's logger
// for the duration of the Discover call. Without this option, callers
// who never called ctrllog.SetLogger themselves see a one-time
// "log.SetLogger(...) was never called" warning on stderr and lose
// l8k's structured discovery output entirely.
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
	return func(o *discoverOptions) { o.logger = &logger }
}

// Discover walks the cluster, populates a LaunchKitConfig with the
// discovered hardware topology (per-group PFs, capabilities, kernel
// modules, machine and GPU types, fabric type, recommended firmware), and
// returns it. With no options, discovery starts from the binary's embedded
// default config — no filesystem layout is required on the host.
//
// Side effects: discovery is NOT read-only by design. It writes the
// nvidia.kubernetes-launch-kit.machine and .gpu labels to every node in a
// resolved group, and patches the existing NicClusterPolicy (if any) via
// server-side apply with field owner "l8k-discovery". Callers that need a
// side-effect-free probe should treat this as out of scope for the current
// library API.
//
// Inputs:
//   - ctx: bounds the discovery. The DaemonSet/pod waits below have their
//     own timeouts but honor ctx cancellation.
//   - kubeClient: a controller-runtime client. Required; nil returns an
//     error.
//   - restConfig: a REST config for the same cluster. Required for the
//     pod-exec probes that populate kernel-module lists, fabric type, GPU
//     topology, and machine/GPU types. Passing nil keeps discovery working
//     for the group bucketing and label-write phases but silently skips
//     the probes (the returned config will be less populated).
//
// kubeclient.New returns both a controller-runtime client and a *rest.Config
// suitable for these parameters.
func Discover(
	ctx context.Context,
	kubeClient client.Client,
	restConfig *rest.Config,
	opts ...DiscoverOption,
) (*config.LaunchKitConfig, error) {
	if kubeClient == nil {
		return nil, errors.New("Discover: kubeClient must not be nil")
	}

	o := discoverOptions{}
	for _, opt := range opts {
		opt(&o)
	}

	if o.logger != nil {
		ctrllog.SetLogger(*o.logger)
	}

	cfg := o.baseConfig
	if cfg == nil {
		def, err := config.DefaultLaunchKitConfig()
		if err != nil {
			return nil, fmt.Errorf("Discover: failed to load embedded default config: %w", err)
		}
		cfg = def
	}

	// CollapseNicRails defaults to true to match the CLI's
	// --collapse-nic-rails flag; library callers get the recommended
	// one-rail-per-NIC topology unless they explicitly opt out via
	// WithCollapseNicRails(false). The zero value of the bool field
	// (false) would silently produce legacy one-rail-per-PF output,
	// which is the wrong default for a library API meant to mirror CLI
	// behavior.
	collapseNicRails := true
	if o.collapseNicRails != nil {
		collapseNicRails = *o.collapseNicRails
	}

	p := &NetworkOperatorPlugin{
		NodeSelector:     o.nodeSelector,
		RESTConfig:       restConfig,
		KeepNamespace:    o.keepNamespace,
		CollapseNicRails: collapseNicRails,
	}
	if err := p.DiscoverClusterConfig(ctx, kubeClient, cfg); err != nil {
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
