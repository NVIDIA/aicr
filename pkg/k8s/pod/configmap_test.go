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

package pod_test

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/stretchr/testify/assert"
)

func TestParseConfigMapURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		uri           string
		wantNamespace string
		wantName      string
		wantErr       bool
	}{
		{
			name:          "valid URI",
			uri:           "cm://default/my-config",
			wantNamespace: "default",
			wantName:      "my-config",
			wantErr:       false,
		},
		{
			name:          "valid URI with spaces",
			uri:           "cm://default / my-config ",
			wantNamespace: "default",
			wantName:      "my-config",
			wantErr:       false,
		},
		{
			name:    "missing prefix",
			uri:     "default/my-config",
			wantErr: true,
		},
		{
			name:    "missing namespace",
			uri:     "cm:///my-config",
			wantErr: true,
		},
		{
			name:    "missing name",
			uri:     "cm://default/",
			wantErr: true,
		},
		{
			name:    "invalid format",
			uri:     "cm://invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			namespace, name, err := pod.ParseConfigMapURI(tt.uri)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantNamespace, namespace)
			assert.Equal(t, tt.wantName, name)
		})
	}
}
