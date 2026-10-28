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
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"oras.land/oras-go/v2/registry/remote/auth"
)

// dockerConfig is the subset of a docker config.json the operator reads.
type dockerConfig struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
}

// DockerConfigCredentials returns a credential function over a docker config
// JSON (the `.dockerconfigjson` key of a kubernetes.io/dockerconfigjson
// Secret). A host without an entry is anonymous. Errors never include the
// credential.
func DockerConfigCredentials(dockerConfigJSON []byte) (auth.CredentialFunc, error) {
	var cfg dockerConfig
	if err := json.Unmarshal(dockerConfigJSON, &cfg); err != nil {
		return nil, invalid("registry credentials are not a docker config")
	}
	creds := make(map[string]auth.Credential, len(cfg.Auths))
	for host, a := range cfg.Auths {
		user, pass := a.Username, a.Password
		if a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, invalid("registry credentials for %s are not base64 user:password", host)
			}
			var ok bool
			if user, pass, ok = strings.Cut(string(raw), ":"); !ok {
				return nil, invalid("registry credentials for %s are not user:password", host)
			}
		}
		creds[normalizeHost(host)] = auth.Credential{Username: user, Password: pass}
	}
	return func(_ context.Context, hostport string) (auth.Credential, error) {
		if c, ok := creds[normalizeHost(hostport)]; ok {
			return c, nil
		}
		return auth.EmptyCredential, nil
	}, nil
}

// normalizeHost strips a scheme and path, as docker config keys may carry them.
func normalizeHost(host string) string {
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host, _, _ = strings.Cut(host, "/")
	return host
}
