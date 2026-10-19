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
//   - DiscoverNetwork: call l8k's DiscoverReadOnly() against the cluster
//     pointed at by KubeconfigPath. It lists nodes, the Network Operator's
//     nic-configuration-daemon pods, and the NicDevice objects that daemon
//     already publishes; it creates, patches, and deletes nothing. aicr
//     never grants pods/exec, so a daemon node whose NicDevice cannot be
//     verified fails the collector rather than being probed.
//
// Both modes go through the same in-package toMeasurement translator,
// which enforces the current single-group constraint.
package network

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	stdos "os"
	"sync"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/go-logr/logr"
	l8kkubeclient "github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	l8kdisc "github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/discovery"
	l8kui "github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// Seams for the live-discovery path so its error mapping can be exercised
// without a cluster. Production values are the l8k entry points.
var (
	newL8kClient     = l8kkubeclient.New
	discoverReadOnly = l8kdisc.DiscoverReadOnly
)

// ctrlLogOnce guards the process-wide controller-runtime logger install.
// l8k's discovery package logs through controller-runtime's global logger;
// without one, controller-runtime prints a one-time "log.SetLogger(...) was
// never called" warning to stderr and drops l8k's structured output.
var ctrlLogOnce sync.Once

func installControllerRuntimeLogger() {
	ctrlLogOnce.Do(func() {
		ctrllog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
	})
}

// slogOutput forwards l8k's user-facing messages (for example "Excluding N
// ready daemon node(s)…") to aicr's slog stream. Everything the embedded
// silent Output handles (progress, headers, sections) stays silent.
type slogOutput struct{ l8kui.Output }

func (slogOutput) Info(format string, args ...any) { slog.Info("l8k: " + fmt.Sprintf(format, args...)) }
func (slogOutput) Warning(format string, args ...any) {
	slog.Warn("l8k: " + fmt.Sprintf(format, args...))
}
func (slogOutput) Error(format string, args ...any) {
	slog.Error("l8k: " + fmt.Sprintf(format, args...))
}

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

	// DiscoverNetwork enables read-only live discovery via l8k's
	// DiscoverReadOnly. It requires the Network Operator's
	// nic-configuration-daemon to already be running; when it is not,
	// Collect returns ErrCodeNotFound and the snapshotter skips the
	// NetworkTopology measurement.
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
	slog.Info("collecting network topology via l8k read-only discovery")

	// The REST config is deliberately discarded. Passing it as
	// WithReadOnlyRESTConfig would let l8k fall back to pods/exec on a
	// Ready daemon node that has not published a NicDevice; aicr grants no
	// exec and fails closed instead, and l8k's error names the node.
	kc, _, err := newL8kClient(client.ResolveKubeconfigPath(c.KubeconfigPath))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "l8k discovery client init failed", err)
	}
	installControllerRuntimeLogger()

	// l8k bounds its wait for nic-configuration-daemon pods with its own
	// 5-minute timer only when the context carries no deadline; with a
	// deadline it polls until that deadline and then has nothing left for
	// the inventory calls, so any daemon pod that is not Ready fails the
	// whole discovery (NVIDIA/k8s-launch-kit#305). Hand l8k a context with
	// no deadline so its own bound applies, and keep cancellation wired so
	// CollectorNetworkTimeout — which must exceed that 5-minute wait — still
	// aborts the call.
	discCtx, cancelDisc := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelDisc()
	defer context.AfterFunc(ctx, cancelDisc)()
	discCtx = l8kui.WithOutput(discCtx, slogOutput{Output: l8kui.NewSilent()})

	cfg, err := discoverReadOnly(discCtx, kc)
	if err != nil && ctx.Err() != nil {
		// The parent expired or was canceled while l8k was running; report
		// that rather than whatever l8k surfaced on the canceled context.
		err = ctx.Err()
	}
	if err != nil {
		switch {
		case stderrors.Is(err, l8kdisc.ErrNotInstalled):
			return nil, errors.Wrap(errors.ErrCodeNotFound,
				"network operator NIC configuration daemon not found; NetworkTopology skipped", err)
		case apierrors.IsForbidden(err):
			return nil, errors.Wrap(errors.ErrCodeUnauthorized,
				"l8k read-only discovery denied by RBAC", err)
		case stderrors.Is(err, context.DeadlineExceeded), stderrors.Is(err, context.Canceled):
			return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout,
				"l8k read-only discovery cancelled or timed out")
		}
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "l8k read-only discovery failed", err)
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
