// Copyright (c) 2023 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package multihost

import (
	"errors"
	"net"
	"net/netip"
	"strings"

	"github.com/celzero/firestack/intra/dialers"
	"github.com/celzero/firestack/intra/log"
)

var errNoIps error = errors.New("multihost: no ips")

// nooplock is a no-op lock.
type nooplock struct{}

// MH is a list of hostnames and/or ip addresses for one endpoint.
type MH struct {
	nooplock // todo: replace with sync.RWMutex
	id       string
	names    []string
	addrs    []netip.Addr
}

func (nooplock) Lock()    {}
func (nooplock) Unlock()  {}
func (nooplock) RLock()   {}
func (nooplock) RUnlock() {}

// New returns a new multihost with the given id.
func New(id string) *MH {
	return &MH{id: id}
}

func (h *MH) String() string {
	return h.id + ":" + strings.Join(h.straddrs(), ",")
}

func (h *MH) straddrs() []string {
	a := make([]string, 0, len(h.addrs))
	for _, ip := range h.addrs {
		if ip.IsUnspecified() || !ip.IsValid() {
			continue
		}
		a = append(a, ip.String())
	}
	return a
}

func (h *MH) Names() []string {
	return h.names
}

func (h *MH) Addrs() []netip.Addr {
	return h.addrs
}

func (h *MH) AnyAddr() string {
	if len(h.addrs) <= 0 {
		return ""
	}
	return h.addrs[0].String()
}

func (h *MH) Len() int {
	h.RLock()
	defer h.RUnlock()
	// names may exist without addrs and vice versa
	return max(len(h.addrs), len(h.names))
}

func (h *MH) addrlen() int {
	return len(h.addrs)
}

// Refresh re-adds the list of IPs, hostnames, and re-resolves the hostname.
func (h *MH) Refresh() int {
	// resolve ip from domain names
	n := h.With(h.names)
	// re-add existing ips, if any
	return n + h.Add(h.straddrs())
}

// Add appends the list of IPs, hostnames, and hostname's IPs as resolved.
func (h *MH) Add(domainsOrIps []string) int {
	if len(domainsOrIps) <= 0 {
		log.W("multihost: %s no domains or ips", h.id)
		return 0
	}

	h.Lock()
	if h.names == nil {
		h.names = make([]string, 0)
	}
	if h.addrs == nil {
		h.addrs = make([]netip.Addr, 0)
	}
	for _, dip := range domainsOrIps {
		dip = h.normalize(dip) // host or ip or host:port or ip:port
		if len(dip) <= 0 {
			continue
		}
		if ip, err := netip.ParseAddr(dip); err != nil { // may be hostname
			h.names = append(h.names, dip) // add hostname regardless of resolution
			if resolvedips, err := dialers.Resolve(dip); err == nil && len(resolvedips) > 0 {
				h.addrs = append(h.addrs, resolvedips...)
			} else {
				if err == nil { // err may be nil even on zero answers
					err = errNoIps
				}
				log.W("multihost: %s no ips for %q; err? %v", h.id, dip, err)
			}
		} else { // may be ip
			h.addrs = append(h.addrs, ip)
		}
	}
	h.Unlock()

	// TODO: remove dups from h.addrs and h.names

	log.D("multihost: %s with %s => %s", h.id, h.names, h.addrs)
	return h.Len()
}

// With sets the list of IPs, hostnames, and hostname's IPs as resolved.
func (h *MH) With(domainsOrIps []string) int {
	h.Lock()
	h.names = make([]string, 0)
	h.addrs = make([]netip.Addr, 0)
	h.Unlock()
	return h.Add(domainsOrIps)
}

func (h *MH) normalize(dip string) string {
	dip = strings.TrimSpace(dip)
	if hostOrIP, _, err := net.SplitHostPort(dip); err == nil {
		return hostOrIP
	}
	return dip
}

func (h *MH) EqualAddrs(other *MH) bool {
	if (other == nil) || (h.addrlen() != other.addrlen()) {
		return false
	}

	h.RLock()
	defer h.RUnlock()
	for _, me := range h.addrs {
		var ok bool
		for _, them := range other.addrs {
			if me.Compare(them) == 0 {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
