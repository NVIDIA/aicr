// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

// Package helmclient is a tiny leaf-package that builds a Helm Go SDK
// action.Configuration from a controller-runtime *rest.Config. Extracted
// from pkg/networkoperatorplugin/helm.go so the preflight sub-package can
// reuse the same wiring without an import cycle.
package helmclient

import (
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	memcacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	sigsyaml "sigs.k8s.io/yaml"
)

// StorageDriver selects how Helm persists release metadata in the cluster.
// "secrets" is Helm 3's default and matches what `helm list` reads — keeping
// l8k's deploy/validate path consistent with what an operator running plain
// helm commands would see.
const StorageDriver = "secrets"

// DefaultReleaseName is the helm release name l8k installs the
// network-operator chart under. Constants live here so both helm.go (Phase
// 0 install) and preflight checks key off the same identifier.
const DefaultReleaseName = "network-operator"

// DefaultNamespace is the fallback namespace l8k installs the chart into
// when neither l8k-config.yaml nor the embedded catalog supplies one.
const DefaultNamespace = "nvidia-network-operator"

// NewActionConfig builds an action.Configuration backed by the supplied
// *rest.Config. Used by Phase 0 install/upgrade and by the preflight helm
// checks; centralising it here keeps the storage driver and kube client
// wiring identical across both paths.
func NewActionConfig(restConfig *rest.Config, namespace, helmDriver string) (*action.Configuration, error) {
	if restConfig == nil {
		return nil, fmt.Errorf("nil rest config")
	}
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if helmDriver == "" {
		helmDriver = StorageDriver
	}
	getter := &restClientGetter{restConfig: restConfig, namespace: namespace}
	cfg := new(action.Configuration)
	if err := cfg.Init(getter, namespace, helmDriver, debugNoop); err != nil {
		return nil, err
	}
	return cfg, nil
}

// UnmarshalValues parses helm values YAML into a generic map. Returns an
// empty map (not nil) when the input is empty or YAML null. Shared between
// the Phase 0 conflict check and the preflight values check so the two
// flows can't disagree about how to interpret malformed input.
func UnmarshalValues(b []byte) (map[string]interface{}, error) {
	if len(b) == 0 {
		return map[string]interface{}{}, nil
	}
	out := map[string]interface{}{}
	if err := sigsyaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]interface{}{}
	}
	return out, nil
}

// debugNoop discards helm-internal debug logging. l8k's structured logger
// already covers the deploy-level signal users care about.
func debugNoop(format string, v ...interface{}) {
	_ = format
	_ = v
}

// restClientGetter adapts a *rest.Config to genericclioptions.RESTClientGetter
// so that action.Configuration.Init can construct a kube client without
// going through clientcmd / kubeconfig file resolution.
type restClientGetter struct {
	restConfig *rest.Config
	namespace  string
}

func (r *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(r.restConfig), nil
}

func (r *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	cfg, err := r.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cfg.Burst = 100
	d, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return memcacheddiscovery.NewMemCacheClient(d), nil
}

func (r *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := r.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(dc)
	return restmapper.NewShortcutExpander(mapper, dc, func(s string) {}), nil
}

func (r *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	overrides := &clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: r.namespace}}
	return clientcmd.NewDefaultClientConfig(*clientcmdapi.NewConfig(), overrides)
}
