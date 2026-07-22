//
// Copyright 2026 The Sigstore Authors.
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

package util

import (
	"fmt"
	"os"

	"github.com/sigstore/sigstore-go/pkg/tuf"
)

// GetTUFClient gets a TUF client based on the flags
func GetTUFClient(tufRepository string, tufRootPath string) (*tuf.Client, error) {
	switch tufRepository {
	case "default":
		if tufRootPath != "" {
			return nil, fmt.Errorf("tuf-root-path is not supported when using the default TUF repository")
		}
		return tuf.DefaultClient()
	case "staging":
		if tufRootPath != "" {
			return nil, fmt.Errorf("tuf-root-path is not supported when using the staging TUF repository")
		}
		options := tuf.DefaultOptions().WithRoot(tuf.StagingRoot()).WithRepositoryBaseURL(tuf.StagingMirror)
		return tuf.New(options)
	default:
		fmt.Printf("Using custom TUF repository: %s\n", tufRepository)
		if tufRootPath == "" {
			return nil, fmt.Errorf("tuf-root-path is required when using a custom TUF repository")
		}
		rootBytes, err := os.ReadFile(tufRootPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read TUF root path: %w", err)
		}
		options := tuf.DefaultOptions().WithRoot(rootBytes).WithRepositoryBaseURL(tufRepository)
		return tuf.New(options)
	}
}
