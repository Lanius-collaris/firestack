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

package tunnel

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/celzero/firestack/intra/log"
	"github.com/celzero/firestack/intra/netstack"
	"github.com/celzero/firestack/intra/settings"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Tunnel represents a session on a TUN device.
type Tunnel interface {
	Mtu() int
	// IsConnected indicates whether the tunnel is in a connected state.
	IsConnected() bool
	// Disconnect disconnects the tunnel.
	Disconnect()
	// Enabled checks if the tunnel is up.
	Enabled() bool
	// Write writes input data to the TUN interface.
	Write(data []byte) (int, error)
	// Close connections
	CloseConns(activecsv string) (closedcsv string)
	// Creates a new link using fd (tun device) and mtu.
	SetLink(fd, mtu int) error
	// Creates the link and updates the routes
	SetLinkAndRoutes(fd, mtu, engine int) error
	// New route
	SetRoute(engine int) error
	// Set or unset the pcap sink
	SetPcap(fpcap string) error
}

type gtunnel struct {
	stack  *stack.Stack              // a tcpip stack
	ep     netstack.SeamlessEndpoint // endpoint for the stack
	hdl    netstack.GConnHandler     // tcp, udp, and icmp handlers
	mtu    int                       // mtu of the tun device
	pcapio *pcapsink                 // pcap output, if any
	closed atomic.Bool               // open/close?
	once   *sync.Once
}

type pcapsink struct {
	sync.RWMutex // protects sink
	sink         io.WriteCloser
}

var (
	errStackMissing = errors.New("tun: netstack not initialized")
	errInvalidTunFd = errors.New("invalid tun fd")
	errNoWriter     = errors.New("no write() on netstack")
)

func (p *pcapsink) Write(b []byte) (int, error) {
	go p.writeAsync(b)
	return len(b), nil
}

func (p *pcapsink) writeAsync(b []byte) {
	p.RLock()
	w := p.sink
	p.RUnlock()

	if w != nil {
		w.Write(b)
	} // else: no op
}

func (p *pcapsink) Close() error {
	p.log(false)       // detach
	err := p.file(nil) // detach
	return err
}

func (p *pcapsink) file(f io.WriteCloser) (err error) {
	p.Lock()
	w := p.sink
	p.sink = f
	p.Unlock()

	if w != nil {
		err = w.Close()
	}
	y := f != nil
	netstack.FilePcap(y)
	return
}

func (p *pcapsink) log(y bool) bool {
	return netstack.LogPcap(y)
}

func (t *gtunnel) Mtu() int {
	// return int(t.stack.NICInfo()[0].MTU)
	return t.mtu
}

func (t *gtunnel) Disconnect() {
	t.once.Do(func() {
		s := t.stack
		p := t.pcapio
		hdl := t.hdl

		err0 := hdl.Close()
		err1 := p.Close()
		s.Destroy()
		t.closed.Store(true)
		log.I("tun: netstack closed; errs: %v / %v", err0, err1)
	})
}

func (t *gtunnel) Enabled() bool {
	s := t.stack

	// nic may be down even if tunnel is up, when SetLink is in between
	// removing existing nic and creating a new one.
	return s != nil && s.CheckNIC(settings.NICID)
}

func (t *gtunnel) IsConnected() bool {
	return !t.closed.Load()
}

func (t *gtunnel) Write([]byte) (int, error) {
	// May be: t.endpoint.WritePackets()
	return 0, errNoWriter
}

func NewGTunnel(fd, mtu int, tcph netstack.GTCPConnHandler, udph netstack.GUDPConnHandler, icmph netstack.GICMPHandler) (t Tunnel, err error) {
	dupfd, err := dup(fd) // tunnel will own dupfd
	if err != nil {
		return nil, err
	}

	sink := new(pcapsink)
	hdl := netstack.NewGConnHandler(tcph, udph, icmph)
	stack := netstack.NewNetstack() // always dual-stack
	// NewEndpoint takes ownership of dupfd; closes it on errors
	ep, err := netstack.NewEndpoint(dupfd, mtu, sink)
	if err != nil {
		return nil, err
	}
	netstack.Route(stack, settings.IP46) // always dual-stack

	t = &gtunnel{stack, ep, hdl, mtu, sink, atomic.Bool{}, new(sync.Once)}

	// Enabled() may temporarily return false when Up() is in progress.
	if err = netstack.Up(stack, ep, hdl); err != nil { // attach new endpoint
		return nil, err
	}

	log.I("tun: new netstack up; fd(%d), mtu(%d)", fd, mtu)
	return
}

func (t *gtunnel) CloseConns(activecsv string) (closedcsv string) {
	return t.hdl.CloseConns(activecsv)
}

func (t *gtunnel) SetPcap(fpcap string) error {
	pcap := t.pcapio

	ignored := pcap.Close() // close any existing pcap sink

	if len(fpcap) == 0 {
		log.I("netstack: pcap closed (ignored-err? %v)", ignored)
		return nil // nothing else to do; pcap is closed
	} else if len(fpcap) == 1 {
		// if fdpcap is 0, 1, or 2 then pcap is written to stdout
		ok := pcap.log(true)
		log.I("netstack: pcap(%s)/log(%t)", fpcap, ok)
		return nil // fdbased will write to stdout
	} else if fout, err := os.OpenFile(fpcap, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600); err == nil {
		ignored = pcap.file(fout) // attach
		log.I("netstack: pcap(%s)/file(%v) (ignored-err? %v)", fpcap, fout, ignored)
		return nil // sniffer will write to fout
	} else {
		log.E("netstack: pcap(%s); (err? %v)", fpcap, err)
		return err // no pcap
	}
}

func (t *gtunnel) SetLinkAndRoutes(fd, mtu, engine int) (err error) {
	// route is always dual-stack (settings.IP46); never changed
	log.I("tun: requested route (%s); unchanged", settings.L3(engine))
	return t.SetLink(fd, mtu)
}

func (t *gtunnel) SetLink(fd, mtu int) error {
	dupfd, err := dup(fd) // tunnel will own dupfd
	if err != nil {
		return err
	}

	t.ep.Swap(dupfd, mtu) // swap fd and mtu
	t.mtu = mtu

	log.I("tun: new link; fd(%d), mtu(%d)", dupfd, mtu)
	return nil
}

func (t *gtunnel) SetRoute(engine int) error {
	// netstack route is never changed; always dual-stack
	netstack.Route(t.stack, settings.IP46)
	log.I("tun: new route; (no-op) got %s but set %s", settings.L3(engine), settings.IP46)
	return nil
}

func dup(fd int) (int, error) {
	if fd < 0 {
		return -1, errInvalidTunFd
	}

	// copy so golang gc may not close orig fd
	newfd, err := unix.Dup(fd)
	if err != nil {
		return -1, err
	}

	// kt-land gives up its ownership of fd
	return newfd, nil
}
