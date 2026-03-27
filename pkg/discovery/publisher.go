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

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// labelAgent marks a ConfigMap as a DNS-AID agent record.
	labelAgent = "dns-aid.io/agent"
	// labelName stores the agent name.
	labelName = "dns-aid.io/name"
	// labelProtocol stores the agent protocol.
	labelProtocol = "dns-aid.io/protocol"
	// indexConfigMapName is the name of the ConfigMap holding the agent index.
	indexConfigMapName = "dns-aid-index"
	// maxIndexUpdateRetries is the maximum number of optimistic concurrency retries
	// when updating the index ConfigMap.
	maxIndexUpdateRetries = 3
)

// k8sNameRegex validates Kubernetes resource names per RFC 1123 DNS subdomain.
var k8sNameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`)

// Publisher manages DNS-AID agent registration via Kubernetes resources.
// CoreDNS with the rdatapolicy plugin reads these resources to serve SVCB records.
type Publisher struct {
	clientset kubernetes.Interface
	namespace string
	labels    map[string]string
}

// NewPublisher creates a Publisher with functional options.
func NewPublisher(clientset kubernetes.Interface, opts ...PublisherOption) *Publisher {
	p := &Publisher{
		clientset: clientset,
		namespace: "default",
		labels:    make(map[string]string),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Publish creates or updates Kubernetes resources that represent an agent
// in the DNS-AID discovery system. CoreDNS rdatapolicy reads these to
// serve SVCB records at _{name}._{protocol}._agents.{namespace}.svc.cluster.local.
func (p *Publisher) Publish(ctx context.Context, reg AgentRegistration) error {
	ctx, cancel := context.WithTimeout(ctx, defaults.DiscoveryPublishTimeout)
	defer cancel()

	if err := validateRegistration(reg); err != nil {
		return err
	}

	// Create or update the agent ConfigMap
	if err := p.ensureAgentConfigMap(ctx, reg); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to publish agent ConfigMap", err)
	}

	// Update the index
	if err := p.updateIndex(ctx); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to update agent index", err)
	}

	slog.Info("published agent to DNS-AID",
		slog.String("name", reg.Name),
		slog.String("protocol", string(reg.Protocol)),
		slog.String("namespace", p.namespace),
	)

	return nil
}

// Deregister removes the Kubernetes resources for an agent, removing it
// from DNS-AID discovery.
func (p *Publisher) Deregister(ctx context.Context, name string, protocol Protocol) error {
	ctx, cancel := context.WithTimeout(ctx, defaults.DiscoveryDeregisterTimeout)
	defer cancel()

	cmName := configMapName(name, protocol)

	err := p.clientset.CoreV1().ConfigMaps(p.namespace).Delete(ctx, cmName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to delete agent ConfigMap %q", cmName), err)
	}

	if err := p.updateIndex(ctx); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to update agent index after deregister", err)
	}

	slog.Info("deregistered agent from DNS-AID",
		slog.String("name", name),
		slog.String("protocol", string(protocol)),
		slog.String("namespace", p.namespace),
	)

	return nil
}

// validateRegistration checks that agent name and protocol are valid K8s resource name components.
func validateRegistration(reg AgentRegistration) error {
	if !k8sNameRegex.MatchString(reg.Name) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("agent name %q is not a valid DNS label (must match [a-z0-9]([a-z0-9-]*[a-z0-9])?)", reg.Name))
	}
	if !k8sNameRegex.MatchString(string(reg.Protocol)) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("protocol %q is not a valid DNS label", reg.Protocol))
	}
	// ConfigMap name = "dns-aid-{name}-{protocol}" must be <= 253 chars
	cm := configMapName(reg.Name, reg.Protocol)
	if len(cm) > 253 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("resulting ConfigMap name %q exceeds 253 character limit", cm))
	}
	return nil
}

// ensureAgentConfigMap creates or updates the ConfigMap for an agent record.
func (p *Publisher) ensureAgentConfigMap(ctx context.Context, reg AgentRegistration) error {
	cmName := configMapName(reg.Name, reg.Protocol)

	capsJSON, err := json.Marshal(reg.Capabilities)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal capabilities", err)
	}

	params := SvcParams{
		Policy: reg.PolicyURI,
		Realm:  reg.Realm,
		BAP:    reg.bapVersions(),
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal SvcParams", err)
	}

	labels := map[string]string{
		labelAgent:    "true",
		labelName:     reg.Name,
		labelProtocol: string(reg.Protocol),
	}
	for k, v := range p.labels {
		labels[k] = v
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: p.namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			"target":       fmt.Sprintf("%s.%s.svc.cluster.local.", reg.ServiceName, p.namespace),
			"port":         fmt.Sprintf("%d", reg.Port),
			"priority":     "1",
			"params":       string(paramsJSON),
			"capabilities": string(capsJSON),
			"version":      reg.Version,
		},
	}

	// Create-or-update semantics per CLAUDE.md rules
	_, err = p.clientset.CoreV1().ConfigMaps(p.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		_, err = p.clientset.CoreV1().ConfigMaps(p.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to update ConfigMap %q", cmName), err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to create ConfigMap %q", cmName), err)
	}

	return nil
}

// updateIndex scans all dns-aid agent ConfigMaps in the namespace and
// writes the index ConfigMap. Uses optimistic concurrency via resourceVersion
// to prevent lost updates when multiple publishers race.
func (p *Publisher) updateIndex(ctx context.Context) error {
	for range maxIndexUpdateRetries {
		err := p.tryUpdateIndex(ctx)
		if err == nil {
			return nil
		}
		if !k8serrors.IsConflict(err) {
			return err
		}
		slog.Debug("index update conflict, retrying")
	}
	return errors.New(errors.ErrCodeInternal,
		fmt.Sprintf("failed to update agent index after %d retries due to conflicts", maxIndexUpdateRetries))
}

// tryUpdateIndex performs a single attempt to update the index ConfigMap.
// Returns a K8s Conflict error if the resourceVersion has changed.
func (p *Publisher) tryUpdateIndex(ctx context.Context) error {
	cmList, err := p.clientset.CoreV1().ConfigMaps(p.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelAgent + "=true",
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list agent ConfigMaps", err)
	}

	// Build index as "name:protocol" entries, one per line
	var lines []string
	for _, cm := range cmList.Items {
		name := cm.Labels[labelName]
		proto := cm.Labels[labelProtocol]
		if name != "" && proto != "" {
			lines = append(lines, fmt.Sprintf("%s:%s", name, proto))
		}
	}

	agentsData := strings.Join(lines, "\n")

	// Try to get existing index to preserve resourceVersion for optimistic concurrency
	existing, err := p.clientset.CoreV1().ConfigMaps(p.namespace).Get(ctx, indexConfigMapName, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get agent index ConfigMap", err)
	}

	if k8serrors.IsNotFound(err) {
		// Create new index
		indexCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      indexConfigMapName,
				Namespace: p.namespace,
				Labels:    map[string]string{labelAgent: "index"},
			},
			Data: map[string]string{"agents": agentsData},
		}
		_, createErr := p.clientset.CoreV1().ConfigMaps(p.namespace).Create(ctx, indexCM, metav1.CreateOptions{})
		if k8serrors.IsAlreadyExists(createErr) {
			// Another writer created it between our Get and Create — treat as conflict to retry
			return k8serrors.NewConflict(corev1.Resource("configmaps"), indexConfigMapName, createErr)
		}
		if createErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to create agent index ConfigMap", createErr)
		}
		return nil
	}

	// Update existing — resourceVersion enforces optimistic concurrency
	existing.Data = map[string]string{"agents": agentsData}
	_, err = p.clientset.CoreV1().ConfigMaps(p.namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return err // May be Conflict — caller retries
	}
	return nil
}

// configMapName returns the ConfigMap name for an agent.
func configMapName(name string, protocol Protocol) string {
	return fmt.Sprintf("dns-aid-%s-%s", name, protocol)
}
