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
	"sort"
	"strings"
	"testing"
)

func TestCLIVerbsDerivedFromRegistry(t *testing.T) {
	verbs := cliVerbs()
	if len(verbs) == 0 {
		t.Fatal("cliVerbs() returned no verbs; registry walk is broken")
	}

	set := make(map[string]bool, len(verbs))
	for _, v := range verbs {
		set[v] = true
	}

	t.Run("sorted and deduped", func(t *testing.T) {
		if !sort.StringsAreSorted(verbs) {
			t.Errorf("verbs not sorted: %v", verbs)
		}
		if len(set) != len(verbs) {
			t.Errorf("verbs contain duplicates: %v", verbs)
		}
	})

	t.Run("builtins excluded", func(t *testing.T) {
		for _, b := range []string{"help", "completion"} {
			if set[b] {
				t.Errorf("builtin %q must not be a coverage verb", b)
			}
		}
	})

	t.Run("pure groups expand to subcommands", func(t *testing.T) {
		// evidence has no own Action, so it must expand rather than appear bare.
		if set["evidence"] {
			t.Error("bare group verb \"evidence\" should expand to subcommands")
		}
		for _, want := range []string{"evidence digest", "evidence publish", "evidence verify"} {
			if !set[want] {
				t.Errorf("expected expanded subcommand %q in %v", want, verbs)
			}
		}
	})

	t.Run("action commands present", func(t *testing.T) {
		for _, want := range []string{"recipe", "bundle", "validate", "snapshot", "query"} {
			if !set[want] {
				t.Errorf("expected verb %q in %v", want, verbs)
			}
		}
	})

	t.Run("action parent does not hide its subcommands", func(t *testing.T) {
		// `recipe` has its own Action AND nests subcommands; both the parent and
		// the visible subcommands must appear (e.g. `recipe verify-catalog`).
		if !set["recipe"] {
			t.Error("expected executable parent verb \"recipe\"")
		}
		hasRecipeSub := false
		for _, v := range verbs {
			if strings.HasPrefix(v, "recipe ") {
				hasRecipeSub = true
				break
			}
		}
		if !hasRecipeSub {
			t.Errorf("expected at least one `recipe <sub>` verb in %v", verbs)
		}
	})
}
