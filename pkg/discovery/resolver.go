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

package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/miekg/dns"
	"golang.org/x/sync/errgroup"
)

// maxDiscoverConcurrency bounds the number of parallel DNS queries in DiscoverAll.
const maxDiscoverConcurrency = 10

// Discoverer queries DNS for agent SVCB records per the DNS-AID protocol.
type Discoverer struct {
	dnsClient *dns.Client
	dnsServer string
	timeout   time.Duration
}

// NewDiscoverer creates a Discoverer with functional options.
// If no DNS server is specified, it defaults to the system resolver at 127.0.0.1:53.
func NewDiscoverer(opts ...DiscovererOption) *Discoverer {
	d := &Discoverer{
		dnsClient: new(dns.Client),
		dnsServer: "127.0.0.1:53",
		timeout:   defaults.DiscoveryDNSTimeout,
	}
	for _, opt := range opts {
		opt(d)
	}
	d.dnsClient.Timeout = d.timeout
	return d
}

// Discover queries a specific agent by name and protocol in the given domain.
// Returns the agent record or an error if the agent is not found.
func (d *Discoverer) Discover(ctx context.Context, name string, protocol Protocol, domain string) (*AgentRecord, error) {
	fqdn := FQDN(name, protocol, domain)

	msg, err := d.querySVCB(ctx, fqdn)
	if err != nil {
		return nil, err
	}

	if msg.Rcode == dns.RcodeNameError {
		return nil, errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("agent %q not found at %s", name, fqdn))
	}

	record := ParseSVCBAnswer(msg, name, protocol, domain)
	if record == nil {
		return nil, errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("no SVCB records for agent %q at %s", name, fqdn))
	}

	slog.Debug("discovered agent",
		slog.String("name", name),
		slog.String("protocol", string(protocol)),
		slog.String("endpoint", record.Endpoint),
	)

	return record, nil
}

// DiscoverAll queries the index record and resolves all agents in a domain concurrently.
// Individual agent resolution failures are logged and skipped.
func (d *Discoverer) DiscoverAll(ctx context.Context, domain string) ([]AgentRecord, error) {
	entries, err := d.ListIndex(ctx, domain)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to list agent index", err)
	}

	if len(entries) == 0 {
		return nil, nil
	}

	var (
		mu      sync.Mutex
		records []AgentRecord
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxDiscoverConcurrency)

	for _, entry := range entries {
		entry := entry // capture loop variable
		g.Go(func() error {
			record, err := d.Discover(gctx, entry.Name, entry.Protocol, domain)
			if err != nil {
				slog.Warn("failed to resolve agent from index",
					slog.String("name", entry.Name),
					slog.String("protocol", string(entry.Protocol)),
					slog.String("error", err.Error()),
				)
				return nil // non-fatal: skip unresolvable agents
			}
			mu.Lock()
			records = append(records, *record)
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "agent discovery failed", err)
	}

	return records, nil
}

// ListIndex queries the _index._agents.{domain} TXT record and returns
// the list of registered agents.
func (d *Discoverer) ListIndex(ctx context.Context, domain string) ([]IndexEntry, error) {
	fqdn := IndexFQDN(domain)

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(fqdn), dns.TypeTXT)

	resp, _, err := d.dnsClient.ExchangeContext(ctx, msg, d.dnsServer)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("DNS query failed for %s", fqdn), err)
	}

	if resp.Rcode == dns.RcodeNameError {
		return nil, nil // No index = no agents
	}

	var entries []IndexEntry
	for _, rr := range resp.Answer {
		txt, ok := rr.(*dns.TXT)
		if !ok {
			continue
		}
		// Index format: each TXT string is "name:protocol"
		for _, s := range txt.Txt {
			parts := strings.SplitN(s, ":", 2)
			if len(parts) != 2 {
				slog.Debug("skipping malformed index entry", slog.String("entry", s))
				continue
			}
			entries = append(entries, IndexEntry{
				Name:     parts[0],
				Protocol: Protocol(parts[1]),
			})
		}
	}

	return entries, nil
}

// querySVCB sends a DNS SVCB query for the given FQDN.
func (d *Discoverer) querySVCB(ctx context.Context, fqdn string) (*dns.Msg, error) {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(fqdn), dns.TypeSVCB)

	resp, _, err := d.dnsClient.ExchangeContext(ctx, msg, d.dnsServer)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("DNS SVCB query failed for %s", fqdn), err)
	}

	return resp, nil
}
