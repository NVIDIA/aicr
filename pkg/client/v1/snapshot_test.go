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

package aicr_test

import (
	"bytes"
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	aicr "aicr.run/pkg/client/v1"
	aicrerrors "aicr.run/pkg/errors"
	"aicr.run/pkg/fingerprint"
	"aicr.run/pkg/measurement"
	"aicr.run/pkg/serializer"
	"aicr.run/pkg/snapshotter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validSnapshotBody is a snapshot the loader accepts. Tests that are not
// specifically exercising a rejection MUST use it: a document the loader
// rejects for its own reasons (an empty measurements list, say) makes an
// "expected an error" assertion pass without proving anything.
var validSnapshotBody = "kind: Snapshot\napiVersion: " + snapshotter.FullAPIVersion +
	"\nmeasurements:\n  - type: K8s\n"

// writeSnapshot writes a snapshot document to a temp file and returns its path.
func writeSnapshot(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return path
}

// TestLoadSnapshot_RoundTripsThroughTheFacadeShape covers the happy path: a
// well-formed snapshot loads, arrives in the facade shape, and still carries
// the internal measurement data the validate and resolve paths read.
func TestLoadSnapshot_RoundTripsThroughTheFacadeShape(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, validSnapshotBody)

	client := newVerifyClient(t)
	snap, err := client.LoadSnapshot(context.Background(), path, "")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("LoadSnapshot returned a nil Snapshot on a nil error")
	}
	if snap.Kind != "Snapshot" {
		t.Errorf("Kind = %q, want Snapshot", snap.Kind)
	}
	if snap.APIVersion != snapshotter.FullAPIVersion {
		t.Errorf("APIVersion = %q, want %q", snap.APIVersion, snapshotter.FullAPIVersion)
	}
	// Unwrap must yield the loaded document, not a synthesized placeholder:
	// ValidateState and ResolveRecipeFromSnapshot read the measurements
	// through it, so losing them here would fail far from the cause.
	internal := snap.Unwrap()
	if internal == nil {
		t.Fatal("Unwrap returned nil; the loaded snapshot was not retained")
	}
	if len(internal.Measurements) != 1 {
		t.Errorf("Measurements = %d, want 1", len(internal.Measurements))
	}
}

func TestLoadSnapshot_WorkloadImageObservations(t *testing.T) {
	t.Parallel()

	client := newVerifyClient(t)
	requested := "registry.invalid/team/image:tag@sha256:requested"
	runtimeID := "containerd://sha256:observed"
	legacy := measurement.Subtype{Name: "image", Data: map[string]measurement.Reading{
		"image": measurement.Str("tag"),
	}}
	server := measurement.Subtype{Name: "server", Data: map[string]measurement.Reading{
		"version": measurement.Str("v1.28.0"),
	}}
	observations := measurement.Subtype{
		Name: "workload-images",
		Data: map[string]measurement.Reading{"coverage": measurement.Str("complete")},
		Items: []measurement.ItemEntry{{
			Context: map[string]string{
				"namespace": "ns", "pod": "pod", "pod-uid": "uid", "container-type": "regular", "container": "main",
			},
			Data: map[string]measurement.Reading{
				"requested-image":  measurement.Str(requested),
				"runtime-image-id": measurement.Str(runtimeID),
				"status":           measurement.Str("observed"),
			},
		}},
	}

	for _, format := range []serializer.Format{serializer.FormatYAML, serializer.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			load := func(subtypes ...measurement.Subtype) *aicr.Snapshot {
				t.Helper()
				snapshot := snapshotter.NewSnapshot()
				snapshot.Kind, snapshot.APIVersion = "Snapshot", snapshotter.FullAPIVersion
				snapshot.Measurements = append(snapshot.Measurements, &measurement.Measurement{
					Type: measurement.TypeK8s, Subtypes: subtypes,
				})
				var body bytes.Buffer
				require.NoError(t, serializer.NewWriter(format, &body).Serialize(t.Context(), snapshot))
				loaded, err := client.LoadSnapshot(t.Context(), writeSnapshot(t, body.String()), "")
				require.NoError(t, err)
				return loaded
			}
			old := load(server, legacy)
			updated := load(server, legacy, observations)
			oldMeasurements := old.Unwrap().Measurements
			updatedMeasurements := updated.Unwrap().Measurements
			assert.Nil(t, oldMeasurements[0].GetSubtype("workload-images"))
			assert.Equal(t, oldMeasurements[0].GetSubtype("image"), updatedMeasurements[0].GetSubtype("image"))
			assert.Equal(t, fingerprint.FromMeasurements(oldMeasurements), fingerprint.FromMeasurements(updatedMeasurements))
			oldCriteria, err := client.CriteriaFromSnapshot(old)
			require.NoError(t, err)
			updatedCriteria, err := client.CriteriaFromSnapshot(updated)
			require.NoError(t, err)
			assert.Equal(t, oldCriteria, updatedCriteria)
			observed := updatedMeasurements[0].GetSubtype("workload-images")
			require.NotNil(t, observed)
			require.Len(t, observed.Items, 1)
			assert.Equal(t, observations.Items[0].Context, observed.Items[0].Context)
			assert.Equal(t, requested, observed.Items[0].Data["requested-image"].Any())
			assert.Equal(t, runtimeID, observed.Items[0].Data["runtime-image-id"].Any())
		})
	}
}

// TestLoadSnapshot_RawIsEmpty pins the documented contract that Raw is set
// only by CollectSnapshot. A loaded snapshot's durable bytes are the file
// itself, and populating Raw here would invite callers to round-trip a file
// through the parsed type — exactly what Raw exists to discourage.
func TestLoadSnapshot_RawIsEmpty(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, validSnapshotBody)

	client := newVerifyClient(t)
	snap, err := client.LoadSnapshot(context.Background(), path, "")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snap.Raw) != 0 {
		t.Errorf("Raw = %d bytes, want empty for a loaded snapshot", len(snap.Raw))
	}
}

// TestLoadSnapshot_FailsClosedOnNonSnapshot is the important negative case.
// Snapshot deserialization is non-strict, so without the loader's identity
// gate a wrong document decodes into a zero-value Snapshot, derives
// criteria(any), and silently produces a fallback recipe with exit 0. The
// facade must surface that rejection rather than flatten it.
func TestLoadSnapshot_FailsClosedOnNonSnapshot(t *testing.T) {
	t.Parallel()

	client := newVerifyClient(t)

	tests := []struct {
		name string
		body string
	}{
		{
			name: "wrong kind",
			body: "kind: AICRConfig\napiVersion: " + snapshotter.FullAPIVersion + "\nspec: {}\n",
		},
		{
			name: "unsupported apiVersion",
			body: "kind: Snapshot\napiVersion: aicr.nvidia.com/v1alpha1\nmeasurements: []\n",
		},
		{
			// Correctly stamped but carrying nothing to specialize on.
			// Resolving it would emit the generic fallback recipe.
			name: "no usable measurements",
			body: "kind: Snapshot\napiVersion: " + snapshotter.FullAPIVersion + "\nmeasurements: []\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := client.LoadSnapshot(context.Background(), writeSnapshot(t, tt.body), "")
			wantInvalidRequest(t, err)
			if got != nil {
				t.Errorf("snapshot = %+v, want nil alongside the error", got)
			}
		})
	}
}

// TestLoadSnapshot_MissingFileIsAnError confirms an unreadable path is an
// error rather than an empty snapshot.
func TestLoadSnapshot_MissingFileIsAnError(t *testing.T) {
	t.Parallel()

	client := newVerifyClient(t)
	if _, err := client.LoadSnapshot(context.Background(),
		filepath.Join(t.TempDir(), "absent.yaml"), ""); err == nil {
		t.Error("expected an error for a missing snapshot file")
	}
}

// TestLoadSnapshot_Guards covers every rejection that happens before any I/O.
func TestLoadSnapshot_Guards(t *testing.T) {
	t.Parallel()

	client := newVerifyClient(t)
	closed := newClosedClient(t)
	path := writeSnapshot(t, validSnapshotBody)

	tests := []struct {
		name   string
		client *aicr.Client
		ctx    context.Context //nolint:containedctx // table-driven guard inputs
		path   string
	}{
		{name: "nil client", client: nil, ctx: context.Background(), path: path},
		{name: "nil context", client: client, ctx: nil, path: path},
		{name: "empty path", client: client, ctx: context.Background(), path: ""},
		{name: "closed client", client: closed, ctx: context.Background(), path: path},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.client.LoadSnapshot(tt.ctx, tt.path, "")
			wantInvalidRequest(t, err)
		})
	}
}

// TestLoadSnapshot_HonorsContextCancellation proves the caller's context
// reaches the loader rather than being replaced by the facade's own bound.
//
// Two things make this assertion real rather than incidental. The fixture is
// one the loader ACCEPTS, so the only reason to fail is the canceled context —
// an independently-invalid document would satisfy a bare "err != nil" while
// proving nothing. And the assertion is on the cause, not the presence of an
// error, so a future change that fails for some unrelated reason does not
// keep this test green.
func TestLoadSnapshot_HonorsContextCancellation(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, validSnapshotBody)
	client := newVerifyClient(t)

	// Guard the premise: the same fixture must load cleanly under a live
	// context, or the cancellation assertion below means nothing.
	if _, err := client.LoadSnapshot(context.Background(), path, ""); err != nil {
		t.Fatalf("fixture does not load under a live context, so this test cannot isolate cancellation: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.LoadSnapshot(ctx, path, "")
	if err == nil {
		t.Fatal("expected an error for an already-canceled context")
	}
	if !stderrors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want one wrapping context.Canceled", err)
	}
	// Assert the CODE, not just the cause. A deliberate abort must not be
	// coded as a timeout: errors.IsTransient reports true for ErrCodeTimeout
	// and for a bare context.Canceled, so a caller's retry loop would
	// re-enter on a Ctrl-C. Only ErrCodeCanceled stops that.
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeCanceled {
		t.Errorf("error code = %v, want %v", err, aicrerrors.ErrCodeCanceled)
	}
	if aicrerrors.IsTransient(err) {
		t.Error("a canceled load reports as transient; a retry loop would re-enter on an operator abort")
	}
}

// TestLoadSnapshot_HTTPSourceHonorsCancellation extends the abort contract to
// the HTTP(S) source.
//
// LoadSnapshot accepts three source forms, and the classification lives on a
// different code path for each: a local file read, a *url.Error from the HTTP
// transport, and an apierrors classification for cm://. Asserting the contract
// on only one of them leaves it true for whichever source a caller happened to
// test with — so the network source is covered here, and the cm:// classifier
// is covered directly in pkg/serializer (it needs a live API client to reach
// end to end, and the classification is the whole behavior under test).
func TestLoadSnapshot_HTTPSourceHonorsCancellation(t *testing.T) {
	t.Parallel()

	// Serve a valid snapshot on one path and block forever on another, so the
	// same server proves both that the HTTP source works and that canceling
	// it aborts rather than fails transiently.
	released := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validSnapshotBody))
	})
	mux.HandleFunc("/blocked.yaml", func(_ http.ResponseWriter, _ *http.Request) {
		<-released
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		close(released)
		srv.Close()
	})

	client := newVerifyClient(t)

	// Guard the premise: the HTTP source must work at all, or the abort
	// assertion below proves nothing.
	snap, err := client.LoadSnapshot(context.Background(), srv.URL+"/snapshot.yaml", "")
	if err != nil {
		t.Fatalf("HTTP source does not load, so this test cannot isolate cancellation: %v", err)
	}
	if snap.Kind != "Snapshot" {
		t.Fatalf("Kind = %q, want Snapshot", snap.Kind)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	t.Cleanup(cancel)

	_, err = client.LoadSnapshot(ctx, srv.URL+"/blocked.yaml", "")
	if err == nil {
		t.Fatal("expected an error for an already-canceled context")
	}
	if !stderrors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want one wrapping context.Canceled", err)
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeCanceled {
		t.Errorf("error code = %v, want %v", err, aicrerrors.ErrCodeCanceled)
	}
	if aicrerrors.IsTransient(err) {
		t.Error("a canceled HTTP load reports as transient; a retry loop would re-enter on an operator abort")
	}
}
