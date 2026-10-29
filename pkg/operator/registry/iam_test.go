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

package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The operator's IAM policy grants only an authorization token and push and
// pull on the runtime repository prefix.
func TestECRPolicyIsLeastPrivilege(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "aicr-operator", "files", "ecr-iam-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Statement []struct {
			Effect   string
			Action   any
			Resource string
		}
	}
	if err = json.Unmarshal(raw, &policy); err != nil {
		t.Fatalf("policy is not JSON: %v", err)
	}
	pushPull := []string{
		"ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:CompleteLayerUpload", "ecr:GetDownloadUrlForLayer",
		"ecr:InitiateLayerUpload", "ecr:PutImage", "ecr:UploadLayerPart",
	}
	for _, s := range policy.Statement {
		if s.Effect != "Allow" {
			t.Fatalf("unexpected effect %q", s.Effect)
		}
		var actions []string
		switch a := s.Action.(type) {
		case string:
			actions = append(actions, a)
		case []any:
			for _, x := range a {
				actions = append(actions, x.(string))
			}
		}
		for _, a := range actions {
			if strings.Contains(a, "*") {
				t.Errorf("wildcard action %q", a)
			}
			switch {
			case a == "ecr:GetAuthorizationToken":
				if s.Resource != "*" || len(actions) != 1 {
					t.Errorf("GetAuthorizationToken statement = %+v", s)
				}
			case slices.Contains(pushPull, a):
				if !strings.HasSuffix(s.Resource, ":repository/${REPOSITORY_PREFIX}/*") {
					t.Errorf("%s is not scoped to the runtime prefix: %s", a, s.Resource)
				}
			default:
				t.Errorf("action %s is not push, pull or token", a)
			}
		}
	}
}
