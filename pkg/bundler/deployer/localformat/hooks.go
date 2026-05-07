// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package localformat

import (
	"bytes"
	stderrors "errors"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// stripHelmHooks removes helm.sh/hook* annotations (helm.sh/hook,
// helm.sh/hook-weight, helm.sh/hook-delete-policy) from every document in a
// multi-doc YAML stream.
//
// Why we do this at the wrapping step:
//
// Local-helm wrapped charts emit their content into NNN-<name>/templates/
// alongside an NNN-prefix that drives deploy ordering at the bundle layer.
// That numeric ordering subsumes the role helm hooks play in upstream charts
// (apply CRD-dependent resources after the chart that installs them) — the
// dependent resource is in a later-numbered folder by construction. So the
// hook annotations no longer carry information at the bundle layer.
//
// Why leaving them in is harmful:
//
// Argo CD's Helm processor maps `helm.sh/hook: post-install` to its own
// `argocd.argoproj.io/hook: PostSync`, treating the resource as a hook
// rather than a regular sync resource. With `syncPolicy.automated`, Argo
// only runs a sync *operation* when there's something out-of-sync. A chart
// whose only resources are hook-annotated reports as "Synced" trivially
// (no comparable sync resources) and the PostSync hook never fires —
// the resource is silently never applied. Stripping at bundle time avoids
// this cross-deployer surprise.
func stripHelmHooks(rendered []byte) ([]byte, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		decodeErr := decoder.Decode(&doc)
		if stderrors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"failed to parse rendered manifest as YAML", decodeErr)
		}
		stripHooksFromDocument(&doc)
		// Skip separator-only / null documents so re-encoding doesn't emit
		// `null` content that would later defeat the hasYAMLObjects check.
		if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
			continue
		}
		docs = append(docs, &doc)
	}

	if len(docs) == 0 {
		// Empty or whitespace-only input — pass through unchanged.
		return rendered, nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"failed to re-emit YAML after stripping hook annotations", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to close YAML encoder", err)
	}
	return buf.Bytes(), nil
}

// syncPhaseHooks lists the helm.sh/hook phases whose role (ordering chart-
// dependent applies relative to the chart's CRDs) is replaced by the
// bundle-layer NNN folder ordering. Stripping these from rendered manifests
// avoids the "PostSync hook never fires under syncPolicy.automated" failure
// described in stripHelmHooks's doc.
//
// pre-delete and post-delete are deliberately NOT in this set: those are
// lifecycle hooks that fire on Application deletion, which folder ordering
// does NOT replace. Argo CD maps them to PreDelete/PostDelete phases —
// stripping them would convert the resource into a regular sync resource
// and silently lose the delete-time behavior.
var syncPhaseHooks = map[string]bool{
	"pre-install":   true,
	"post-install":  true,
	"pre-upgrade":   true,
	"post-upgrade":  true,
	"pre-rollback":  true,
	"post-rollback": true,
	"crd-install":   true,
	"test":          true,
	"test-success":  true,
	"test-failure":  true,
}

// stripHooksFromDocument navigates a Document node to metadata.annotations
// and removes helm.sh/hook* entries that name only sync-phase hooks.
// Lifecycle hooks (pre-delete, post-delete) are preserved; if a resource's
// helm.sh/hook value mixes sync-phase and lifecycle entries, the sync-phase
// entries are filtered out and the surviving lifecycle entries are written
// back. The associated helm.sh/hook-weight and helm.sh/hook-delete-policy
// annotations are kept iff helm.sh/hook itself survives, since they are
// meaningful only paired with a hook annotation. Silently no-ops when the
// document does not have the expected shape (non-mapping root, missing
// metadata, missing annotations).
func stripHooksFromDocument(doc *yaml.Node) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return
	}
	root := doc.Content[0]
	metadata := findMappingValueNode(root, "metadata")
	if metadata == nil {
		return
	}
	annotations := findMappingValueNode(metadata, "annotations")
	if annotations == nil || annotations.Kind != yaml.MappingNode {
		return
	}

	// Pass 1: decide whether helm.sh/hook survives, and with what value.
	keepHook, newHookValue := false, ""
	for i := 0; i+1 < len(annotations.Content); i += 2 {
		if annotations.Content[i].Value != "helm.sh/hook" {
			continue
		}
		var kept []string
		for _, phase := range strings.Split(annotations.Content[i+1].Value, ",") {
			phase = strings.TrimSpace(phase)
			if phase == "" || syncPhaseHooks[phase] {
				continue
			}
			kept = append(kept, phase)
		}
		if len(kept) > 0 {
			keepHook = true
			newHookValue = strings.Join(kept, ",")
		}
		break
	}

	// Pass 2: filter annotations. helm.sh/hook-weight and
	// helm.sh/hook-delete-policy are dropped iff helm.sh/hook is dropped.
	filtered := annotations.Content[:0]
	for i := 0; i+1 < len(annotations.Content); i += 2 {
		key := annotations.Content[i]
		val := annotations.Content[i+1]
		switch key.Value {
		case "helm.sh/hook":
			if !keepHook {
				continue
			}
			val.Value = newHookValue
		case "helm.sh/hook-weight", "helm.sh/hook-delete-policy":
			if !keepHook {
				continue
			}
		}
		filtered = append(filtered, key, val)
	}
	annotations.Content = filtered
}

// findMappingValueNode returns the value node for the given key in a
// mapping node, or nil if the key is not present or the parent is not a
// mapping. yaml.MappingNode.Content alternates [key, value, key, value,...].
func findMappingValueNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
