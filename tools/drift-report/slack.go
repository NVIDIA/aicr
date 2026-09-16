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

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// SlackPayload renders the weekly digest as a ready-to-POST incoming-webhook
// body. The JSON is encoded here rather than interpolated in the workflow's
// shell so a chart name containing a quote cannot produce a malformed request.
func SlackPayload(r Report) ([]byte, error) {
	date := r.GeneratedAt
	if i := strings.Index(date, "T"); i > 0 {
		date = date[:i]
	}

	var b strings.Builder
	if r.Summary.Behind == 0 && r.Summary.Unresolved == 0 {
		fmt.Fprintf(&b, "AICR component drift - %s: all %d pins current", date, r.Summary.Tracked)
	} else {
		// Tracked counts pins; Behind and Unresolved count chart rows (the
		// OpenShift twins share a row), so label them distinctly rather than
		// implying all three share a unit.
		fmt.Fprintf(&b, "AICR component drift - %s (%d pins, %d charts behind, %d charts unresolved)",
			date, r.Summary.Tracked, r.Summary.Behind, r.Summary.Unresolved)
		for _, row := range r.Drift {
			fmt.Fprintf(&b, "\n• %s  %s -> %s  %s",
				strings.Join(row.Components, ", "), row.Current, row.Latest, row.UpdateType)
		}
		for _, u := range r.Unresolved {
			fmt.Fprintf(&b, "\n• %s  unresolved: %s", strings.Join(u.Components, ", "), u.Reason)
		}
	}
	if r.RunURL != "" {
		fmt.Fprintf(&b, "\n<%s|report artifact>", r.RunURL)
	}
	if r.Summary.Behind > 0 || r.Summary.Unresolved > 0 {
		fmt.Fprint(&b, "\nReview with /aicr-reviewing-component-drift (Codex: $aicr-reviewing-component-drift)")
	}

	payload, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: b.String()})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "encode Slack payload", err)
	}
	return payload, nil
}
