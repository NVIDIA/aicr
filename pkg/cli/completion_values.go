// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package cli

import (
	"strings"

	"github.com/urfave/cli/v3"
)

// CompletableFlag is implemented by flags that offer value completions.
// When the user presses TAB after typing a flag name (e.g. "--intent "),
// the shell completion handler calls Completions() to get valid values.
type CompletableFlag interface {
	cli.Flag
	Completions() []string
}

// completableStringFlag wraps a cli.StringFlag with a completion function.
type completableStringFlag struct {
	*cli.StringFlag
	completions func() []string
}

// Completions returns the valid values for this flag.
func (f *completableStringFlag) Completions() []string {
	return f.completions()
}

// withCompletions wraps a StringFlag with a function that returns valid values.
// The completion function is called at completion time, so it always returns
// current values from the source of truth.
func withCompletions(flag *cli.StringFlag, completions func() []string) cli.Flag {
	return &completableStringFlag{StringFlag: flag, completions: completions}
}

// findCompletableFlag looks up a CompletableFlag by flag name (with or without
// leading dashes) on the given command. Returns the flag and true if found.
func findCompletableFlag(cmd *cli.Command, name string) (CompletableFlag, bool) {
	name = strings.TrimLeft(name, "-")
	if name == "" {
		return nil, false
	}
	for _, f := range cmd.Flags {
		cf, ok := f.(CompletableFlag)
		if !ok {
			continue
		}
		for _, n := range f.Names() {
			if n == name {
				return cf, true
			}
		}
	}
	return nil, false
}
