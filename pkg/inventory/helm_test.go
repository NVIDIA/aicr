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

package inventory

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// undecodablePayload fails decodeRelease at its first step. Superseded
// revisions carry it so a test fails loudly if they are ever decoded.
const undecodablePayload = "!!!not base64!!!"

// releasePayload encodes a stored record the way Helm writes one.
func releasePayload(t *testing.T, release, namespace, status, chart, chartVersion string, revision int) string {
	t.Helper()
	return encodeReleaseFixture(t, fmt.Sprintf(
		`{"name":%q,"info":{"status":%q},"chart":{"metadata":{"name":%q,"version":%q,"appVersion":"25.3.3"}},`+
			`"version":%d,"namespace":%q}`,
		release, status, chart, chartVersion, revision, namespace))
}

// helmLabels mirrors the label set Helm's storage drivers write. createdAt and
// modifiedAt are omitted: nothing here reads them.
func helmLabels(release string, revision int, status string) map[string]string {
	return map[string]string{
		"owner":   "helm",
		"name":    release,
		"status":  status,
		"version": strconv.Itoa(revision),
	}
}

// storageKey is the Secret/ConfigMap name Helm derives from the release and
// its revision. Nothing parses it; it is here so fixtures collide exactly
// where real records would.
func storageKey(release string, revision int) string {
	return fmt.Sprintf("sh.helm.release.v1.%s.v%d", release, revision)
}

func helmSecret(namespace, release string, revision int, status, payload string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      storageKey(release, revision),
			Namespace: namespace,
			Labels:    helmLabels(release, revision, status),
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte(payload)},
	}
}

func helmConfigMap(namespace, release string, revision int, status, payload string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      storageKey(release, revision),
			Namespace: namespace,
			Labels:    helmLabels(release, revision, status),
		},
		Data: map[string]string{"release": payload},
	}
}

func TestHelmReleases(t *testing.T) {
	gpuOperator := releasePayload(t, "gpu-operator", "gpu-operator", "deployed", "gpu-operator", "v25.3.3", 1)
	networkOperator := releasePayload(t, "network-operator", "nvidia-network-operator", "deployed",
		"network-operator", "v25.1.0", 1)

	wantGPUOperator := installedRelease{
		Source:       sourceHelm,
		Name:         "gpu-operator",
		Namespace:    "gpu-operator",
		Revision:     1,
		Status:       "deployed",
		ChartName:    "gpu-operator",
		ChartVersion: "v25.3.3",
		AppVersion:   "25.3.3",
	}
	wantNetworkOperator := installedRelease{
		Source:       sourceHelm,
		Name:         "network-operator",
		Namespace:    "nvidia-network-operator",
		Revision:     1,
		Status:       "deployed",
		ChartName:    "network-operator",
		ChartVersion: "v25.1.0",
		AppVersion:   "25.3.3",
	}

	tests := []struct {
		name string
		// objects seed the fake cluster in the order given, so a case can
		// prove ordering is imposed by the implementation and not inherited.
		objects []runtime.Object
		// failResource and failErr make List on that resource fail.
		failResource string
		failErr      error
		// within defaults to defaultTestScope when nil.
		within        scope
		cancelContext bool
		want          []installedRelease
		// wantRecords is every object the walk examined, in scope or not. It
		// is asserted on every case rather than where it looked interesting,
		// because it is the denominator the other two counts are read against
		// and a driver that stopped counting would otherwise pass.
		wantRecords      int
		wantUnattributed int
		wantUnreadable   int
		wantErr          bool
		wantErrCode      errors.ErrorCode
		wantErrContains  []string
		wantErrContext   map[string]any
	}{
		{
			name:        "single release",
			objects:     []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			want:        []installedRelease{wantGPUOperator},
			wantRecords: 1,
		},
		{
			name: "newest revision wins without decoding superseded revisions",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator", 2, "superseded", undecodablePayload),
				helmSecret("gpu-operator", "gpu-operator", 3, "deployed",
					releasePayload(t, "gpu-operator", "gpu-operator", "deployed", "gpu-operator", "v25.10.0", 3)),
				helmSecret("gpu-operator", "gpu-operator", 1, "superseded", undecodablePayload),
			},
			want: []installedRelease{{
				Source:       sourceHelm,
				Name:         "gpu-operator",
				Namespace:    "gpu-operator",
				Revision:     3,
				Status:       "deployed",
				ChartName:    "gpu-operator",
				ChartVersion: "v25.10.0",
				AppVersion:   "25.3.3",
			}},
			wantRecords: 3,
		},
		{
			name: "revisions past nine sort numerically, not lexically",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator", 9, "superseded", undecodablePayload),
				helmSecret("gpu-operator", "gpu-operator", 10, "deployed",
					releasePayload(t, "gpu-operator", "gpu-operator", "deployed", "gpu-operator", "v25.10.0", 10)),
			},
			want: []installedRelease{{
				Source:       sourceHelm,
				Name:         "gpu-operator",
				Namespace:    "gpu-operator",
				Revision:     10,
				Status:       "deployed",
				ChartName:    "gpu-operator",
				ChartVersion: "v25.10.0",
				AppVersion:   "25.3.3",
			}},
			wantRecords: 2,
		},
		{
			name: "releases in different namespaces, returned sorted by name",
			objects: []runtime.Object{
				helmSecret("nvidia-network-operator", "network-operator", 1, "deployed", networkOperator),
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want:        []installedRelease{wantGPUOperator, wantNetworkOperator},
			wantRecords: 2,
		},
		{
			name: "one release name installed in two namespaces stays two releases",
			objects: []runtime.Object{
				helmSecret("tenant-b", "gpu-operator", 4, "deployed",
					releasePayload(t, "gpu-operator", "tenant-b", "deployed", "gpu-operator", "v25.10.0", 4)),
				helmSecret("tenant-a", "gpu-operator", 1, "deployed",
					releasePayload(t, "gpu-operator", "tenant-a", "deployed", "gpu-operator", "v25.3.3", 1)),
			},
			want: []installedRelease{
				{
					Source: sourceHelm, Name: "gpu-operator", Namespace: "tenant-a", Revision: 1, Status: "deployed",
					ChartName: "gpu-operator", ChartVersion: "v25.3.3", AppVersion: "25.3.3",
				},
				{
					Source: sourceHelm, Name: "gpu-operator", Namespace: "tenant-b", Revision: 4, Status: "deployed",
					ChartName: "gpu-operator", ChartVersion: "v25.10.0", AppVersion: "25.3.3",
				},
			},
			wantRecords: 2,
		},
		{
			name: "failed release is reported with its status",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator", 1, "superseded", undecodablePayload),
				helmSecret("gpu-operator", "gpu-operator", 2, "failed",
					releasePayload(t, "gpu-operator", "gpu-operator", "failed", "gpu-operator", "v25.10.0", 2)),
			},
			want: []installedRelease{{
				Source:       sourceHelm,
				Name:         "gpu-operator",
				Namespace:    "gpu-operator",
				Revision:     2,
				Status:       "failed",
				ChartName:    "gpu-operator",
				ChartVersion: "v25.10.0",
				AppVersion:   "25.3.3",
			}},
			wantRecords: 2,
		},
		{
			name: "secrets Helm does not own are ignored",
			objects: []runtime.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "app-credentials", Namespace: "default"},
					Type:       corev1.SecretTypeOpaque,
					Data:       map[string][]byte{"release": []byte(undecodablePayload)},
				},
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "default"},
					Data:       map[string]string{"release": undecodablePayload},
				},
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want:        []installedRelease{wantGPUOperator},
			wantRecords: 1,
		},
		{
			name: "storage format this build has not been taught is refused",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)
					s.Type = "helm.sh/release.v2"
					return s
				}(),
			},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"helm.sh/release.v2", "helm.sh/release.v1", "gpu-operator"},
		},
		{
			name: "chart annotations reach the result",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", encodeReleaseFixture(t,
					`{"info":{"status":"deployed"},"chart":{"metadata":{"name":"gpu-operator",`+
						`"version":"v25.3.3","appVersion":"25.3.3",`+
						`"annotations":{"aicr.run/component-version":"1.4.2","other":"kept"}}}}`)),
			},
			want: []installedRelease{{
				Source:       sourceHelm,
				Name:         "gpu-operator",
				Namespace:    "gpu-operator",
				Revision:     1,
				Status:       "deployed",
				ChartName:    "gpu-operator",
				ChartVersion: "v25.3.3",
				AppVersion:   "25.3.3",
				Annotations: map[string]string{
					"aicr.run/component-version": "1.4.2",
					"other":                      "kept",
				},
			}},
			wantRecords: 1,
		},
		{
			// The storage labels are the one source of a release's identity.
			// A payload that disagrees is not consulted, so a decoder that
			// started reading identity out of it would fail here.
			name: "labels beat a payload that disagrees about identity",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", encodeReleaseFixture(t,
					`{"name":"wrong","namespace":"wrong","version":99,"info":{"status":"deployed"},`+
						`"chart":{"metadata":{"name":"gpu-operator","version":"v25.3.3","appVersion":"25.3.3"}}}`)),
			},
			want:        []installedRelease{wantGPUOperator},
			wantRecords: 1,
		},
		{
			name:        "configmap storage driver",
			objects:     []runtime.Object{helmConfigMap("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			want:        []installedRelease{wantGPUOperator},
			wantRecords: 1,
		},
		{
			name: "both storage drivers in one cluster, each picking its newest revision",
			objects: []runtime.Object{
				helmConfigMap("nvidia-network-operator", "network-operator", 2, "deployed",
					releasePayload(t, "network-operator", "nvidia-network-operator", "deployed",
						"network-operator", "v25.7.0", 2)),
				helmConfigMap("nvidia-network-operator", "network-operator", 1, "superseded", undecodablePayload),
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want: []installedRelease{wantGPUOperator, {
				Source:       sourceHelm,
				Name:         "network-operator",
				Namespace:    "nvidia-network-operator",
				Revision:     2,
				Status:       "deployed",
				ChartName:    "network-operator",
				ChartVersion: "v25.7.0",
				AppVersion:   "25.3.3",
			}},
			wantRecords: 3,
		},
		{
			// keepNewest keeps the incumbent on a revision tie, and the secret
			// driver is read first, so its record wins. Both encode the same
			// release so either answer would be defensible; this pins the one
			// the comment claims. Without a fixture where one release appears
			// under both drivers at one revision, the rule is unasserted.
			name: "a revision tie between the two drivers keeps the secret record",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator", 7, "deployed",
					releasePayload(t, "gpu-operator", "gpu-operator", "deployed",
						"gpu-operator", "v-from-secret", 7)),
				helmConfigMap("gpu-operator", "gpu-operator", 7, "deployed",
					releasePayload(t, "gpu-operator", "gpu-operator", "deployed",
						"gpu-operator", "v-from-configmap", 7)),
			},
			want: []installedRelease{{
				Source: sourceHelm, Name: "gpu-operator", Namespace: "gpu-operator", Revision: 7,
				Status: "deployed", ChartName: "gpu-operator", ChartVersion: "v-from-secret",
				AppVersion: "25.3.3",
			}},
			wantRecords: 2,
		},
		{
			name:    "no releases",
			objects: nil,
			want:    nil,
		},
		{
			name: "version label that is not an integer",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)
					s.Labels["version"] = "abc"
					return s
				}(),
			},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"gpu-operator", `"abc"`},
			wantErrContext: map[string]any{
				"object":    storageKey("gpu-operator", 1),
				"namespace": "gpu-operator",
				"release":   "gpu-operator",
			},
		},
		{
			// A record naming no release belongs to no component, so it cannot
			// reach the comparison and must not fail it. It is counted rather
			// than dropped: an exclusion is reported, never silent.
			name: "record without a name label is counted, not fatal",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)
					delete(s.Labels, "name")
					return s
				}(),
				helmSecret("nvidia-network-operator", "network-operator", 1, "deployed", networkOperator),
			},
			want:             []installedRelease{wantNetworkOperator},
			wantUnattributed: 1,
			wantRecords:      2,
		},
		{
			// The payload is never touched, so a record that would fail every
			// decode step costs nothing when it names no release.
			name: "unattributable record is not decoded",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("stranger", "stranger", 1, "deployed", undecodablePayload)
					delete(s.Labels, "name")
					s.Type = "helm.sh/release.v2"
					return s
				}(),
			},
			want:             nil,
			wantUnattributed: 1,
			wantRecords:      1,
		},
		{
			// The cluster is full of releases this project knows nothing
			// about. One of them being unreadable is not a reason to fail a
			// run that does not report on it.
			name: "foreign secret in an unrecognized storage format is skipped",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("teamspace", "their-app", 1, "deployed", gpuOperator)
					s.Type = "helm.sh/release.v2"
					return s
				}(),
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want:        []installedRelease{wantGPUOperator},
			wantRecords: 2,
		},
		{
			// Proves the skip precedes decoding, not merely reporting: an
			// out-of-scope payload is never inflated.
			name: "foreign record with an undecodable payload is skipped",
			objects: []runtime.Object{
				helmSecret("teamspace", "their-app", 4, "deployed", undecodablePayload),
				helmConfigMap("teamspace", "their-other-app", 2, "deployed", undecodablePayload),
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want:        []installedRelease{wantGPUOperator},
			wantRecords: 3,
		},
		{
			// Both drivers, because each has its own loop and the ordering has
			// to hold in both: the label is parsed only for records the read
			// answers for.
			name: "foreign record with a malformed revision label is skipped",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("teamspace", "their-app", 1, "deployed", gpuOperator)
					s.Labels["version"] = "abc"
					return s
				}(),
				func() runtime.Object {
					c := helmConfigMap("teamspace", "their-other-app", 1, "deployed", gpuOperator)
					c.Labels["version"] = "not-a-number"
					return c
				}(),
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want:        []installedRelease{wantGPUOperator},
			wantRecords: 3,
		},
		{
			name: "foreign record without a destination the scope names is skipped",
			objects: []runtime.Object{
				helmSecret("teamspace", "their-app", 1, "deployed", gpuOperator),
			},
			want:        nil,
			wantRecords: 1,
		},
		{
			// The bundle writer names an injected folder "<component>-<phase>",
			// and that is the release name the cluster holds.
			name: "injected folder releases are in their component's scope",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator-readiness", 1, "deployed",
					releasePayload(t, "gpu-operator-readiness", "gpu-operator", "deployed", "local-helm", "0.1.0", 1)),
				helmSecret("gpu-operator", "gpu-operator-pre", 1, "deployed",
					releasePayload(t, "gpu-operator-pre", "gpu-operator", "deployed", "local-helm", "0.1.0", 1)),
				helmSecret("gpu-operator", "gpu-operator-post", 1, "deployed",
					releasePayload(t, "gpu-operator-post", "gpu-operator", "deployed", "local-helm", "0.1.0", 1)),
			},
			want: []installedRelease{
				{
					Source: sourceHelm, Name: "gpu-operator-post", Namespace: "gpu-operator", Revision: 1,
					Status: "deployed", ChartName: "local-helm", ChartVersion: "0.1.0", AppVersion: "25.3.3",
				},
				{
					Source: sourceHelm, Name: "gpu-operator-pre", Namespace: "gpu-operator", Revision: 1,
					Status: "deployed", ChartName: "local-helm", ChartVersion: "0.1.0", AppVersion: "25.3.3",
				},
				{
					Source: sourceHelm, Name: "gpu-operator-readiness", Namespace: "gpu-operator", Revision: 1,
					Status: "deployed", ChartName: "local-helm", ChartVersion: "0.1.0", AppVersion: "25.3.3",
				},
			},
			wantRecords: 3,
		},
		{
			// The stale-baseline trap. Dropping the unreadable newest revision
			// alone would leave the readable superseded one to win keepNewest,
			// and the release would be reported at the version it was upgraded
			// away from: a confident wrong answer where a gap was intended.
			// Under Flux every release name is "<targetNamespace>-<name>", so
			// nothing is confident and every release sits in this tier.
			name: "a tolerated newest revision does not promote a superseded one",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator-gpu-operator", 1, "superseded",
					releasePayload(t, "gpu-operator-gpu-operator", "gpu-operator", "superseded",
						"gpu-operator", "v25.3.3", 1)),
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator-gpu-operator", 2, "deployed",
						releasePayload(t, "gpu-operator-gpu-operator", "gpu-operator", "deployed",
							"gpu-operator", "v25.10.0", 2))
					s.Type = "helm.sh/release.v2"
					return s
				}(),
			},
			want:           nil,
			wantUnreadable: 1,
			wantRecords:    2,
		},
		{
			// Order must not matter: the superseded revision may already be in
			// hand when the unreadable one arrives, or the reverse.
			name: "a release poisoned before its readable revision arrives stays withheld",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator-gpu-operator", 2, "deployed",
						releasePayload(t, "gpu-operator-gpu-operator", "gpu-operator", "deployed",
							"gpu-operator", "v25.10.0", 2))
					s.Labels["version"] = "not-a-number"
					return s
				}(),
				helmSecret("gpu-operator", "gpu-operator-gpu-operator", 1, "superseded",
					releasePayload(t, "gpu-operator-gpu-operator", "gpu-operator", "superseded",
						"gpu-operator", "v25.3.3", 1)),
			},
			want:           nil,
			wantUnreadable: 1,
			wantRecords:    2,
		},
		{
			// A poisoned release is one release however many of its revisions
			// could not be read, and poisoning one must not withhold another.
			name: "unreadable revisions of one release count once and do not spread",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator-gpu-operator", 3, "deployed", gpuOperator)
					s.Type = "helm.sh/release.v2"
					return s
				}(),
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator-gpu-operator", 2, "superseded", gpuOperator)
					s.Labels["version"] = "nope"
					return s
				}(),
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want:           []installedRelease{wantGPUOperator},
			wantUnreadable: 1,
			wantRecords:    3,
		},
		{
			// The same release name in two namespaces is two releases, so
			// poisoning one leaves the other readable.
			name: "poisoning is per release, not per name",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("tenant-a", "gpu-operator-gpu-operator", 1, "deployed", gpuOperator)
					s.Type = "helm.sh/release.v2"
					return s
				}(),
				helmSecret("tenant-b", "gpu-operator-gpu-operator", 1, "deployed",
					releasePayload(t, "gpu-operator-gpu-operator", "tenant-b", "deployed",
						"gpu-operator", "v25.10.0", 1)),
			},
			want: []installedRelease{{
				Source: sourceHelm, Name: "gpu-operator-gpu-operator", Namespace: "tenant-b", Revision: 1,
				Status: "deployed", ChartName: "gpu-operator", ChartVersion: "v25.10.0", AppVersion: "25.3.3",
			}},
			wantUnreadable: 1,
			wantRecords:    2,
		},
		{
			// Flux's "<targetNamespace>-<name>" is a possible match, not a
			// confident one, so an unreadable record in that shape is counted
			// rather than allowed to fail a run it may not belong to.
			name: "malformed possible record is skipped and counted",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator-gpu-operator", 1, "deployed", gpuOperator)
					s.Type = "helm.sh/release.v2"
					return s
				}(),
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want:           []installedRelease{wantGPUOperator},
			wantUnreadable: 1,
			wantRecords:    2,
		},
		{
			name: "possible record with a malformed revision label is skipped and counted",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("gpu-operator", "tenant-a-gpu-operator", 1, "deployed", gpuOperator)
					s.Labels["version"] = "abc"
					return s
				}(),
				func() runtime.Object {
					c := helmConfigMap("gpu-operator", "tenantgpu-operator", 1, "deployed", gpuOperator)
					c.Labels["version"] = "nope"
					return c
				}(),
			},
			want:           nil,
			wantUnreadable: 2,
			wantRecords:    2,
		},
		{
			// Leniency has to reach decode time, not just the label checks.
			name: "possible record that cannot be decoded is skipped and counted",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator-gpu-operator", 1, "deployed", undecodablePayload),
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
			},
			want:           []installedRelease{wantGPUOperator},
			wantUnreadable: 1,
			wantRecords:    2,
		},
		{
			name: "possible record with an empty payload is skipped and counted",
			objects: []runtime.Object{
				helmConfigMap("gpu-operator", "tenant-a-gpu-operator", 1, "deployed", ""),
			},
			want:           nil,
			wantUnreadable: 1,
			wantRecords:    1,
		},
		{
			// The confident tier is unchanged: an injected folder is a name
			// this project wrote, so a broken one still fails the run.
			name: "malformed injected-folder record still fails the run",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator-readiness", 1, "deployed", undecodablePayload),
			},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"gpu-operator-readiness"},
		},
		{
			// An empty inventory and an unanswerable request read the same in
			// the result, and the reading is "every component is new".
			name:            "an empty scope is refused, not answered",
			objects:         []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			within:          newScope(),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInvalidRequest,
			wantErrContains: []string{"no components"},
		},
		{
			name: "record without the release data key",
			objects: []runtime.Object{
				func() runtime.Object {
					s := helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)
					s.Data = nil
					return s
				}(),
			},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"gpu-operator", `"release"`},
			wantErrContext: map[string]any{
				"object":    storageKey("gpu-operator", 1),
				"namespace": "gpu-operator",
				"release":   "gpu-operator",
			},
		},
		{
			name: "record with an empty release data key",
			objects: []runtime.Object{
				helmConfigMap("gpu-operator", "gpu-operator", 1, "deployed", ""),
			},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"gpu-operator", `"release"`},
		},
		{
			name:            "listing secrets is forbidden",
			objects:         []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			failResource:    "secrets",
			failErr:         forbidden("secrets"),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeUnauthorized,
			wantErrContains: []string{"list", "secrets"},
			wantErrContext:  map[string]any{"resource": "secrets"},
		},
		{
			name:            "listing configmaps is forbidden",
			objects:         []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			failResource:    "configmaps",
			failErr:         forbidden("configmaps"),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeUnauthorized,
			wantErrContains: []string{"list", "configmaps"},
		},
		{
			name: "newest revision that cannot be decoded fails the run",
			objects: []runtime.Object{
				helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator),
				helmSecret("gpu-operator", "gpu-operator", 2, "deployed", undecodablePayload),
			},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"gpu-operator"},
		},
		{
			name:            "listing secrets runs out of deadline",
			objects:         []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			failResource:    "secrets",
			failErr:         fmt.Errorf("client rate limiter Wait: %w", context.DeadlineExceeded),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeTimeout,
			wantErrContains: []string{"secrets"},
		},
		{
			name:         "listing configmaps is canceled",
			objects:      []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			failResource: "configmaps",
			failErr:      fmt.Errorf("get %q: %w", "/api/v1/configmaps", context.Canceled),
			wantErr:      true,
			// Not ErrCodeTimeout: errors.IsTransient reports false for a
			// cancellation and true for a timeout, so a Ctrl-C coded as the
			// latter re-enters a caller's retry loop and exits with the wrong
			// status. The deadline case above is the one that stays a timeout.
			wantErrCode:     errors.ErrCodeCanceled,
			wantErrContains: []string{"configmaps", "canceled"},
		},
		{
			name:            "the paged list outlives the apiserver's window",
			objects:         []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			failResource:    "secrets",
			failErr:         apierrors.NewResourceExpired("continue token expired"),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeUnavailable,
			wantErrContains: []string{"secrets", "re-run"},
			wantErrContext:  map[string]any{"resource": "secrets"},
		},
		{
			name:            "listing secrets fails for a reason that is not RBAC",
			objects:         []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			failResource:    "secrets",
			failErr:         apierrors.NewServiceUnavailable("the server is currently unable to handle the request"),
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"secrets"},
		},
		{
			name: "configmap record labels are validated like secret record labels",
			objects: []runtime.Object{
				func() runtime.Object {
					c := helmConfigMap("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)
					c.Labels["version"] = "abc"
					return c
				}(),
			},
			wantErr:         true,
			wantErrCode:     errors.ErrCodeInternal,
			wantErrContains: []string{"gpu-operator", "abc"},
		},
		{
			name:            "canceled context",
			objects:         []runtime.Object{helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
			cancelContext:   true,
			wantErr:         true,
			wantErrCode:     errors.ErrCodeCanceled,
			wantErrContains: []string{"Helm release", "canceled"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(tt.objects...)
			if tt.failResource != "" {
				client.PrependReactor("list", tt.failResource,
					func(_ k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.failErr
					})
			}

			ctx := context.Background()
			if tt.cancelContext {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}

			within := tt.within
			if within == nil {
				within = defaultTestScope()
			}

			read, err := helmReleases(ctx, client, within)
			if (err != nil) != tt.wantErr {
				t.Fatalf("helmReleases() error = %v, wantErr %v", err, tt.wantErr)
			}
			got := read.Releases
			if tt.wantErr {
				if !reflect.DeepEqual(read, inventoryRead{}) {
					t.Errorf("helmReleases() returned %+v alongside an error", read)
				}
				assertError(t, err, tt.wantErrCode, tt.wantErrContext)
				for _, want := range tt.wantErrContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}

				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("helmReleases() = %+v, want %+v", got, tt.want)
			}
			if read.Unattributed != tt.wantUnattributed {
				t.Errorf("helmReleases() counted %d unattributable records, want %d",
					read.Unattributed, tt.wantUnattributed)
			}
			if read.Unreadable != tt.wantUnreadable {
				t.Errorf("helmReleases() counted %d unreadable records, want %d",
					read.Unreadable, tt.wantUnreadable)
			}
			if read.Records != tt.wantRecords {
				t.Errorf("helmReleases() examined %d records, want %d", read.Records, tt.wantRecords)
			}
		})
	}
}

// defaultTestScope names the components the fixtures in this file install.
// Anything outside it stands in for the rest of a real cluster.
func defaultTestScope() scope {
	return newScope("gpu-operator", "network-operator")
}

// assertError pins a structured error's code, and its context when the caller
// supplies one. A nil wantContext skips the context check; a non-nil one is
// exact, so an error that publishes an extra key, or maps a key to the wrong
// value, fails here.
func assertError(t *testing.T, err error, wantCode errors.ErrorCode, wantContext map[string]any) {
	t.Helper()
	var structured *errors.StructuredError
	if !stderrors.As(err, &structured) {
		t.Fatalf("error %v is not a *errors.StructuredError", err)
	}
	if structured.Code != wantCode {
		t.Errorf("error code = %q, want %q", structured.Code, wantCode)
	}
	if wantContext != nil && !maps.Equal(structured.Context, wantContext) {
		t.Errorf("error context = %#v, want exactly %#v", structured.Context, wantContext)
	}
}

// forbidden is the RBAC denial the apiserver returns for a cluster-scoped List
// the caller may not perform.
func forbidden(resource string) error {
	return apierrors.NewForbidden(corev1.Resource(resource), "",
		stderrors.New("User cannot list resource at the cluster scope"))
}

// TestHelmReleasesCancellation covers the stages a canceled context cannot
// reach through helmReleases, which stops at the first one.
func TestHelmReleasesCancellation(t *testing.T) {
	payload := releasePayload(t, "gpu-operator", "gpu-operator", "deployed", "gpu-operator", "v25.3.3", 1)

	tests := []struct {
		name string
		run  func(context.Context) error
	}{
		{
			name: "configmap records",
			run: func(ctx context.Context) error {
				client := fake.NewSimpleClientset(
					helmConfigMap("gpu-operator", "gpu-operator", 1, "deployed", payload))
				return collectConfigMapRecords(ctx, client,
					&helmWalk{within: defaultTestScope(), newest: map[releaseID]storedRecord{}})
			},
		},
		{
			name: "decoding the selected revisions",
			run: func(ctx context.Context) error {
				_, err := decodeNewest(ctx, &helmWalk{
					within: defaultTestScope(),
					newest: map[releaseID]storedRecord{
						{namespace: "gpu-operator", name: "gpu-operator"}: {
							release:     "gpu-operator",
							namespace:   "gpu-operator",
							revision:    1,
							object:      storageKey("gpu-operator", 1),
							confidence:  confident,
							payloadText: payload,
						},
					},
				})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := tt.run(ctx)
			if err == nil {
				t.Fatal("expected a canceled context to fail")
			}
			assertError(t, err, errors.ErrCodeCanceled, nil)
			if errors.IsTransient(err) {
				// The whole point of the code: an abort the operator asked for
				// must not be reported as a retryable infrastructure fault.
				t.Errorf("error %v is transient, want a canceled abort to be terminal", err)
			}
		})
	}
}

// TestHelmReleasesFollowsListPages proves each collector keeps listing while
// the server hands back a continue token, that the fold spans pages, and that
// the selector survives every request.
//
// What it cannot prove: the fake clientset drops Limit and Continue from the
// ListOptions before a reactor sees them (k8stesting.ListActionImpl carries
// only the label and field restrictions), so no test here can observe the
// token this code sends back. TestHelmReleasesEchoesPagingOptions covers that
// over the wire.
func TestHelmReleasesFollowsListPages(t *testing.T) {
	gpuOperator := releasePayload(t, "gpu-operator", "gpu-operator", "deployed", "gpu-operator", "v25.3.3", 1)
	networkOperator := releasePayload(t, "network-operator", "nvidia-network-operator", "deployed",
		"network-operator", "v25.1.0", 1)
	newer := releasePayload(t, "gpu-operator", "gpu-operator", "deployed", "gpu-operator", "v25.10.0", 5)

	wantGPUOperator := installedRelease{
		Source: sourceHelm,
		Name:   "gpu-operator", Namespace: "gpu-operator", Revision: 1, Status: "deployed",
		ChartName: "gpu-operator", ChartVersion: "v25.3.3", AppVersion: "25.3.3",
	}
	wantNetworkOperator := installedRelease{
		Source: sourceHelm,
		Name:   "network-operator", Namespace: "nvidia-network-operator", Revision: 1, Status: "deployed",
		ChartName: "network-operator", ChartVersion: "v25.1.0", AppVersion: "25.3.3",
	}
	wantNewer := installedRelease{
		Source: sourceHelm,
		Name:   "gpu-operator", Namespace: "gpu-operator", Revision: 5, Status: "deployed",
		ChartName: "gpu-operator", ChartVersion: "v25.10.0", AppVersion: "25.3.3",
	}

	secretPage := func(secrets ...*corev1.Secret) runtime.Object {
		page := &corev1.SecretList{}
		for _, secret := range secrets {
			page.Items = append(page.Items, *secret)
		}

		return page
	}
	configMapPage := func(configMaps ...*corev1.ConfigMap) runtime.Object {
		page := &corev1.ConfigMapList{}
		for _, configMap := range configMaps {
			page.Items = append(page.Items, *configMap)
		}

		return page
	}

	tests := []struct {
		name     string
		resource string
		pages    []runtime.Object
		want     []installedRelease
	}{
		{
			name:     "secret driver",
			resource: "secrets",
			pages: []runtime.Object{
				secretPage(helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)),
				secretPage(helmSecret("nvidia-network-operator", "network-operator", 1, "deployed", networkOperator)),
			},
			want: []installedRelease{wantGPUOperator, wantNetworkOperator},
		},
		{
			name:     "configmap driver",
			resource: "configmaps",
			pages: []runtime.Object{
				configMapPage(helmConfigMap("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)),
				configMapPage(helmConfigMap("nvidia-network-operator", "network-operator", 1, "deployed",
					networkOperator)),
			},
			want: []installedRelease{wantGPUOperator, wantNetworkOperator},
		},
		{
			// The fold has to span pages, not just reduce within one, and it
			// must not depend on which page the winner arrived in.
			name:     "newest revision on the first page",
			resource: "secrets",
			pages: []runtime.Object{
				secretPage(helmSecret("gpu-operator", "gpu-operator", 5, "deployed", newer)),
				secretPage(helmSecret("gpu-operator", "gpu-operator", 3, "superseded", undecodablePayload)),
			},
			want: []installedRelease{wantNewer},
		},
		{
			name:     "newest revision on the last page",
			resource: "secrets",
			pages: []runtime.Object{
				secretPage(helmSecret("gpu-operator", "gpu-operator", 3, "superseded", undecodablePayload)),
				secretPage(helmSecret("gpu-operator", "gpu-operator", 5, "deployed", newer)),
			},
			want: []installedRelease{wantNewer},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Every page but the last carries a continue token, so a collector
			// that stops at the first page loses whatever the second holds.
			for i, page := range tt.pages[:len(tt.pages)-1] {
				accessor, err := meta.ListAccessor(page)
				if err != nil {
					t.Fatalf("list accessor for page %d: %v", i+1, err)
				}
				accessor.SetContinue(fmt.Sprintf("page-%d", i+2))
			}

			client := fake.NewSimpleClientset()
			var listed int
			var selectors []string
			client.PrependReactor("list", tt.resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
				listAction, ok := action.(k8stesting.ListActionImpl)
				if !ok {
					return true, nil, fmt.Errorf("unexpected action %T", action)
				}
				selectors = append(selectors, listAction.GetListRestrictions().Labels.String())

				listed++
				if listed > len(tt.pages) {
					return true, nil, stderrors.New("listed again after the last page")
				}

				return true, tt.pages[listed-1], nil
			})

			read, err := helmReleases(context.Background(), client, defaultTestScope())
			if err != nil {
				t.Fatalf("helmReleases() error = %v", err)
			}
			got := read.Releases
			if listed != len(tt.pages) {
				t.Errorf("listed %d pages, want %d", listed, len(tt.pages))
			}
			for i, selector := range selectors {
				if selector != "owner=helm" {
					t.Errorf("page %d listed with selector %q, want %q", i+1, selector, "owner=helm")
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("helmReleases() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestHelmReleasesRefusesRepeatedContinueToken pins the progress guard. A
// server that echoes one token would otherwise spin this cluster-scoped read
// until the deadline, issuing thousands of Lists on the way.
func TestHelmReleasesRefusesRepeatedContinueToken(t *testing.T) {
	payload := releasePayload(t, "gpu-operator", "gpu-operator", "deployed", "gpu-operator", "v25.3.3", 1)

	tests := []struct {
		name     string
		resource string
		page     func() runtime.Object
	}{
		{
			name:     "secret driver",
			resource: "secrets",
			page: func() runtime.Object {
				return &corev1.SecretList{
					ListMeta: metav1.ListMeta{Continue: "stuck"},
					Items: []corev1.Secret{
						*helmSecret("gpu-operator", "gpu-operator", 1, "deployed", payload),
					},
				}
			},
		},
		{
			name:     "configmap driver",
			resource: "configmaps",
			page: func() runtime.Object {
				return &corev1.ConfigMapList{
					ListMeta: metav1.ListMeta{Continue: "stuck"},
					Items: []corev1.ConfigMap{
						*helmConfigMap("gpu-operator", "gpu-operator", 1, "deployed", payload),
					},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			var listed int
			client.PrependReactor("list", tt.resource, func(_ k8stesting.Action) (bool, runtime.Object, error) {
				listed++

				return true, tt.page(), nil
			})

			read, err := helmReleases(context.Background(), client, defaultTestScope())
			if err == nil {
				t.Fatalf("helmReleases() = %+v, want an error", read)
			}
			if !reflect.DeepEqual(read, inventoryRead{}) {
				t.Errorf("helmReleases() returned %+v alongside an error", read)
			}
			// Two Lists: the first hands out the token, the second repeats it.
			if listed != 2 {
				t.Errorf("listed %d times before refusing, want 2", listed)
			}
			assertError(t, err, errors.ErrCodeInternal, map[string]any{"resource": tt.resource})
			if !strings.Contains(err.Error(), "continue token") {
				t.Errorf("error %q does not name the continue token", err)
			}
		})
	}
}

// TestHelmReleasesEchoesPagingOptions serves the release records over HTTP,
// through a real clientset, because that is the only place the paging request
// is observable: the fake clientset discards Limit and Continue before any
// reactor runs.
func TestHelmReleasesEchoesPagingOptions(t *testing.T) {
	gpuOperator := releasePayload(t, "gpu-operator", "gpu-operator", "deployed", "gpu-operator", "v25.3.3", 1)
	networkOperator := releasePayload(t, "network-operator", "nvidia-network-operator", "deployed",
		"network-operator", "v25.1.0", 1)

	pages := []*corev1.SecretList{
		{
			ListMeta: metav1.ListMeta{Continue: "page-2"},
			Items:    []corev1.Secret{*helmSecret("gpu-operator", "gpu-operator", 1, "deployed", gpuOperator)},
		},
		{
			Items: []corev1.Secret{
				*helmSecret("nvidia-network-operator", "network-operator", 1, "deployed", networkOperator),
			},
		},
	}

	var mu sync.Mutex
	var secretQueries []url.Values
	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/secrets":
			mu.Lock()
			secretQueries = append(secretQueries, r.URL.Query())
			page := pages[min(served, len(pages)-1)]
			served++
			mu.Unlock()
			page.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "SecretList"}
			if err := json.NewEncoder(w).Encode(page); err != nil {
				t.Errorf("encode secret page: %v", err)
			}
		case "/api/v1/configmaps":
			if err := json.NewEncoder(w).Encode(&corev1.ConfigMapList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMapList"},
			}); err != nil {
				t.Errorf("encode configmap page: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("clientset for %s: %v", server.URL, err)
	}

	read, err := helmReleases(context.Background(), client, defaultTestScope())
	if err != nil {
		t.Fatalf("helmReleases() error = %v", err)
	}
	got := read.Releases

	mu.Lock()
	defer mu.Unlock()
	if len(secretQueries) != len(pages) {
		t.Fatalf("served %d secret requests, want %d", len(secretQueries), len(pages))
	}
	wantLimit := strconv.FormatInt(defaults.HelmReleaseListPageSize, 10)
	for i, query := range secretQueries {
		if query.Get("limit") != wantLimit {
			t.Errorf("request %d sent limit=%q, want %q", i+1, query.Get("limit"), wantLimit)
		}
		if query.Get("labelSelector") != "owner=helm" {
			t.Errorf("request %d sent labelSelector=%q, want %q", i+1, query.Get("labelSelector"), "owner=helm")
		}
	}
	if token := secretQueries[0].Get("continue"); token != "" {
		t.Errorf("first request sent continue=%q, want it unset", token)
	}
	if token := secretQueries[1].Get("continue"); token != "page-2" {
		t.Errorf("second request sent continue=%q, want %q", token, "page-2")
	}

	want := []installedRelease{
		{
			Source: sourceHelm, Name: "gpu-operator", Namespace: "gpu-operator", Revision: 1, Status: "deployed",
			ChartName: "gpu-operator", ChartVersion: "v25.3.3", AppVersion: "25.3.3",
		},
		{
			Source: sourceHelm, Name: "network-operator", Namespace: "nvidia-network-operator", Revision: 1, Status: "deployed",
			ChartName: "network-operator", ChartVersion: "v25.1.0", AppVersion: "25.3.3",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("helmReleases() = %+v, want %+v", got, want)
	}
}
