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

package snapshotter

import (
	stderrors "errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/agent"
	"github.com/NVIDIA/aicr/pkg/runid"
)

// AgentRolesConfig selects the ServiceAccount that WriteAgentRoleManifests
// renders the snapshot agent's RBAC for.
//
// There is deliberately no Kubeconfig field: writing the manifests contacts
// no cluster, so there is no connection to configure.
type AgentRolesConfig struct {
	// Namespace is the namespace of the ServiceAccount, and the namespace
	// the rendered Role and RoleBinding declare. Required.
	Namespace string

	// ServiceAccountName is the name of the ServiceAccount the rendered
	// bindings name as their subject. Required, and not verified to
	// exist — see WriteAgentRoleManifests.
	ServiceAccountName string

	// DiscoverNetwork also renders the cluster-scoped MUTATING rules that
	// `aicr snapshot --discover-network` needs, with a header enumerating
	// each one and the discovery step it exists for.
	DiscoverNetwork bool

	// RunID names the output directory (`snapshot-rbac-<RunID>`). Empty
	// generates one, which is the normal path; it is injectable so tests
	// and automation can pin a directory name.
	RunID string
}

// AgentRoleObject identifies one written manifest so a caller can report
// what landed where without re-deriving either the name or the file.
type AgentRoleObject struct {
	// Kind is the Kubernetes kind ("Role", "RoleBinding", "ClusterRole",
	// "ClusterRoleBinding").
	Kind string

	// Name is the object's metadata.name.
	Name string

	// Path is the manifest's path, including the output directory.
	Path string
}

// AgentRolesResult names the directory WriteAgentRoleManifests wrote and
// what it put there.
//
// It is snapshotter-owned rather than pkg/k8s/agent's own Manifest type so
// callers presenting the outcome — the CLI among them — need no dependency
// on the Kubernetes-facing package.
type AgentRolesResult struct {
	// Dir is the output directory, relative to the working directory the
	// call was made from.
	Dir string

	// RunID is the run ID the directory name was built from.
	RunID string

	Namespace          string
	ServiceAccountName string

	// Objects lists what was written, in the order the files apply.
	Objects []AgentRoleObject

	// DiscoverNetwork echoes AgentRolesConfig.DiscoverNetwork: it is the
	// difference between a read-only grant and one carrying cluster-scoped
	// mutating rules, so anything reporting this result can say which was
	// written.
	DiscoverNetwork bool
}

// WriteAgentRoleManifests writes the RBAC manifests that grant the snapshot
// agent's permissions to an operator-supplied ServiceAccount into a new
// `snapshot-rbac-<runID>` directory in the current working directory.
//
// It APPLIES NOTHING and contacts no cluster. No clientset is built, the
// ServiceAccount is never looked up, and no permission pre-flight runs — so
// the call succeeds with no kubeconfig and no cluster privileges at all. The
// operator reviews the files and then applies them:
//
//	kubectl apply -f snapshot-rbac-<runID>/
//
// and removes the grant with the matching delete:
//
//	kubectl delete -f snapshot-rbac-<runID>/
//
// The ServiceAccount named in ServiceAccountName is NOT verified to exist.
// That is a deliberate simplification of the earlier behavior, which failed
// with ErrCodeNotFound against the cluster: a mistyped name now yields
// manifests the operator inspects before applying, and the rendered
// RoleBinding tells them how to check.
//
// The directory must not already exist. Colliding with one returns
// ErrCodeConflict rather than overwriting, because the manifests an operator
// is midway through reviewing are exactly what must not change under them.
//
// The objects are outside every run's lifecycle: no run-ID label, never in a
// run's created-set, never deleted by run cleanup. Teardown is the
// operator's `kubectl delete`.
func WriteAgentRoleManifests(config *AgentRolesConfig) (*AgentRolesResult, error) {
	if config == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "agent roles config is required")
	}
	// Reject what can be rejected before touching the filesystem, so a bad
	// value never leaves a half-written directory behind.
	if strings.TrimSpace(config.Namespace) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"Namespace is required: it is the namespace the rendered Role and RoleBinding declare")
	}
	if strings.TrimSpace(config.ServiceAccountName) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"ServiceAccountName is required: the rendered bindings need a subject to name")
	}

	manifests, err := agent.BuildServiceAccountRoleManifests(agent.ManifestOptions{
		Namespace:          config.Namespace,
		ServiceAccountName: config.ServiceAccountName,
		DiscoverNetwork:    config.DiscoverNetwork,
	})
	if err != nil {
		return nil, err
	}

	runID := config.RunID
	if runID == "" {
		runID = runid.Generate()
	}
	dir := defaults.AgentRBACManifestDirPrefix + runID

	// Mkdir, not MkdirAll: an existing directory must fail rather than have
	// its contents joined by a second run's files.
	if mkErr := os.Mkdir(dir, defaults.AgentRBACManifestDirMode); mkErr != nil {
		if stderrors.Is(mkErr, fs.ErrExist) {
			return nil, errors.NewWithContext(errors.ErrCodeConflict,
				"refusing to overwrite the existing directory "+dir+
					"; move or delete it, or pass a different run ID",
				map[string]any{"directory": dir})
		}
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create the RBAC manifest directory", mkErr)
	}

	objects, writeErr := writeManifestFiles(dir, manifests)
	if writeErr != nil {
		// A partially written directory would block the retry with the
		// ErrCodeConflict above, so remove what this call created. The
		// write error is what the caller needs to see, not this one.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			slog.Warn("failed to remove the partially written RBAC manifest directory",
				"directory", dir, "error", rmErr)
		}
		return nil, writeErr
	}

	return &AgentRolesResult{
		Dir:                dir,
		RunID:              runID,
		Namespace:          config.Namespace,
		ServiceAccountName: config.ServiceAccountName,
		Objects:            objects,
		DiscoverNetwork:    config.DiscoverNetwork,
	}, nil
}

// writeManifestFiles writes each manifest into dir and returns what it
// wrote. os.WriteFile is used rather than an explicit Create/Write/Close: it
// reports the Close error, which for a writable handle is where a failed
// flush surfaces.
func writeManifestFiles(dir string, manifests []agent.Manifest) ([]AgentRoleObject, error) {
	objects := make([]AgentRoleObject, 0, len(manifests))
	for _, m := range manifests {
		path := filepath.Join(dir, m.FileName)
		if err := os.WriteFile(path, m.Content, defaults.AgentRBACManifestFileMode); err != nil {
			return nil, errors.WrapWithContext(errors.ErrCodeInternal,
				"failed to write the RBAC manifest", err,
				map[string]any{"path": path, "kind": m.Kind})
		}
		objects = append(objects, AgentRoleObject{Kind: m.Kind, Name: m.Name, Path: path})
	}
	return objects, nil
}
