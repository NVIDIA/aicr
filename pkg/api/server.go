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

package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/discovery"
	"github.com/NVIDIA/aicr/pkg/errors"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/logging"
	"github.com/NVIDIA/aicr/pkg/openshell"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/server"
)

const (
	name           = "aicrd"
	versionDefault = "dev"
)

var (
	// overridden during build with ldflags to reflect actual version info
	// e.g., -X "github.com/NVIDIA/aicr/pkg/api.version=1.0.0"
	version = versionDefault
	commit  = "unknown"
	date    = "unknown"
)

// Serve starts the API server and blocks until shutdown.
// It configures logging, sets up routes, and handles graceful shutdown.
// Returns an error if the server fails to start or encounters a fatal error.
func Serve() error {
	// Install signal handling at the entrypoint so SIGTERM/SIGINT cancels
	// the context throughout pre-Run setup (allowlist parsing, bundler
	// creation) as well as during request handling.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logging.SetDefaultStructuredLogger(name, version)
	slog.Debug("starting",
		"name", name,
		"version", version,
		"commit", commit,
		"date", date,
	)

	// Parse allowlists from environment variables
	allowLists, err := recipe.ParseAllowListsFromEnv()
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to parse allowlists from environment", err)
	}

	if allowLists != nil {
		slog.Info("criteria allowlists configured",
			"accelerators", len(allowLists.Accelerators),
			"services", len(allowLists.Services),
			"intents", len(allowLists.Intents),
			"os_types", len(allowLists.OSTypes),
		)
		slog.Debug("criteria allowlists loaded",
			"accelerators", allowLists.AcceleratorStrings(),
			"services", allowLists.ServiceStrings(),
			"intents", allowLists.IntentStrings(),
			"os_types", allowLists.OSTypeStrings(),
		)
	}

	// Setup recipe handler
	rb := recipe.NewBuilder(
		recipe.WithVersion(version),
		recipe.WithAllowLists(allowLists),
	)

	// Setup bundle handler
	bb, err := bundler.New(
		bundler.WithAllowLists(allowLists),
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create bundler", err)
	}

	r := map[string]http.HandlerFunc{
		"/v1/recipe": rb.HandleRecipes,
		"/v1/query":  rb.HandleQuery,
		"/v1/bundle": bb.HandleBundles,
	}

	var serverOpts []server.Option
	serverOpts = append(serverOpts,
		server.WithName(name),
		server.WithVersion(version),
	)

	// Wire DNS-AID discovery when enabled via environment.
	// Accept the full set of stdlib truthy values ("true", "1", "T", "TRUE", ...)
	// rather than only the lowercase "true" literal so misconfiguration is loud.
	if discoveryEnabled() {
		opts, agentHandler, err := setupDiscovery()
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to setup discovery", err)
		}
		serverOpts = append(serverOpts, opts...)
		r["/v1/agents"] = agentHandler
	}

	serverOpts = append(serverOpts, server.WithHandler(r))

	// Create and run server
	s := server.New(serverOpts...)

	if err := s.Run(ctx); err != nil {
		slog.Error("server exited with error", "error", err)
		return errors.Wrap(errors.ErrCodeInternal, "server exited with error", err)
	}

	return nil
}

// discoveryEnabled reports whether DNS-AID discovery should be wired up. It
// honors the standard set of truthy values supported by strconv.ParseBool so
// "1", "True", "TRUE" all work, not just lowercase "true".
func discoveryEnabled() bool {
	v := os.Getenv("AICR_DISCOVERY_ENABLED")
	if v == "" {
		return false
	}
	enabled, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("invalid AICR_DISCOVERY_ENABLED value, treating as disabled",
			"value", v, "error", err)
		return false
	}
	return enabled
}

// saNamespacePath is the path to the K8s service-account namespace token.
// Held in a variable so tests can redirect it to a tempfile (or empty path
// to force the "default" fallback) and run deterministically off-cluster.
var saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// podNamespace returns the Kubernetes namespace this pod is running in.
// Reads from AICR_DISCOVERY_NAMESPACE env var, then the service account mount,
// and falls back to "default". The SA file content is whitespace-trimmed
// because some CRI runtimes append a trailing newline that would otherwise
// produce an invalid namespace string downstream.
func podNamespace() string {
	if ns := os.Getenv("AICR_DISCOVERY_NAMESPACE"); ns != "" {
		return strings.TrimSpace(ns)
	}
	if saNamespacePath != "" {
		if data, err := os.ReadFile(saNamespacePath); err == nil {
			if ns := strings.TrimSpace(string(data)); ns != "" {
				return ns
			}
		}
	}
	return "default"
}

// discoveryDomain returns the Kubernetes DNS domain for agent discovery.
// Reads from AICR_DISCOVERY_DOMAIN env var, then derives from podNamespace().
func discoveryDomain() string {
	if domain := os.Getenv("AICR_DISCOVERY_DOMAIN"); domain != "" {
		return domain
	}
	return podNamespace() + ".svc.cluster.local."
}

// setupDiscovery creates the DNS-AID publisher, discoverer, and lifecycle hooks.
// Returns server options for OnStart/OnShutdown and the /v1/agents handler.
func setupDiscovery() ([]server.Option, http.HandlerFunc, error) {
	clientset, _, err := k8sclient.GetKubeClient()
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to create K8s client for discovery", err)
	}

	ns := podNamespace()
	domain := discoveryDomain()

	pub := discovery.NewPublisher(clientset,
		discovery.WithNamespace(ns),
		discovery.WithLabels(map[string]string{"app.kubernetes.io/managed-by": name}),
	)

	// Use the same PORT resolution as pkg/server so the agent registration
	// always advertises the port the server actually binds to.
	reg := discovery.AgentRegistration{
		Name:         name,
		Protocol:     discovery.ProtocolMCP,
		Namespace:    ns,
		ServiceName:  name,
		Port:         server.ResolvePort(defaults.ServerDefaultPort),
		Capabilities: []string{"recipe", "bundle", "validate", "query"},
		Version:      version,
	}

	disc := discovery.NewDiscoverer()

	// Create OpenShell policy guard
	guardMode := openshell.ModePermissive
	if mode := os.Getenv("OPENSHELL_MODE"); mode != "" {
		guardMode = openshell.Mode(mode)
		if !openshell.ValidMode(guardMode) {
			return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
				"invalid OPENSHELL_MODE: "+mode+" (valid: strict, permissive, disabled)")
		}
	}
	guard := openshell.NewGuard(
		openshell.WithMode(guardMode),
		openshell.WithCallerID(name),
		openshell.WithCallerDomain(domain),
	)
	slog.Info("openshell policy enforcement configured",
		slog.String("mode", string(guardMode)),
		slog.String("caller_id", name),
		slog.String("caller_domain", domain),
	)

	onStart := server.WithOnStart(func(ctx context.Context) error {
		slog.Info("publishing agent to DNS-AID",
			slog.String("name", reg.Name),
			slog.String("domain", domain),
		)
		return pub.Publish(ctx, reg)
	})

	onShutdown := server.WithOnShutdown(func(ctx context.Context) error {
		slog.Info("deregistering agent from DNS-AID",
			slog.String("name", reg.Name),
		)
		return pub.Deregister(ctx, reg.Name, reg.Protocol)
	})

	handler := handleAgents(disc, guard, domain)

	return []server.Option{onStart, onShutdown}, handler, nil
}
