// Copyright (c) 2020 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This file incorporates work covered by the following copyright and
// permission notice:
//
//     Copyright 2019 The Outline Authors
//
//     Licensed under the Apache License, Version 2.0 (the "License");
//     you may not use this file except in compliance with the License.
//     You may obtain a copy of the License at
//
//          http://www.apache.org/licenses/LICENSE-2.0
//
//     Unless required by applicable law or agreed to in writing, software
//     distributed under the License is distributed on an "AS IS" BASIS,
//     WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//     See the License for the specific language governing permissions and
//     limitations under the License.

package intra

import (
	"errors"
	"sync"

	"github.com/celzero/firestack/intra/dnsx"
	"github.com/celzero/firestack/intra/ipn"
	"github.com/celzero/firestack/intra/log"
	"github.com/celzero/firestack/intra/protect"
	"github.com/celzero/firestack/intra/rnet"
	"github.com/celzero/firestack/intra/settings"
	"github.com/celzero/firestack/intra/x64"
	"github.com/celzero/firestack/tunnel"
)

var errClosed = errors.New("tunnel closed for business")

type Bridge interface {
	Listener
	protect.Controller
}

// Listener receives usage statistics when a UDP or TCP socket is closed,
// or a DNS query is completed.
type Listener interface {
	SocketListener
	dnsx.DNSListener
	rnet.ServerListener
}

// Tunnel represents an Intra session.
type Tunnel interface {
	tunnel.Tunnel
	// Get the resolver.
	GetResolver() dnsx.Resolver
	// Set DNSMode, BlockMode, PtMode, RouteMode.
	SetTunMode(dnsmode, blockmode, ptmode, routemode int)
	// Get proxies.
	GetProxies() ipn.Proxies
	// Sets new default routes for the given engine, where engine is
	// one of the constants (Ns4, Ns6, Ns46) defined in package settings.
	SetRoute(engine int) error
	// Sets pcap output to fpcap which is the absolute filepath
	// to which a PCAP file will be written to.
	// If len(fpcap) is 0, no PCAP file will be written.
	// If len(fpcap) is 1, PCAP be written to stdout.
	SetPcap(fpcap string) error
	// A bridge to the client code.
	getBridge() Bridge
}

type rtunnel struct {
	tunnel.Tunnel
	tunmode  *settings.TunMode
	bridge   Bridge
	proxies  ipn.Proxies
	resolver dnsx.Resolver
	services rnet.Services
	clomu    sync.RWMutex
	closed   bool
}

func NewTunnel(fd, mtu int, fakedns string, tunmode *settings.TunMode, bdg Bridge) (Tunnel, error) {
	l3 := tunmode.L3()

	natpt := x64.NewNatPt(tunmode)
	proxies := ipn.NewProxifier(bdg)
	services := rnet.NewServices(proxies, bdg, bdg)

	resolver := dnsx.NewResolver(fakedns, tunmode, bdg, natpt)
	resolver.Add(newSystemTransport(bdg))            // may be overridden, may be nil
	resolver.Add(newGroundedDefaultTransport())      // may be overridden
	resolver.Add(newBlockAllTransport())             // fixed
	resolver.Add(newDNSCryptTransport(proxies, bdg)) // fixed
	resolver.Add(newMDNSTransport(l3))               // fixed

	addIPMapper(resolver) // namespace aware resolver for pkg split

	tcph := NewTCPHandler(resolver, proxies, tunmode, bdg, bdg)
	udph := NewUDPHandler(resolver, proxies, tunmode, bdg, bdg)
	icmph := NewICMPHandler(resolver, proxies, tunmode, bdg)

	gt, err := tunnel.NewGTunnel(fd, mtu, tunmode.IpMode, tcph, udph, icmph)

	if err != nil {
		log.I("tun: <<< new >>>; err(%v)", err)
		return nil, err
	}

	t := &rtunnel{
		Tunnel:   gt,
		tunmode:  tunmode,
		bridge:   bdg,
		proxies:  proxies,
		resolver: resolver,
		services: services,
	}

	log.I("tun: <<< new >>>; ok")
	resolver.Start()
	return t, nil
}

func (t *rtunnel) getBridge() Bridge {
	return t.bridge // may return nil, esp after Disconnect()
}

func (t *rtunnel) Disconnect() {
	t.clomu.Lock()
	closed := t.closed
	t.closed = true
	t.clomu.Unlock()

	if closed {
		log.W("tun: <<< disconnect >>>; already closed")
		return
	}

	err0 := t.resolver.Stop()
	err1 := t.proxies.StopProxies()
	n := t.services.StopServers()
	t.bridge = nil // "free" ref to the client
	log.I("tun: <<< disconnect >>>; err0(%v); err1(%v); svc(%d)", err0, err1, n)

	t.Tunnel.Disconnect()
}

func (t *rtunnel) SetRoute(engine int) error {
	t.clomu.RLock()
	closed := t.closed
	t.clomu.RUnlock()

	if closed {
		log.W("tun: <<< set route >>>; already closed")
		return errClosed
	}

	t.tunmode.SetMode(t.tunmode.DNSMode, t.tunmode.BlockMode, t.tunmode.PtMode, engine, t.tunmode.RouteMode)
	return t.Tunnel.SetRoute(engine)
}

func (t *rtunnel) GetResolver() dnsx.Resolver {
	return t.resolver
}

func (t *rtunnel) GetProxies() ipn.Proxies {
	return t.proxies
}

func (t *rtunnel) GetServices() rnet.Services {
	return t.services
}

func (t *rtunnel) SetTunMode(dnsmode, blockmode, ptmode, rmode int) {
	t.tunmode.SetMode(dnsmode, blockmode, ptmode, t.tunmode.IpMode, rmode)
}
