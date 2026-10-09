// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/measurement"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// SubtypeDefaultStorageClass records which StorageClasses are marked as
	// the cluster default, the class a PVC without storageClassName binds
	// through. It does not report provisioner health or capacity.
	SubtypeDefaultStorageClass = "default-storage-class"

	// Both annotations are honored by the DefaultStorageClass admission
	// plugin (k8s.io/component-helpers storageutil.IsDefaultAnnotation).
	defaultStorageClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	defaultStorageClassBetaAnnotation = "storageclass.beta.kubernetes.io/is-default-class"

	storageClassListPageSize = 500

	storageClassKeyCollectionState = "collection-state"
	storageClassKeyDefaultCount    = "default-count"
	storageClassKeyDefaultClasses  = "default-classes"

	storageClassStatePresent  = "present"
	storageClassStateMultiple = "multiple"
	storageClassStateAbsent   = "absent"
	storageClassStateUnknown  = "unknown"
)

type defaultStorageClassSummary struct {
	state    string
	defaults []string
}

func (s defaultStorageClassSummary) subtype() measurement.Subtype {
	data := map[string]measurement.Reading{
		storageClassKeyCollectionState: measurement.Str(s.state),
	}
	if s.state != storageClassStateUnknown {
		data[storageClassKeyDefaultCount] = measurement.Int(len(s.defaults))
	}
	if len(s.defaults) > 0 {
		data[storageClassKeyDefaultClasses] = measurement.Str(strings.Join(s.defaults, ","))
	}
	return measurement.Subtype{Name: SubtypeDefaultStorageClass, Data: data}
}

func unknownDefaultStorageClassSubtype() measurement.Subtype {
	return defaultStorageClassSummary{state: storageClassStateUnknown}.subtype()
}

func isDefaultStorageClass(annotations map[string]string) bool {
	return annotations[defaultStorageClassAnnotation] == "true" ||
		annotations[defaultStorageClassBetaAnnotation] == "true"
}

// collectDefaultStorageClass records the StorageClasses annotated as the
// cluster default. More than one default is recorded as its own state:
// Kubernetes then binds default-class PVCs to the newest, which is rarely
// what the operator intended.
func (k *Collector) collectDefaultStorageClass(ctx context.Context) measurement.Subtype {
	if err := ctx.Err(); err != nil {
		slog.Warn("default StorageClass collection cancelled", slog.String("error", err.Error()))
		return unknownDefaultStorageClassSubtype()
	}
	if k.ClientSet == nil {
		slog.Warn("default StorageClass collection client unavailable")
		return unknownDefaultStorageClassSubtype()
	}

	var defaults []string
	opts := metav1.ListOptions{Limit: storageClassListPageSize}
	for {
		if err := ctx.Err(); err != nil {
			slog.Warn("default StorageClass collection cancelled", slog.String("error", err.Error()))
			return unknownDefaultStorageClassSubtype()
		}
		page, err := k.ClientSet.StorageV1().StorageClasses().List(ctx, opts)
		if err != nil {
			slog.Warn("failed to list StorageClasses; recording unknown",
				slog.String("error", err.Error()))
			return unknownDefaultStorageClassSubtype()
		}
		if page == nil {
			slog.Warn("StorageClass list returned no response; recording unknown")
			return unknownDefaultStorageClassSubtype()
		}
		for i := range page.Items {
			if isDefaultStorageClass(page.Items[i].Annotations) {
				defaults = append(defaults, page.Items[i].Name)
			}
		}
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}

	slices.Sort(defaults)
	summary := defaultStorageClassSummary{defaults: defaults}
	switch len(defaults) {
	case 0:
		summary.state = storageClassStateAbsent
	case 1:
		summary.state = storageClassStatePresent
	default:
		summary.state = storageClassStateMultiple
	}
	return summary.subtype()
}
