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

// Command aicr-operator runs the AICR Operator manager.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/config"
	"github.com/NVIDIA/aicr/pkg/operator/controller"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/manager"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
	"github.com/NVIDIA/aicr/pkg/operator/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("operator exited", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opts := manager.DefaultOptions()
	cfg := config.Defaults()
	fs := flag.NewFlagSet("aicr-operator", flag.ContinueOnError)
	opts.BindFlags(fs)
	cfg.BindFlags(fs)
	logLevel := fs.String("log-level", "info", "Log level: debug, info, warn or error.")
	showVersion := fs.Bool("version", false, "Print the version and exit.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	info := version.Get()
	if *showVersion {
		fmt.Printf("%s (commit %s, built %s, %s)\n", info.Version, info.Commit, info.Date, info.GoVersion)
		return nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q: %w", *logLevel, err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	logger.Info("starting aicr-operator",
		"version", info.Version, "commit", info.Commit, "release", info.Release, "options", opts.String(),
		"aicrVersion", cfg.AICRVersion, "aicrImage", cfg.AICRImage, "registry", cfg.RegistryRepository,
		"bypassApprovals", cfg.BypassApprovals)
	if err := cfg.Validate(info.Release); err != nil {
		return err
	}
	if cfg.ValidationEnabled && os.Getenv("POD_NAME") == "" {
		return operrors.New(operrors.ErrCodeInvalidRequest,
			"validation needs POD_NAME, the operator's pod name, to bind evidence to the digest the operator runs as")
	}

	scheme, err := manager.NewScheme()
	if err != nil {
		return err
	}
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes client config: %w", err)
	}
	mgr, err := manager.New(restConfig, opts, scheme, controller.CacheOptions(cfg.OperatorNamespace, cfg.JobsNamespace, cfg.DiscoveryNamespace))
	if err != nil {
		return err
	}
	ctx := ctrl.SetupSignalHandler()
	if err = setupControllers(ctx, mgr, &cfg, info.Version); err != nil {
		return err
	}
	// Start returns on SIGTERM/SIGINT, or with an error when leadership is
	// lost; either way the process exits and a replica takes over.
	return mgr.Start(ctx)
}

// setupControllers registers the resolver, the rollout controller and, with
// the aws provider, the repo-creds refresher.
func setupControllers(ctx context.Context, mgr ctrlmanager.Manager, cfg *config.Config, operatorVersion string) error {
	provider, refresher, err := credentials(ctx, mgr.GetClient(), mgr.GetAPIReader(), cfg)
	if err != nil {
		return err
	}
	opts := registry.Options{Credentials: provider.Credential}
	if cfg.RegistryPlainHTTP {
		opts.PlainHTTPHosts = []string{cfg.Registry().Host}
	}
	reg := registry.New(opts)
	resolverController := &controller.ResolverReconciler{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Config: cfg, Registry: reg, OperatorVersion: operatorVersion,
		Credentials: provider, Events: mgr.GetEventRecorder(argocd.FieldManager),
	}
	if err = resolverController.SetupWithManager(mgr); err != nil {
		return err
	}
	engine := &argocd.Adapter{
		Writer: mgr.GetClient(), Reader: mgr.GetAPIReader(), Mapper: mgr.GetRESTMapper(),
		Target: argocd.Target{Namespace: cfg.EngineNamespace, Project: cfg.EngineProject, RuntimeName: v1alpha1.SingletonName},
	}
	rolloutController := &controller.RolloutReconciler{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Config: cfg, Engine: engine, Now: time.Now,
		Events: mgr.GetEventRecorder(argocd.FieldManager), Registry: reg, JobCredentials: provider,
	}
	// The chart passes the pod's own name, so evidence can bind the digest
	// the operator runs as.
	if pod := os.Getenv("POD_NAME"); pod != "" {
		self := &controller.OperatorImage{Reader: mgr.GetAPIReader(), Namespace: cfg.OperatorNamespace, Pod: pod, Container: "manager"}
		rolloutController.Operator = self.Get
	}
	if refresher != nil {
		rolloutController.Credentials = refresher
		if err = mgr.Add(refresher); err != nil {
			return fmt.Errorf("register the repo-creds refresher: %w", err)
		}
		metrics.Registry.MustRegister(refresher.Collectors()...)
	}
	metrics.Registry.MustRegister(controller.Collectors()...)
	return rolloutController.SetupWithManager(mgr)
}

// credentials builds the registry credential provider for the installation:
// ECR through the operator's AWS identity (EKS Pod Identity or IRSA, from the
// SDK's default chain), or an administrator's static Secret.
func credentials(ctx context.Context, writer client.Client, reader client.Reader, cfg *config.Config) (registry.Provider, *controller.RepoCredsRefresher, error) {
	host := cfg.Registry().Host
	if cfg.RegistryAuthProvider != registry.ProviderAWS {
		return &registry.StaticProvider{Reader: reader, Namespace: cfg.OperatorNamespace, SecretName: cfg.RegistryCredentialsSecret}, nil, nil
	}
	region, err := registry.ECRRegion(host)
	if err != nil {
		return nil, nil, err
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	provider := &registry.ECRProvider{Client: ecr.NewFromConfig(awsConfig), Host: host, Now: time.Now}
	refresher := &controller.RepoCredsRefresher{
		Client: writer, Reader: reader, Provider: provider,
		Namespace: cfg.EngineNamespace, Name: cfg.EngineRepoCredsSecret, Now: time.Now,
	}
	return provider, refresher, nil
}
