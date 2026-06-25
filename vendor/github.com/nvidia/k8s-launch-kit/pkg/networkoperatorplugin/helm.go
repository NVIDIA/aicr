// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

// Package networkoperatorplugin's helm.go wraps the Helm Go SDK so that
// `l8k deploy` can install or upgrade the network-operator chart in-process
// before applying the post-install CRs.
//
//   - InstallOrUpgrade is the only entry point. It fetches the chart tgz
//     from cfg.HelmRepoURL, decides install-vs-upgrade-vs-no-op by checking
//     the live release's chart version AND user-supplied values, and respects
//     the overwriteExisting toggle.
//   - Action.Configuration wiring lives in pkg/networkoperatorplugin/helmclient
//     so the preflight sub-package reuses the same client.
//   - Values diff logic lives in pkg/networkoperatorplugin/preflight
//     (DeepEqualValues) so the validate flow and the install gate share one
//     source of truth.

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage/driver"
	"k8s.io/client-go/rest"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	pkgerrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/helmclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/preflight"
)

// networkOperatorChartName is the chart name in the upstream NVIDIA helm
// repositories (both nvidia and nvstaging/mellanox publish it under this
// name). Combined with cfg.HelmRepoURL it resolves to a single tgz.
const networkOperatorChartName = "network-operator"

// ErrReleaseExistsWithDifferentValues indicates that a network-operator
// release is already installed in the target namespace, but its
// user-supplied values do not match the values l8k would install now.
// Caller passes overwriteExisting=true to promote to `helm upgrade --install`.
var ErrReleaseExistsWithDifferentValues = errors.New("network-operator release exists with different values")

// ErrReleaseExistsWithDifferentChartVersion indicates that a release is
// already installed but its chart version differs from what
// `--network-operator-release` pinned. Same remediation:
// overwriteExisting=true triggers a helm upgrade to converge.
var ErrReleaseExistsWithDifferentChartVersion = errors.New("network-operator release exists with different chart version")

// ErrReleaseStuckInPendingState indicates that a previous helm operation
// crashed mid-flight, leaving the release locked in a `pending-install`,
// `pending-upgrade`, or `pending-rollback` status. Helm refuses every
// follow-up operation with "another operation in progress" until the
// release is unstuck via `helm rollback` or `helm uninstall`.
var ErrReleaseStuckInPendingState = errors.New("network-operator release is stuck in pending state")

// InstallOrUpgrade installs the nvidia/network-operator helm chart into
// cfg.Namespace, or upgrades it when overwriteExisting=true. When the
// release already exists and BOTH the chart version AND user-supplied
// values match, the call is a no-op. When either diverges and
// overwriteExisting=false, returns one of the typed sentinels so the
// CLI layer can surface a clear remediation pointing at --overwrite-existing.
//
// timeout controls the Wait timeout passed to helm install/upgrade. dryRun,
// when true, threads through to action.Install.DryRun / action.Upgrade.DryRun.
func InstallOrUpgrade(
	ctx context.Context,
	restConfig *rest.Config,
	cfg *config.NetworkOperatorConfig,
	valuesYAML []byte,
	overwriteExisting bool,
	timeout time.Duration,
	dryRun bool,
) error {
	if cfg == nil {
		return pkgerrors.NewValidationError(
			"helm install requires NetworkOperator config",
			nil,
			"populate networkOperator in l8k-config.yaml or pass --network-operator-release",
		)
	}
	if cfg.HelmRepoURL == "" {
		return pkgerrors.NewValidationError(
			"helm install requires NetworkOperator.HelmRepoURL",
			nil,
			"set --network-operator-release (catalog supplies the URL) or set helmRepoURL in l8k-config.yaml",
		)
	}
	if cfg.Version == "" {
		return pkgerrors.NewValidationError(
			"helm install requires NetworkOperator.Version",
			nil,
			"set --network-operator-release (catalog supplies the version) or set version in l8k-config.yaml",
		)
	}

	chartVersion := strings.TrimPrefix(cfg.Version, "v")
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = helmclient.DefaultNamespace
	}

	actionCfg, err := helmclient.NewActionConfig(restConfig, namespace, helmclient.StorageDriver)
	if err != nil {
		return pkgerrors.NewClusterError(
			"failed to initialize helm action configuration",
			err,
			"verify the kubeconfig is valid and the cluster is reachable",
		)
	}

	generated, err := helmclient.UnmarshalValues(valuesYAML)
	if err != nil {
		return pkgerrors.NewValidationError(
			"failed to parse generated values.yaml",
			err,
			"re-run `l8k generate` to recreate values.yaml",
		)
	}

	loadChart := func() (*chart.Chart, error) {
		chartPath, cleanup, perr := pullChart(ctx, cfg.HelmRepoURL, networkOperatorChartName, chartVersion)
		if perr != nil {
			return nil, pkgerrors.NewDeploymentError(
				fmt.Sprintf("failed to fetch network-operator chart %s from %s", chartVersion, cfg.HelmRepoURL),
				perr,
				"verify the helm repository URL and that the chart version exists",
			)
		}
		defer cleanup()
		chrt, lerr := loader.Load(chartPath)
		if lerr != nil {
			return nil, pkgerrors.NewDeploymentError(
				"failed to load fetched network-operator chart",
				lerr,
				"the downloaded chart tgz appears to be corrupt; re-run deploy",
			)
		}
		return chrt, nil
	}

	return installOrUpgradeWithLoader(ctx, actionCfg, loadChart, generated, chartVersion, namespace, overwriteExisting, timeout, dryRun)
}

// installOrUpgradeWithLoader is the test seam for InstallOrUpgrade: it owns
// the existing-release / conflict / install-or-upgrade decision but
// delegates chart fetching to loadChart, which tests can override with an
// in-memory chart. Chart loading is lazy — the no-op path (release exists
// with matching chart version and values) avoids the chart download entirely.
//
// Conflict semantics (gates BOTH on chart version and values):
//   - existing chart version differs → ErrReleaseExistsWithDifferentChartVersion
//   - existing values differ          → ErrReleaseExistsWithDifferentValues
//   - both match                       → no-op
//   - overwriteExisting=true on either → helm upgrade
func installOrUpgradeWithLoader(
	ctx context.Context,
	actionCfg *action.Configuration,
	loadChart func() (*chart.Chart, error),
	generated map[string]interface{},
	chartVersion, namespace string,
	overwriteExisting bool,
	timeout time.Duration,
	dryRun bool,
) error {
	get := action.NewGet(actionCfg)
	existing, getErr := get.Run(helmclient.DefaultReleaseName)
	switch {
	case getErr == nil:
		// Stuck-release gate FIRST. Helm refuses every operation
		// (`another operation (install/upgrade/rollback) is in
		// progress`) when the release status is one of the pending
		// states — usually because a previous deploy crashed
		// mid-helm. Catching this upfront lets us return a clear
		// remediation instead of helm's mid-flight error string.
		if existing.Info != nil && isPendingStatus(existing.Info.Status) {
			return fmt.Errorf("%w: status=%s", ErrReleaseStuckInPendingState, existing.Info.Status)
		}

		// Chart version gate — a version drift is the more
		// impactful conflict and should be reported before values.
		deployedChartVersion := ""
		if existing.Chart != nil && existing.Chart.Metadata != nil {
			deployedChartVersion = existing.Chart.Metadata.Version
		}
		if deployedChartVersion != chartVersion {
			if !overwriteExisting {
				return fmt.Errorf("%w: deployed=%s expected=%s",
					ErrReleaseExistsWithDifferentChartVersion, deployedChartVersion, chartVersion)
			}
			chrt, lerr := loadChart()
			if lerr != nil {
				return lerr
			}
			return runUpgrade(ctx, actionCfg, chrt, generated, chartVersion, namespace, timeout, dryRun)
		}

		// Values gate — same diff logic the preflight values check
		// uses. Empty diff => no-op.
		deployed := existing.Config
		if deployed == nil {
			deployed = map[string]interface{}{}
		}
		if diffs := preflight.DeepEqualValues(deployed, generated); len(diffs) == 0 {
			return nil
		}
		if !overwriteExisting {
			return ErrReleaseExistsWithDifferentValues
		}
		chrt, lerr := loadChart()
		if lerr != nil {
			return lerr
		}
		return runUpgrade(ctx, actionCfg, chrt, generated, chartVersion, namespace, timeout, dryRun)

	case errors.Is(getErr, driver.ErrReleaseNotFound):
		chrt, lerr := loadChart()
		if lerr != nil {
			return lerr
		}
		return runInstall(ctx, actionCfg, chrt, generated, chartVersion, namespace, timeout, dryRun)

	default:
		return pkgerrors.NewClusterError(
			fmt.Sprintf("failed to read existing helm release %q", helmclient.DefaultReleaseName),
			getErr,
			"verify cluster connectivity and permissions to read helm release secrets",
		)
	}
}

func runInstall(
	ctx context.Context,
	actionCfg *action.Configuration,
	chrt *chart.Chart,
	values map[string]interface{},
	chartVersion, namespace string,
	timeout time.Duration,
	dryRun bool,
) error {
	inst := action.NewInstall(actionCfg)
	inst.ReleaseName = helmclient.DefaultReleaseName
	inst.Namespace = namespace
	inst.CreateNamespace = true
	inst.Wait = !dryRun
	inst.Timeout = timeout
	inst.DryRun = dryRun
	inst.Version = chartVersion

	if _, err := inst.RunWithContext(ctx, chrt, values); err != nil {
		return pkgerrors.NewDeploymentError(
			fmt.Sprintf("helm install of network-operator failed in namespace %s", namespace),
			err,
			"inspect the cluster events and pods in the namespace, then re-run deploy",
		)
	}
	return nil
}

func runUpgrade(
	ctx context.Context,
	actionCfg *action.Configuration,
	chrt *chart.Chart,
	values map[string]interface{},
	chartVersion, namespace string,
	timeout time.Duration,
	dryRun bool,
) error {
	upg := action.NewUpgrade(actionCfg)
	upg.Namespace = namespace
	upg.Install = true // `helm upgrade --install` UX
	upg.Wait = !dryRun
	upg.Timeout = timeout
	upg.DryRun = dryRun
	upg.Version = chartVersion

	if _, err := upg.RunWithContext(ctx, helmclient.DefaultReleaseName, chrt, values); err != nil {
		return pkgerrors.NewDeploymentError(
			fmt.Sprintf("helm upgrade of network-operator failed in namespace %s", namespace),
			err,
			"inspect the cluster events and pods in the namespace, then re-run deploy",
		)
	}
	return nil
}

// pullChart fetches a chart tarball from repoURL into a temp directory and
// returns the local path plus a cleanup func. Uses helm's downloader
// directly — no repo cache, no `helm repo add` side effects.
func pullChart(_ context.Context, repoURL, chartName, chartVersion string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "l8k-helm-chart-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp dir for chart pull: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	settings := cli.New()
	// Force the temp dir as both the repo config and cache, so we don't
	// touch the user's ~/.config/helm state.
	settings.RepositoryConfig = filepath.Join(tmpDir, "repositories.yaml")
	settings.RepositoryCache = tmpDir

	getters := getter.All(settings)
	chartURL, err := repo.FindChartInRepoURL(
		repoURL,
		chartName,
		chartVersion,
		"", // certFile
		"", // keyFile
		"", // caFile
		getters,
	)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("resolve %s@%s in %s: %w", chartName, chartVersion, repoURL, err)
	}

	dl := downloader.ChartDownloader{
		Out:              io.Discard,
		Verify:           downloader.VerifyNever,
		Getters:          getters,
		RepositoryConfig: settings.RepositoryConfig,
		RepositoryCache:  settings.RepositoryCache,
	}
	saved, _, err := dl.DownloadTo(chartURL, chartVersion, tmpDir)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("download %s: %w", chartURL, err)
	}
	return saved, cleanup, nil
}

// isPendingStatus reports whether the helm release is mid-operation
// (install/upgrade/rollback). These are the statuses that cause helm's
// "another operation in progress" lock — usually a sign that a previous
// l8k run was killed before helm could persist a terminal status.
func isPendingStatus(s release.Status) bool {
	switch s {
	case release.StatusPendingInstall,
		release.StatusPendingUpgrade,
		release.StatusPendingRollback:
		return true
	}
	return false
}
