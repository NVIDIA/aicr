// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package argocd provides ArgoCD Application generation for recipes.
package argocd

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/shared"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

//go:embed templates/application.yaml.tmpl
var applicationTemplate string

//go:embed templates/app-of-apps.yaml.tmpl
var appOfAppsTemplate string

//go:embed templates/README.md.tmpl
var readmeTemplate string

// ApplicationData contains data for rendering an ArgoCD Application.
type ApplicationData struct {
	Name        string
	Namespace   string
	Repository  string
	Chart       string
	Version     string
	SyncWave    int
	IsKustomize bool   // True when the component uses Kustomize instead of Helm
	Tag         string // Git ref for Kustomize components (tag, branch, or commit)
	Path        string // Path within the repository to the kustomization
}

// AppOfAppsData contains data for rendering the App of Apps manifest.
type AppOfAppsData struct {
	RepoURL        string
	TargetRevision string
	Path           string
}

// ReadmeData contains data for rendering the README.
type ReadmeData struct {
	RecipeVersion  string
	BundlerVersion string
	Components     []ApplicationData
}

// GeneratorInput contains all data needed to generate ArgoCD Applications.
type GeneratorInput struct {
	// RecipeResult contains the recipe metadata and component references.
	RecipeResult *recipe.RecipeResult

	// ComponentValues maps component names to their values.
	ComponentValues map[string]map[string]any

	// Version is the generator version.
	Version string

	// RepoURL is the Git repository URL for the app-of-apps manifest.
	// If empty, a placeholder URL will be used.
	RepoURL string

	// IncludeChecksums indicates whether to generate a checksums.txt file.
	IncludeChecksums bool
}

// GeneratorOutput contains the result of ArgoCD Application generation.
type GeneratorOutput struct {
	// Files contains the paths of generated files.
	Files []string

	// TotalSize is the total size of all generated files.
	TotalSize int64

	// Duration is the time taken to generate the applications.
	Duration time.Duration

	// DeploymentSteps contains ordered deployment instructions for the user.
	DeploymentSteps []string

	// DeploymentNotes contains optional notes (e.g., "Update repo URL").
	DeploymentNotes []string
}

// Generator creates ArgoCD Applications from recipe results.
type Generator struct{}

// NewGenerator creates a new ArgoCD application generator.
func NewGenerator() *Generator {
	return &Generator{}
}

// Generate creates ArgoCD Applications from the given input.
func (g *Generator) Generate(ctx context.Context, input *GeneratorInput, outputDir string) (*GeneratorOutput, error) {
	start := time.Now()

	output := &GeneratorOutput{
		Files: make([]string, 0),
	}

	if input == nil || input.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "input and recipe result are required")
	}

	// Create output directory
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create output directory", err)
	}

	// Sort components by deployment order
	components := shared.SortComponentRefsByDeploymentOrder(
		input.RecipeResult.ComponentRefs,
		input.RecipeResult.DeploymentOrder,
	)

	// Generate application data for each component (validate names early)
	appDataList := make([]ApplicationData, 0, len(components))
	for i, comp := range components {
		if !shared.IsSafePathComponent(comp.Name) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid component name %q: must not contain path separators or parent directory references", comp.Name))
		}

		isKustomize := comp.Type == recipe.ComponentTypeKustomize

		chartName := comp.Chart
		if chartName == "" {
			chartName = comp.Name
		}

		appData := ApplicationData{
			Name:        comp.Name,
			Namespace:   comp.Namespace,
			Repository:  comp.Source,
			Chart:       chartName,
			Version:     shared.NormalizeVersion(comp.Version),
			SyncWave:    i, // Use index as sync wave
			IsKustomize: isKustomize,
			Tag:         comp.Tag,
			Path:        comp.Path,
		}
		appDataList = append(appDataList, appData)
	}

	// Generate each component's directory and files
	for _, appData := range appDataList {
		select {
		case <-ctx.Done():
			return nil, errors.Wrap(errors.ErrCodeInternal, "context cancelled", ctx.Err())
		default:
		}

		componentDir, err := shared.SafeJoin(outputDir, appData.Name)
		if err != nil {
			return nil, err
		}
		if mkdirErr := os.MkdirAll(componentDir, 0755); mkdirErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to create directory for %s", appData.Name), mkdirErr)
		}

		// Generate application.yaml
		appPath, appSize, err := shared.GenerateFromTemplate(applicationTemplate, appData, componentDir, "application.yaml")
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to generate application.yaml for %s", appData.Name), err)
		}
		output.Files = append(output.Files, appPath)
		output.TotalSize += appSize

		// Generate values.yaml only for Helm components (kustomize uses source directly)
		if !appData.IsKustomize {
			values := input.ComponentValues[appData.Name]
			if values == nil {
				values = make(map[string]any)
			}
			valuesPath, valuesSize, err := shared.WriteValuesFile(values, componentDir, "values.yaml")
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("failed to generate values.yaml for %s", appData.Name), err)
			}
			output.Files = append(output.Files, valuesPath)
			output.TotalSize += valuesSize
		}
	}

	// Generate app-of-apps.yaml
	repoURL := input.RepoURL
	if repoURL == "" {
		repoURL = "https://github.com/YOUR-ORG/YOUR-REPO.git"
	}
	appOfAppsData := AppOfAppsData{
		RepoURL:        repoURL,
		TargetRevision: "main",
		Path:           ".",
	}
	appOfAppsPath, appOfAppsSize, err := shared.GenerateFromTemplate(appOfAppsTemplate, appOfAppsData, outputDir, "app-of-apps.yaml")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate app-of-apps.yaml", err)
	}
	output.Files = append(output.Files, appOfAppsPath)
	output.TotalSize += appOfAppsSize

	// Generate README.md
	readmeData := ReadmeData{
		RecipeVersion:  input.RecipeResult.Metadata.Version,
		BundlerVersion: input.Version,
		Components:     appDataList,
	}
	readmePath, readmeSize, err := shared.GenerateFromTemplate(readmeTemplate, readmeData, outputDir, "README.md")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate README.md", err)
	}
	output.Files = append(output.Files, readmePath)
	output.TotalSize += readmeSize

	// Generate checksums if requested
	if input.IncludeChecksums {
		if err := checksum.GenerateChecksums(ctx, outputDir, output.Files); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate checksums", err)
		}
		checksumPath := checksum.GetChecksumFilePath(outputDir)
		checksumInfo, statErr := os.Stat(checksumPath)
		if statErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to stat checksums file", statErr)
		}
		output.Files = append(output.Files, checksumPath)
		output.TotalSize += checksumInfo.Size()
	}

	output.Duration = time.Since(start)

	// Populate deployment steps for CLI output
	output.DeploymentSteps = []string{
		"Push the generated files to your GitOps repository",
		fmt.Sprintf("kubectl apply -f %s/app-of-apps.yaml", outputDir),
	}
	// Add note if repo URL needs to be updated
	if input.RepoURL == "" {
		output.DeploymentNotes = []string{
			"Update app-of-apps.yaml with your repository URL before applying",
		}
	}

	slog.Debug("argocd applications generated",
		"components", len(appDataList),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
	)

	return output, nil
}
