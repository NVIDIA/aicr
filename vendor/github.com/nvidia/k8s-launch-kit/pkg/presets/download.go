// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package presets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultRepo   = "nvidia/k8s-launch-kit"
	defaultBranch = "main"
	httpTimeout   = 30 * time.Second
)

// githubEntry represents a single entry from the GitHub contents API response.
type githubEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
}

// DownloadOptions configures the preset download behavior.
type DownloadOptions struct {
	Repo       string // GitHub repository (default: nvidia/k8s-launch-kit)
	Branch     string // Git branch (default: main)
	Dir        string // Destination directory (empty = auto-resolve via GetPresetsDir)
	HTTPClient *http.Client
}

// DefaultDownloadOptions returns DownloadOptions with default values.
func DefaultDownloadOptions() DownloadOptions {
	return DownloadOptions{
		Repo:   defaultRepo,
		Branch: defaultBranch,
	}
}

// DownloadPresets fetches the latest topology presets from a GitHub repository
// and writes them to the local presets directory.
// It returns the list of machine types that were downloaded.
func DownloadPresets(ctx context.Context, opts DownloadOptions) ([]string, error) {
	return downloadPresetsWithBaseURL(ctx, opts, "")
}

// downloadPresetsWithBaseURL is the internal implementation of DownloadPresets.
// When baseURL is non-empty, it replaces the default GitHub API and raw content
// hosts — used by tests with httptest servers.
func downloadPresetsWithBaseURL(ctx context.Context, opts DownloadOptions, baseURL string) ([]string, error) {
	if opts.Repo == "" {
		opts.Repo = defaultRepo
	}
	if opts.Branch == "" {
		opts.Branch = defaultBranch
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}

	destDir := opts.Dir
	if destDir == "" {
		// Try to resolve existing presets dir, fall back to default install location
		dir, err := GetPresetsDir()
		if err != nil {
			return nil, err
		}
		if dir != "" {
			destDir = dir
		} else {
			destDir = "/usr/local/share/l8k/presets"
		}
	}

	// Step 1: List machine type directories from GitHub
	apiHost := "https://api.github.com"
	rawHost := "https://raw.githubusercontent.com"
	if baseURL != "" {
		apiHost = baseURL
		rawHost = baseURL
	}

	apiURL := fmt.Sprintf("%s/repos/%s/contents/presets?ref=%s", apiHost, opts.Repo, opts.Branch)
	entries, err := listGitHubDir(ctx, client, apiURL)
	if err != nil {
		return nil, fmt.Errorf("failed to list presets from GitHub: %w", err)
	}

	// Step 2: Download topology.yaml for each machine type directory
	var downloaded []string
	for _, entry := range entries {
		if entry.Type != "dir" {
			continue
		}

		rawURL := fmt.Sprintf("%s/%s/%s/presets/%s/topology.yaml",
			rawHost, opts.Repo, opts.Branch, entry.Name)

		data, err := fetchURL(ctx, client, rawURL)
		if err != nil {
			return downloaded, fmt.Errorf("failed to download preset %s: %w", entry.Name, err)
		}

		// Create machine type directory and write topology.yaml
		machineDir := filepath.Join(destDir, entry.Name)
		if err := os.MkdirAll(machineDir, 0o755); err != nil {
			return downloaded, fmt.Errorf("failed to create directory %s: %w", machineDir, err)
		}

		topoPath := filepath.Join(machineDir, "topology.yaml")
		if err := os.WriteFile(topoPath, data, 0o644); err != nil {
			return downloaded, fmt.Errorf("failed to write %s: %w", topoPath, err)
		}

		downloaded = append(downloaded, entry.Name)
	}

	return downloaded, nil
}

// listGitHubDir fetches a GitHub contents API endpoint and returns the entries.
func listGitHubDir(ctx context.Context, client *http.Client, url string) ([]githubEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	// Use GITHUB_TOKEN for authenticated requests if available
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var entries []githubEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub API response: %w", err)
	}

	return entries, nil
}

// fetchURL performs a GET request and returns the response body.
func fetchURL(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	// Use GITHUB_TOKEN for authenticated requests if available
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	return io.ReadAll(resp.Body)
}
