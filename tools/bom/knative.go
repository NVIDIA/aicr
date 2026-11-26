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

package main

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/sets"

	"aicr.run/pkg/bom"
	"aicr.run/pkg/errors"
)

const (
	knativeOperatorComponent = "knative-operator"
	knativeServingValues     = "components/knative-serving/values.yaml"

	// knativeOperatorRepository is the repository of the operator image the
	// knative-operator chart deploys. The image holds the manifests the
	// operator applies under knativeKodataRoot.
	knativeOperatorRepository = "gcr.io/knative-releases/knative.dev/operator/cmd/operator"
	knativeKodataRoot         = "var/run/ko"

	// knativeIngress is the ingress the knative-serving KnativeServing
	// resource enables.
	knativeIngress = "kourier"
)

// imageFileReader returns the YAML files directly under each of dirs in
// image, keyed by path, as imagefs.ReadYAML does.
type imageFileReader func(ctx context.Context, image string, dirs ...string) (map[string][]byte, error)

// verifyKnativeKodata compares the images knative-operator's images.yaml
// declares for the surveyed pin with the images in the manifests its operator
// image carries for the Serving release knative-serving requests, and for that
// release's Kourier ingress. Any difference fails. A failure to fetch or read
// the image is a warning on res, which fails a strict run.
func verifyKnativeKodata(ctx context.Context, repoRoot string, res *bom.ComponentResult, read imageFileReader) error {
	errCtx := map[string]any{componentContextKey: res.Name}
	operatorImage, err := knativeOperatorImage(res.Images)
	if err != nil {
		return err
	}
	servingVersion, err := knativeServingVersion(repoRoot)
	if err != nil {
		return err
	}
	dirs := []string{
		path.Join(knativeKodataRoot, "knative-serving", servingVersion),
		path.Join(knativeKodataRoot, "ingress", majorMinor(servingVersion), knativeIngress),
	}
	files, err := read(ctx, operatorImage, dirs...)
	if stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("read manifests from %s: %v", operatorImage, err))
		return nil
	}
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "read manifests from "+operatorImage)
	}

	kodata := sets.New[string]()
	for _, dir := range dirs {
		found := false
		for p, data := range files {
			if path.Dir(p) != dir {
				continue
			}
			found = true
			set, extractErr := bom.ExtractImageSetFromYAML(data)
			if extractErr != nil {
				return errors.WrapWithContext(errors.ErrCodeInvalidRequest, "extract images from "+p, extractErr, errCtx)
			}
			kodata.Insert(set.Images...)
		}
		if !found {
			return errors.NewWithContext(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"%s holds no manifests under %s, so it does not bundle the Serving release %s requests",
				operatorImage, dir, knativeServingValues), errCtx)
		}
	}

	d, found, err := loadDeclaredImages(repoRoot, res.Name)
	if err != nil {
		return err
	}
	if !found {
		return errors.NewWithContext(errors.ErrCodeNotFound, bom.DeclaredImagesFile+" is absent", errCtx)
	}
	entry, err := d.ChartFor(res.Repository, res.Chart, res.Version)
	if err != nil {
		return err
	}
	declared := sets.New[string]()
	for _, i := range entry.Images {
		declared.Insert(i.Image)
	}

	var problems []string
	for _, i := range sets.List(kodata.Difference(declared)) {
		problems = append(problems, "undeclared "+i)
	}
	for _, i := range sets.List(declared.Difference(kodata)) {
		problems = append(problems, "not in the operator image "+i)
	}
	if len(problems) > 0 {
		return errors.NewWithContext(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"component %q: %s for %s differs from the Serving %s and %s manifests in %s: %s",
			res.Name, bom.DeclaredImagesFile, res.Version, servingVersion, knativeIngress, operatorImage,
			strings.Join(problems, "; ")), errCtx)
	}
	return nil
}

// verifyKnativeResult runs verifyKnativeKodata on the knative-operator result,
// unless the survey has none or that result already has a warning, such as a
// failed render, which fails a strict run.
func verifyKnativeResult(ctx context.Context, repoRoot string, results []bom.ComponentResult, read imageFileReader) error {
	for i := range results {
		if results[i].Name != knativeOperatorComponent || len(results[i].Warnings) > 0 {
			continue
		}
		if err := verifyKnativeKodata(ctx, repoRoot, &results[i], read); err != nil {
			return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "verify knative-operator declared images")
		}
	}
	return nil
}

// knativeOperatorImage returns the single image in images from
// knativeOperatorRepository.
func knativeOperatorImage(images []string) (string, error) {
	var found []string
	for _, i := range images {
		named, err := reference.ParseNormalizedNamed(i)
		if err == nil && named.Name() == knativeOperatorRepository {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		return "", errors.NewWithContext(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"expected exactly one %s image in the rendered chart, found %v", knativeOperatorRepository, found),
			map[string]any{componentContextKey: knativeOperatorComponent})
	}
	return found[0], nil
}

// knativeServingVersion returns the Serving release knative-serving's
// values.yaml requests.
func knativeServingVersion(repoRoot string) (string, error) {
	root, err := os.OpenRoot(filepath.Join(repoRoot, "recipes"))
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "open recipes root", err)
	}
	defer root.Close() // read-only handle
	data, err := readBoundedFile(root, knativeServingValues)
	if err != nil {
		return "", err
	}
	var values struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, "parse "+knativeServingValues, err)
	}
	if values.Version == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest, knativeServingValues+" sets no version")
	}
	return values.Version, nil
}

// majorMinor returns "1.23" for "1.23.0".
func majorMinor(version string) string {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}
