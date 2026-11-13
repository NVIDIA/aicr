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

package uatbroker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

const awsClusterConfig = `
compute:
  eks:
    nodeGroups:
      system:
        instanceType: m7i.xlarge
      workers:
        - name: cpu-worker
          instanceType: m7i.4xlarge
        - name: gpu-worker
          instanceType: p5.48xlarge
          capacity:
            desired: 2
            reservation:
              preference: capacity-reservations-only
              target: cr-aaa
`

const gcpClusterConfig = `
compute:
  gke:
    nodePools:
      workers:
        - name: gpu-worker
          nodeConfig:
            capacityReservations:
              - projects/p/reservations/r
`

func TestParseClusterConfigReservations(t *testing.T) {
	tests := []struct {
		name    string
		cloud   string
		yaml    string
		want    []reservationRef
		wantErr bool
	}{
		{
			name:  "aws gpu worker target",
			cloud: CloudAWS,
			yaml:  awsClusterConfig,
			want:  []reservationRef{{Worker: "gpu-worker", Target: "cr-aaa"}},
		},
		{
			name:  "aws reservation block without target",
			cloud: CloudAWS,
			yaml: `
compute:
  eks:
    nodeGroups:
      workers:
        - name: gpu-worker
          capacity:
            reservation:
              preference: capacity-reservations-only
`,
		},
		{
			name:  "aws ignores gke section",
			cloud: CloudAWS,
			yaml:  gcpClusterConfig,
		},
		{
			name:  "gcp capacity reservations",
			cloud: CloudGCP,
			yaml: `
compute:
  gke:
    nodePools:
      workers:
        - name: gpu-a
          nodeConfig:
            capacityReservations: [projects/p/reservations/r1, projects/p/reservations/r2]
        - name: cpu
          nodeConfig: {}
`,
			want: []reservationRef{
				{Worker: "gpu-a", Target: "projects/p/reservations/r1"},
				{Worker: "gpu-a", Target: "projects/p/reservations/r2"},
			},
		},
		{
			name:  "gcp ignores eks section",
			cloud: CloudGCP,
			yaml:  awsClusterConfig,
		},
		{
			name:  "azure declares no reservation field",
			cloud: CloudAzure,
			yaml:  awsClusterConfig,
		},
		{
			name:  "kind declares no reservation field",
			cloud: CloudKind,
			yaml:  "kind: NvkindClusterDescriptor\n",
		},
		{
			name:  "empty document",
			cloud: CloudAWS,
			yaml:  "",
		},
		{
			name:    "malformed yaml",
			cloud:   CloudAWS,
			yaml:    "compute: [unterminated",
			wantErr: true,
		},
		{
			name:    "wrong shape",
			cloud:   CloudAWS,
			yaml:    "compute:\n  eks:\n    nodeGroups:\n      workers: not-a-list\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseClusterConfigReservations(tt.cloud, []byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("refs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckClusterConfigReservations(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		rows  []Reservation
		// wantErr lists substrings the error must contain. Empty means no error.
		wantErr []string
	}{
		{
			name:  "aws match",
			files: map[string]string{"aws.yaml": awsClusterConfig},
			rows:  []Reservation{{Name: "aws-a", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "aws.yaml"}},
		},
		{
			name:    "aws drift",
			files:   map[string]string{"aws.yaml": awsClusterConfig},
			rows:    []Reservation{{Name: "aws-a", Cloud: CloudAWS, ReservationID: "cr-bbb", ClusterConfigPath: "aws.yaml"}},
			wantErr: []string{`reservation aws-a has reservation-id cr-bbb but aws.yaml worker "gpu-worker" targets cr-aaa`},
		},
		{
			name:  "gcp match",
			files: map[string]string{"gcp.yaml": gcpClusterConfig},
			rows:  []Reservation{{Name: "gcp-a", Cloud: CloudGCP, ReservationID: "projects/p/reservations/r", ClusterConfigPath: "gcp.yaml"}},
		},
		{
			name:    "gcp drift",
			files:   map[string]string{"gcp.yaml": gcpClusterConfig},
			rows:    []Reservation{{Name: "gcp-a", Cloud: CloudGCP, ReservationID: "projects/p/reservations/other", ClusterConfigPath: "gcp.yaml"}},
			wantErr: []string{"targets projects/p/reservations/r"},
		},
		{
			name:    "reservation-id but no target",
			files:   map[string]string{"aws.yaml": "compute: {}\n"},
			rows:    []Reservation{{Name: "aws-a", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "aws.yaml"}},
			wantErr: []string{"reservation aws-a has reservation-id cr-aaa but aws.yaml declares no capacity reservation"},
		},
		{
			name:    "reservation-id on a cloud with no reservation field",
			files:   map[string]string{"azure.yaml": "compute: {}\n"},
			rows:    []Reservation{{Name: "azure-a", Cloud: CloudAzure, ReservationID: "cr-aaa", ClusterConfigPath: "azure.yaml"}},
			wantErr: []string{"declares no capacity reservation"},
		},
		{
			name:  "quota-backed row with no target",
			files: map[string]string{"azure.yaml": "compute: {}\n"},
			rows:  []Reservation{{Name: "azure-a", Cloud: CloudAzure, ClusterConfigPath: "azure.yaml"}},
		},
		{
			name:    "no reservation-id but a target",
			files:   map[string]string{"aws.yaml": awsClusterConfig},
			rows:    []Reservation{{Name: "aws-a", Cloud: CloudAWS, ClusterConfigPath: "aws.yaml"}},
			wantErr: []string{`reservation aws-a has no reservation-id but aws.yaml worker "gpu-worker" targets cr-aaa`},
		},
		{
			name:  "two slots share one config",
			files: map[string]string{"aws.yaml": awsClusterConfig},
			rows: []Reservation{
				{Name: "aws-1", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "aws.yaml"},
				{Name: "aws-2", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "aws.yaml"},
			},
		},
		{
			name: "every mismatch is reported",
			files: map[string]string{
				"aws.yaml": awsClusterConfig,
				"gcp.yaml": gcpClusterConfig,
			},
			rows: []Reservation{
				{Name: "aws-1", Cloud: CloudAWS, ReservationID: "cr-new", ClusterConfigPath: "aws.yaml"},
				{Name: "gcp-1", Cloud: CloudGCP, ReservationID: "projects/p/reservations/new", ClusterConfigPath: "gcp.yaml"},
			},
			wantErr: []string{"reservation aws-1 ", "reservation gcp-1 "},
		},
		{
			name:  "nested path",
			files: map[string]string{"tests/uat/aws/cluster-config.yaml": awsClusterConfig},
			rows:  []Reservation{{Name: "aws-a", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "tests/uat/aws/cluster-config.yaml"}},
		},
		{
			name:    "missing file",
			rows:    []Reservation{{Name: "aws-a", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "missing.yaml"}},
			wantErr: []string{"open cluster-config"},
		},
		{
			name:    "path escapes root",
			rows:    []Reservation{{Name: "aws-a", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "../outside.yaml"}},
			wantErr: []string{"must be a relative path inside the repository"},
		},
		{
			name:    "absolute path",
			rows:    []Reservation{{Name: "aws-a", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "/etc/cluster-config.yaml"}},
			wantErr: []string{"must be a relative path inside the repository"},
		},
		{
			name:    "malformed cluster-config",
			files:   map[string]string{"aws.yaml": "compute: [unterminated"},
			rows:    []Reservation{{Name: "aws-a", Cloud: CloudAWS, ReservationID: "cr-aaa", ClusterConfigPath: "aws.yaml"}},
			wantErr: []string{"reservation aws-a cluster-config aws.yaml"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, body := range tt.files {
				path := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reg := &Registry{Reservations: tt.rows}
			err := reg.CheckClusterConfigReservations(root)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestCommittedClusterConfigsMatchRegistry fails when a committed
// reservation-id and its cluster-config's reservation targets disagree.
func TestCommittedClusterConfigsMatchRegistry(t *testing.T) {
	root := filepath.Join("..", "..")
	reg, err := LoadRegistryFile(filepath.Join(root, "infra", "uat", "reservations.yaml"))
	if err != nil {
		t.Fatalf("committed reservations.yaml invalid: %v", err)
	}
	if err := reg.CheckClusterConfigReservations(root); err != nil {
		t.Fatal(err)
	}
}

// deployment returns a cluster-config whose deployment block carries the given
// raw YAML scalars, so a case can write a quoted, unquoted or null value.
func deployment(tenancy, location, state, stateLocation string) string {
	return "deployment:\n" +
		"  tenancy: " + tenancy + "\n" +
		"  location: " + location + "\n" +
		"  state: " + state + "\n" +
		"  stateLocation: " + stateLocation + "\n"
}

func TestAWSJanitorTargets(t *testing.T) {
	east1 := deployment(`"111111111111"`, "us-east-1", "tenancy", `""`)
	aws := func(name, cfg string) Reservation {
		return Reservation{Name: name, Cloud: CloudAWS, ClusterConfigPath: cfg}
	}
	type testCase struct {
		name  string
		files map[string]string
		rows  []Reservation
		want  []AWSJanitorTarget
		// wantErr lists substrings the error must contain. Empty means no error.
		wantErr []string
	}
	tests := []testCase{
		{
			name:  "rows sharing a config collapse to one target",
			files: map[string]string{"a.yaml": east1},
			rows:  []Reservation{aws("a1", "a.yaml"), aws("a2", "a.yaml")},
			want:  []AWSJanitorTarget{{Tenancy: "111111111111", Location: "us-east-1", Config: "a.yaml"}},
		},
		{
			// Neither registry order nor config-name order is the sorted order.
			name: "sorted by tenancy then location",
			files: map[string]string{
				"a.yaml": deployment(`"222222222222"`, "us-east-1", "tenancy", `""`),
				"c.yaml": deployment(`"111111111111"`, "us-west-2", "tenancy", `""`),
				"b.yaml": east1,
			},
			rows: []Reservation{aws("r1", "a.yaml"), aws("r2", "c.yaml"), aws("r3", "b.yaml")},
			want: []AWSJanitorTarget{
				{Tenancy: "111111111111", Location: "us-east-1", Config: "b.yaml"},
				{Tenancy: "111111111111", Location: "us-west-2", Config: "c.yaml"},
				{Tenancy: "222222222222", Location: "us-east-1", Config: "a.yaml"},
			},
		},
		{
			// q.yaml is first in registry order but not alphabetically.
			name: "configs agreeing on state share the pair's first config",
			files: map[string]string{
				"q.yaml": deployment(`"111111111111"`, "us-east-2", "tenancy", "us-east-1"),
				"p.yaml": deployment(`"111111111111"`, "us-east-2", "tenancy", "us-east-1"),
			},
			rows: []Reservation{aws("b1", "q.yaml"), aws("b2", "p.yaml")},
			want: []AWSJanitorTarget{{Tenancy: "111111111111", Location: "us-east-2", Config: "q.yaml"}},
		},
		{
			// Non-aws configs are absent, so reading one would fail the case.
			name:  "only aws rows are read",
			files: map[string]string{"a.yaml": east1},
			rows: []Reservation{
				{Name: "g1", Cloud: CloudGCP, ClusterConfigPath: "absent-gcp.yaml"},
				aws("a1", "a.yaml"),
				{Name: "z1", Cloud: CloudAzure, ClusterConfigPath: "absent-azure.yaml"},
				{Name: "k1", Cloud: CloudKind, ClusterConfigPath: "absent-kind.yaml"},
			},
			want: []AWSJanitorTarget{{Tenancy: "111111111111", Location: "us-east-1", Config: "a.yaml"}},
		},
		{
			name:  "unquoted tenancy keeps its digits",
			files: map[string]string{"a.yaml": deployment("036338176747", "us-east-1", "tenancy", `""`)},
			rows:  []Reservation{aws("a1", "a.yaml")},
			want:  []AWSJanitorTarget{{Tenancy: "036338176747", Location: "us-east-1", Config: "a.yaml"}},
		},
		{
			name: "unequal stateLocation in one pair",
			files: map[string]string{
				"a.yaml": east1,
				"b.yaml": deployment(`"111111111111"`, "us-east-1", "tenancy", "us-west-2"),
			},
			rows:    []Reservation{aws("a1", "a.yaml"), aws("b1", "b.yaml")},
			wantErr: []string{"b.yaml (reservation b1) and a.yaml both target 111111111111/us-east-1", ".deployment.state/.stateLocation"},
		},
		{
			name: "unequal state in one pair",
			files: map[string]string{
				"a.yaml": east1,
				"b.yaml": deployment(`"111111111111"`, "us-east-1", "local", `""`),
			},
			rows:    []Reservation{aws("a1", "a.yaml"), aws("b1", "b.yaml")},
			wantErr: []string{"b.yaml (reservation b1) and a.yaml", `"local"/"" vs "tenancy"/""`},
		},
		{
			name:    "no aws row",
			files:   map[string]string{"a.yaml": east1},
			rows:    []Reservation{{Name: "g1", Cloud: CloudGCP, ClusterConfigPath: "a.yaml"}},
			wantErr: []string{"no aws reservation"},
		},
		{
			name:    "missing cluster-config",
			rows:    []Reservation{aws("a1", "absent.yaml")},
			wantErr: []string{"open cluster-config", "absent.yaml"},
		},
		{
			name:    "absolute cluster-config path",
			rows:    []Reservation{aws("a1", "/etc/cluster-config.yaml")},
			wantErr: []string{"reservation a1 cluster-config-path", "must be a relative path inside the repository"},
		},
		{
			name:    "cluster-config path escaping the root",
			rows:    []Reservation{aws("a1", "c/../../outside.yaml")},
			wantErr: []string{"must be a relative path inside the repository"},
		},
		{
			name:    "malformed cluster-config",
			files:   map[string]string{"a.yaml": "deployment: [unterminated"},
			rows:    []Reservation{aws("a1", "a.yaml")},
			wantErr: []string{"reservation a1 cluster-config a.yaml"},
		},
		{
			name:    "non-scalar tenancy",
			files:   map[string]string{"a.yaml": deployment("[1]", "us-east-1", "tenancy", `""`)},
			rows:    []Reservation{aws("a1", "a.yaml")},
			wantErr: []string{"reservation a1 cluster-config a.yaml", "cannot unmarshal"},
		},
		{
			name:    "no deployment block",
			files:   map[string]string{"a.yaml": "compute: {}\n"},
			rows:    []Reservation{aws("a1", "a.yaml")},
			wantErr: []string{`.deployment.tenancy ""`},
		},
	}
	badTenancies := []string{`""`, "null", `"12345678901"`, `"1234567890123"`, `"12345678901a"`, "abcdefghijkl"}
	badLocations := []string{`""`, "null", `"us east-1"`, `"us-east-1;touch"`, `'$(id)'`, "US-EAST-1"}
	rejected := make([]testCase, 0, len(badTenancies)+len(badLocations))
	for _, tenancy := range badTenancies {
		rejected = append(rejected, testCase{
			name:    "tenancy " + tenancy + " is rejected",
			files:   map[string]string{"a.yaml": deployment(tenancy, "us-east-1", "tenancy", `""`)},
			rows:    []Reservation{aws("a1", "a.yaml")},
			wantErr: []string{"reservation a1 cluster-config a.yaml: .deployment.tenancy"},
		})
	}
	for _, location := range badLocations {
		rejected = append(rejected, testCase{
			name:    "location " + location + " is rejected",
			files:   map[string]string{"a.yaml": deployment(`"111111111111"`, location, "tenancy", `""`)},
			rows:    []Reservation{aws("a1", "a.yaml")},
			wantErr: []string{"reservation a1 cluster-config a.yaml: .deployment.location"},
		})
	}
	for _, tt := range slices.Concat(tests, rejected) {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, body := range tt.files {
				path := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reg := &Registry{Reservations: tt.rows}
			got, err := reg.AWSJanitorTargets(root)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !slices.Equal(got, tt.want) {
					t.Errorf("AWSJanitorTargets() = %+v, want %+v", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got targets %+v", tt.wantErr, got)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestCommittedAWSJanitorTargets fails when the committed aws rows'
// cluster-configs cannot yield the janitor's AWS matrix. It pins no pair, so
// onboarding a reservation does not break it.
func TestCommittedAWSJanitorTargets(t *testing.T) {
	root := filepath.Join("..", "..")
	reg, err := LoadRegistryFile(filepath.Join(root, "infra", "uat", "reservations.yaml"))
	if err != nil {
		t.Fatalf("committed reservations.yaml invalid: %v", err)
	}
	targets, err := reg.AWSJanitorTargets(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) == 0 {
		t.Fatal("committed registry yields no AWS janitor targets")
	}
}
