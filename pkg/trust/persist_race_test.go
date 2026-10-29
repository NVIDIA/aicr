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

package trust

import (
	stderrors "errors"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/tuf"
)

// fakeTUF replaces the TUF operations for one test: each call to New or
// Refresh pops the next result, and New records whether the cache was on.
type fakeTUF struct {
	news, refreshes []error
	cached          []bool
}

func (f *fakeTUF) install(t *testing.T) {
	t.Helper()
	savedNew, savedRefresh := tufNew, tufRefresh
	t.Cleanup(func() { tufNew, tufRefresh = savedNew, savedRefresh })
	tufNew = func(opts *tuf.Options) (*tuf.Client, error) {
		f.cached = append(f.cached, !opts.DisableLocalCache)
		err := f.news[0]
		f.news = f.news[1:]
		if err != nil {
			return nil, err
		}
		return &tuf.Client{}, nil
	}
	tufRefresh = func(*tuf.Client) error {
		err := f.refreshes[0]
		f.refreshes = f.refreshes[1:]
		return err
	}
}

// Losing the persist race on the shared cache, as concurrent aicr processes
// refreshing it do, falls back once to a client that reads the metadata into
// memory; any other failure is returned as is, and nothing retries twice.
func TestNewClientFallsBackOnPersistRace(t *testing.T) {
	race := stderrors.New("failed to load metadata: tuf refresh failed: failed to persist metadata")
	other := stderrors.New("failed to load metadata: tuf refresh failed: invalid signature")
	tests := []struct {
		name       string
		news       []error
		wantCached []bool
		wantErr    error
	}{
		{"succeeds on the cache", []error{nil}, []bool{true}, nil},
		{"persist race falls back to memory", []error{race, nil}, []bool{true, false}, nil},
		{"persist race in memory too is returned", []error{race, race}, []bool{true, false}, race},
		{"another failure is not retried", []error{other}, []bool{true}, other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeTUF{news: tt.news}
			f.install(t)
			client, err := newClient(tuf.DefaultOptions())
			if !stderrors.Is(err, tt.wantErr) || (err == nil) != (client != nil) || len(f.cached) != len(tt.wantCached) {
				t.Fatalf("newClient() = %v, %v after %d calls; want %v after %d", client, err, len(f.cached), tt.wantErr, len(tt.wantCached))
			}
			for i, want := range tt.wantCached {
				if f.cached[i] != want {
					t.Errorf("call %d used the cache %t, want %t", i, f.cached[i], want)
				}
			}
		})
	}
}

// A refresh that loses the persist race starts over once in memory, so an
// explicit update or a cold signing-config fetch still succeeds.
func TestRefreshFallsBackOnPersistRace(t *testing.T) {
	race := stderrors.New("failed to persist metadata")
	other := stderrors.New("failed to download timestamp")
	tests := []struct {
		name       string
		news       []error
		refreshes  []error
		wantCached []bool
		wantErr    error
	}{
		{"refresh succeeds", nil, []error{nil}, nil, nil},
		{"persist race refreshes in memory", []error{nil}, []error{race, nil}, []bool{false}, nil},
		{"another failure is not retried", nil, []error{other}, nil, other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeTUF{news: tt.news, refreshes: tt.refreshes}
			f.install(t)
			client, err := refresh(&tuf.Client{}, tuf.DefaultOptions())
			if !stderrors.Is(err, tt.wantErr) || client == nil && err == nil || len(f.refreshes) != 0 || len(f.cached) != len(tt.wantCached) {
				t.Fatalf("refresh() = %v, %v; %d refreshes left, %d New calls", client, err, len(f.refreshes), len(f.cached))
			}
			for i, want := range tt.wantCached {
				if f.cached[i] != want {
					t.Errorf("New call %d used the cache %t, want %t", i, f.cached[i], want)
				}
			}
		})
	}
}
