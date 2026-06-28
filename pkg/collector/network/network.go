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

// Package network collects per-hardware-group network topology by ingesting
// k8s-launch-kit (l8k) cluster-config data and translating it into the
// NetworkTopology Measurement shape documented in
// docs/integrator/measurement-api.md.
//
// Two source modes:
//
//   - ClusterConfigPath: parse a pre-existing cluster-config.yaml file (no
//     cluster contact). Air-gap / CI / reproducible-input friendly.
//   - DiscoverNetwork: call l8k's live Discover() against the cluster
//     pointed at by KubeconfigPath, mutating cluster state (writes
//     nvidia.kubernetes-launch-kit.* labels and patches NicClusterPolicy
//     via server-side-apply).
//
// Both modes go through the same in-package toMeasurement translator,
// which enforces the current single-group constraint.
package network

import (
	"context"
	stderrors "errors"
	"io"
	"io/fs"
	"log/slog"
	stdos "os"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/go-logr/logr"
	l8kconfig "github.com/nvidia/k8s-launch-kit/pkg/config"
	l8kkubeclient "github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	l8kdisc "github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/discovery"
)

// Collector ingests an l8k cluster-config (from disk or live discovery)
// and emits the corresponding NetworkTopology Measurement.
//
// Fields are set directly by the factory (mirrors how pkg/collector/topology
// is wired). ClusterConfigPath and DiscoverNetwork are mutually exclusive
// — ClusterConfigPath wins when both are set so callers can default
// DiscoverNetwork from a flag without inadvertent cluster contact.
//
// When neither field is set, Collect returns (nil, nil) — the collector
// is "inactive" and the snapshotter treats nil Measurement as a no-op.
type Collector struct {
	// ClusterConfigPath is the filesystem path to an l8k
	// cluster-config.yaml. When set, Collect parses the file and
	// translates the result. No cluster contact.
	ClusterConfigPath string

	// DiscoverNetwork enables live discovery via l8k. Discovery is NOT
	// read-only — it writes labels and patches NicClusterPolicy.
	DiscoverNetwork bool

	// KubeconfigPath overrides the kubeconfig used by the live-discovery
	// path. Empty falls back to client.ResolveKubeconfigPath's chain
	// (KUBECONFIG env → ~/.kube/config → in-cluster).
	KubeconfigPath string
}

// Collect runs the configured source mode and returns a NetworkTopology
// Measurement, or (nil, nil) when the collector is inactive.
func (c *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorNetworkTimeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "network collector context cancelled", err)
	}

	switch {
	case c.ClusterConfigPath != "":
		return c.collectFromFile()
	case c.DiscoverNetwork:
		return c.collectViaDiscovery(ctx)
	default:
		// Inactive collector — neither --cluster-config nor
		// --discover-network was supplied. The snapshotter's
		// collectSafe wrapper explicitly treats a nil Measurement as
		// a no-op (see pkg/snapshotter/snapshot.go), so returning
		// (nil, nil) is the documented "nothing to emit" signal here.
		//nolint:nilnil // intentional: nil/nil is the inactive-collector contract
		return nil, nil
	}
}

// ctxKeyPath is the structured-error / slog field name carrying the
// cluster-config file path. Hoisted out of the per-call literals so the
// goconst lint stays happy and so future field renames are one edit.
const ctxKeyPath = "path"

func (c *Collector) collectFromFile() (*measurement.Measurement, error) {
	slog.Info("collecting network topology from cluster-config file",
		slog.String(ctxKeyPath, c.ClusterConfigPath))

	// os.Stat first so we can both reject oversize input before reading
	// (io.LimitReader would silently truncate, which can parse cleanly
	// and emit a partial NetworkTopology) and surface permission /
	// not-a-directory failures with their real codes instead of
	// flattening every Open error to ErrCodeNotFound.
	st, err := stdos.Stat(c.ClusterConfigPath)
	if err != nil {
		code := errors.ErrCodeInternal
		switch {
		case stderrors.Is(err, fs.ErrNotExist):
			code = errors.ErrCodeNotFound
		case stderrors.Is(err, fs.ErrPermission):
			code = errors.ErrCodeUnauthorized
		}
		return nil, errors.WrapWithContext(code,
			"failed to stat cluster-config file", err,
			map[string]any{ctxKeyPath: c.ClusterConfigPath})
	}
	if !st.Mode().IsRegular() {
		// FIFOs, devices, sockets, and dirs would all reach Open below.
		// Opening a FIFO for read blocks indefinitely until a writer
		// shows up — collectFromFile doesn't re-check ctx during that
		// wait, so CollectorNetworkTimeout can't abort it. Reject
		// non-regular files outright.
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"cluster-config path must point to a regular file",
			map[string]any{
				ctxKeyPath: c.ClusterConfigPath,
				"mode":     st.Mode().String(),
			})
	}
	if st.Size() > defaults.MaxClusterConfigBytes {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"cluster-config file exceeds size limit",
			map[string]any{
				ctxKeyPath: c.ClusterConfigPath,
				"size":     st.Size(),
				"limit":    defaults.MaxClusterConfigBytes,
			})
	}

	f, err := stdos.Open(c.ClusterConfigPath)
	if err != nil {
		code := errors.ErrCodeInternal
		switch {
		case stderrors.Is(err, fs.ErrNotExist):
			code = errors.ErrCodeNotFound
		case stderrors.Is(err, fs.ErrPermission):
			code = errors.ErrCodeUnauthorized
		}
		return nil, errors.WrapWithContext(code,
			"failed to open cluster-config file", err,
			map[string]any{ctxKeyPath: c.ClusterConfigPath})
	}
	defer f.Close() //nolint:errcheck // read-only handle

	// Read +1 over the cap as a defense-in-depth guard: the os.Stat
	// check above is the primary gate (it catches files that grew on
	// disk between Stat and Open just as well), but pairing it with a
	// LimitReader keeps parser memory bounded if a future change drops
	// the Stat path.
	limited := io.LimitReader(f, defaults.MaxClusterConfigBytes+1)

	cfg, err := l8kdisc.ParseClusterConfig(limited)
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
			"failed to parse cluster-config file", err,
			map[string]any{ctxKeyPath: c.ClusterConfigPath})
	}

	m, err := toMeasurement(cfg)
	if err != nil {
		return nil, err
	}
	logComplete(m)
	return m, nil
}

func (c *Collector) collectViaDiscovery(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting network topology via l8k discovery")

	kc, restCfg, err := l8kkubeclient.New(client.ResolveKubeconfigPath(c.KubeconfigPath))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "l8k discovery client init failed", err)
	}

	// l8k's discovery sub-package requires a LaunchKitConfig as a
	// positional arg — it reads the daemon image triple
	// (Repository / ComponentVersion / ImagePullSecrets) from it to
	// bootstrap the nic-configuration daemon and writes the discovered
	// per-group topology back into the same struct. The embedded default
	// ships with l8k's recommended Network Operator release line baked
	// in (currently 26.4); aicr doesn't pin a release at snapshot time
	// — that decision happens later in the recipe/deploy path.
	cfg, err := l8kconfig.DefaultLaunchKitConfig()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "l8k default config load failed", err)
	}

	// Route l8k's controller-runtime log lines through aicr's slog so
	// the discovery summary, preset matching, and per-PF debug output
	// appear on the same stream as the rest of the snapshotter. Without
	// this, controller-runtime would emit a one-time "log.SetLogger(...)
	// was never called" warning to stderr and drop l8k's structured
	// output.
	cfg, err = l8kdisc.Discover(ctx, kc, restCfg, cfg,
		l8kdisc.WithLogger(logr.FromSlogHandler(slog.Default().Handler())))
	if err != nil {
		// Surface deadline/cancel as ErrCodeTimeout so the 10-minute
		// CollectorNetworkTimeout (or a parent cancellation) isn't
		// reported as a cluster-availability problem. Only wrap
		// non-context errors as Unavailable.
		switch {
		case stderrors.Is(err, context.DeadlineExceeded), stderrors.Is(err, context.Canceled):
			return nil, errors.Wrap(errors.ErrCodeTimeout, "l8k discovery cancelled or timed out", err)
		}
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "l8k discovery failed", err)
	}

	m, err := toMeasurement(cfg)
	if err != nil {
		return nil, err
	}
	logComplete(m)
	return m, nil
}

// logComplete emits the phase-summary line for a successful collection,
// matching the shape topology uses ("node topology collection complete").
// Reads from the Measurement we just built so the summary is
// self-describing without re-walking the source LaunchKitConfig.
func logComplete(m *measurement.Measurement) {
	if m == nil {
		return
	}
	identifier, machineType, gpuType := "", "", ""
	var pfCount, railCount int64
	if id := m.GetSubtype(subtypeIdentity); id != nil {
		identifier = id.Context[identityCtxIdentifier]
		machineType = id.Context[identityCtxMachineType]
		gpuType = id.Context[identityCtxGPUType]
		if v, err := id.GetInt64(identityDataPFCount); err == nil {
			pfCount = v
		}
		if v, err := id.GetInt64(identityDataRailCount); err == nil {
			railCount = v
		}
	}
	slog.Info("network topology collection complete",
		slog.String("identifier", identifier),
		slog.String("machineType", machineType),
		slog.String("gpuType", gpuType),
		slog.Int64("pfs", pfCount),
		slog.Int64("rails", railCount),
	)
}
