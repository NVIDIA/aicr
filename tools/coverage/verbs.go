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

	"github.com/NVIDIA/aicr/pkg/cli"
	v3 "github.com/urfave/cli/v3"
)

// builtinVerbs are urfave/cli-injected commands that are not AICR capabilities
// and must not appear as coverage rows.
var builtinVerbs = map[string]bool{
	"help":       true,
	"completion": true,
}

// cliVerbs returns the sorted set of user-facing CLI verb paths derived from the
// live command registry (pkg/cli.RootCommand). A pure command *group* — a
// command with subcommands but no Action of its own (e.g. "evidence") — is
// expanded into its leaf subcommands ("evidence digest", ...). A command that
// carries its own Action stays a single verb even if it also nests subcommands.
//
// Deriving from the registry (rather than a hand-maintained list) is the whole
// point: a newly-added verb surfaces as a coverage row automatically instead of
// being silently dropped.
func cliVerbs() []string {
	root := cli.RootCommand()
	verbs := make([]string, 0, len(root.Commands))
	for _, c := range root.Commands {
		verbs = append(verbs, expandVerb("", c)...)
	}
	sort.Strings(verbs)
	return verbs
}

// expandVerb returns the verb path(s) contributed by cmd under the given prefix.
func expandVerb(prefix string, cmd *v3.Command) []string {
	if builtinVerbs[cmd.Name] {
		return nil
	}
	path := cmd.Name
	if prefix != "" {
		path = prefix + " " + cmd.Name
	}

	// A pure group (subcommands, no own Action) contributes only its children.
	if len(cmd.Commands) > 0 && cmd.Action == nil {
		var out []string
		for _, sub := range cmd.Commands {
			out = append(out, expandVerb(path, sub)...)
		}
		return out
	}
	return []string{path}
}
