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

package helper

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GroupVersionResources fetches the APIResourceList for gv with the caller's
// context. DiscoveryInterface.ServerResourcesForGroupVersion issues its request
// with context.TODO() internally (client-go), so an unresponsive apiserver
// could outlive the validator timeout and hang until the Job is killed;
// issuing the same GET through the discovery REST client keeps the request
// cancelable. Fake discovery clients in tests expose no RESTClient — fall back
// to the interface method there (test-only, in-memory, no I/O).
func GroupVersionResources(ctx context.Context, clientset kubernetes.Interface, gv string) (*metav1.APIResourceList, error) {
	disc := clientset.Discovery()
	rc := disc.RESTClient()
	if rc == nil {
		// The interface method cannot carry ctx — honor it around the call so
		// a canceled probe still aborts (fake clients are in-memory, so the
		// call itself cannot hang).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		resources, err := disc.ServerResourcesForGroupVersion(gv)
		// Recheck AFTER the call: a cancellation racing the call (e.g. a test
		// reactor canceling mid-request) can still let it return success — a
		// canceled probe must report cancellation, not a result observed under
		// a dead context.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return resources, err
	}
	resources := &metav1.APIResourceList{}
	if err := rc.Get().AbsPath("/apis/" + gv).Do(ctx).Into(resources); err != nil {
		return nil, err
	}
	resources.GroupVersion = gv
	return resources, nil
}
