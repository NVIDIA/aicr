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

package k8s

import (
	"context"
	stderrors "errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func legacyPluginDS(labels map[string]string, desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      okeLegacyPluginDSName,
			Namespace: okeLegacyPluginNamespace,
			Labels:    labels,
		},
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: desired},
	}
}

// TestCollectOKELegacyPlugin_StateMatrix pins the collapse rules: only
// Oracle's addon-manager-labeled DaemonSet with a non-zero node target reads
// "active"; every benign shape reads "none" with the uncollapsed detail
// preserved; an API failure reads "unknown" so constraints fail closed.
func TestCollectOKELegacyPlugin_StateMatrix(t *testing.T) {
	t.Parallel()

	reconcile := map[string]string{okeLegacyAddonManagerLabel: okeLegacyAddonManagerMode}

	tests := []struct {
		name          string
		objects       []runtime.Object
		listError     error
		wantPlugin    string
		wantDaemonSet string
	}{
		{
			name:          "absent daemonset reads none/absent",
			wantPlugin:    okeLegacyPluginNone,
			wantDaemonSet: okeLegacyDSAbsent,
		},
		{
			name:          "labeled daemonset targeting nodes reads active",
			objects:       []runtime.Object{legacyPluginDS(reconcile, 3)},
			wantPlugin:    okeLegacyPluginActive,
			wantDaemonSet: okeLegacyDSActive,
		},
		{
			name:          "labeled daemonset with zero desired reads none/disabled",
			objects:       []runtime.Object{legacyPluginDS(reconcile, 0)},
			wantPlugin:    okeLegacyPluginNone,
			wantDaemonSet: okeLegacyDSDisabled,
		},
		{
			name:          "same-named daemonset without the addon-manager label reads none/unlabeled",
			objects:       []runtime.Object{legacyPluginDS(map[string]string{"app": "custom"}, 3)},
			wantPlugin:    okeLegacyPluginNone,
			wantDaemonSet: okeLegacyDSUnlabeled,
		},
		{
			name:          "api error reads unknown (fail closed)",
			listError:     stderrors.New("apiserver unavailable"),
			wantPlugin:    okeLegacyPluginUnknown,
			wantDaemonSet: okeLegacyDSUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clientset := fakeclient.NewClientset(tt.objects...)
			if tt.listError != nil {
				clientset.PrependReactor("get", "daemonsets",
					func(clienttesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.listError
					})
			}
			k := &Collector{ClientSet: clientset}

			subtype := k.collectOKELegacyPlugin(context.Background())

			if subtype.Name != SubtypeOKELegacyPlugin {
				t.Fatalf("subtype name = %q, want %q", subtype.Name, SubtypeOKELegacyPlugin)
			}
			if got := subtype.Data[okeLegacyKeyPlugin].Any(); got != tt.wantPlugin {
				t.Errorf("%s = %v, want %q", okeLegacyKeyPlugin, got, tt.wantPlugin)
			}
			if got := subtype.Data[okeLegacyKeyDaemonSet].Any(); got != tt.wantDaemonSet {
				t.Errorf("%s = %v, want %q", okeLegacyKeyDaemonSet, got, tt.wantDaemonSet)
			}
		})
	}
}

// TestUnknownOKELegacyPluginSubtype pins the no-inspection value used by
// emptyK8sMeasurement: a clientless snapshot must read unknown, never none.
func TestUnknownOKELegacyPluginSubtype(t *testing.T) {
	t.Parallel()

	subtype := unknownOKELegacyPluginSubtype()
	if got := subtype.Data[okeLegacyKeyPlugin].Any(); got != okeLegacyPluginUnknown {
		t.Errorf("%s = %v, want %q", okeLegacyKeyPlugin, got, okeLegacyPluginUnknown)
	}
	if got := subtype.Data[okeLegacyKeyDaemonSet].Any(); got != okeLegacyDSUnknown {
		t.Errorf("%s = %v, want %q", okeLegacyKeyDaemonSet, got, okeLegacyDSUnknown)
	}
}
