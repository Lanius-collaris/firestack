// Copyright (c) 2022 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
package netstack

import (
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/celzero/firestack/intra/core"
	"github.com/celzero/firestack/intra/log"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// ref: github.com/tailscale/tailscale/blob/cfb5bd0559/wgengine/netstack/netstack.go#L236-L237
const rcvwnd = 0

const maxInFlight = 512 // arbitrary

type GTCPConnHandler interface {
	// Proxy copies data between src and dst.
	Proxy(conn *GTCPConn, src, dst netip.AddrPort) bool
	// Error notes the error in connecting src to dst; retrying if necessary.
	Error(conn *GTCPConn, src, dst netip.AddrPort, err error)
	// CloseConns closes conns by cids, or all conns if cids is empty.
	CloseConns([]string) []string
	// End closes all conns and releases resources.
	End() error
}

var _ core.TCPConn = (*GTCPConn)(nil)

type GTCPConn struct {
	c    *core.Volatile[*gonet.TCPConn] // conn exposes TCP semantics atop endpoint
	ep   *core.Volatile[tcpip.Endpoint] // endpoint is netstack's io interface
	src  netip.AddrPort                 // local addr (remote addr in netstack)
	dst  netip.AddrPort                 // remote addr (local addr in netstack)
	req  *tcp.ForwarderRequest          // egress request as a TCP state machine
	once sync.Once
}

func setupTcpHandler(s *stack.Stack, h GTCPConnHandler) {
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder(s, h).HandlePacket)
}

// nic.deliverNetworkPacket -> no existing matching endpoints -> tcpForwarder.HandlePacket
// ref: github.com/google/gvisor/blob/e89e736f1/pkg/tcpip/adapters/gonet/gonet_test.go#L189
func tcpForwarder(s *stack.Stack, h GTCPConnHandler) *tcp.Forwarder {
	return tcp.NewForwarder(s, rcvwnd, maxInFlight, func(req *tcp.ForwarderRequest) {
		if req == nil {
			log.E("ns: tcp: forwarder: nil request")
			return
		}
		id := req.ID()
		// src 10.111.222.1:38312
		src := remoteAddrPort(id)
		// dst 213.188.195.179:80
		dst := localAddrPort(id)

		// read/writes are routed using 5-tuple to the same conn (endpoint)
		// demuxer.handlePacket -> find matching endpoint -> queue-packet -> send/recv conn (ep)
		// ref: github.com/google/gvisor/blob/be6ffa7/pkg/tcpip/stack/transport_demuxer.go#L180
		gtcp := makeGTCPConn(req, src, dst)
		// setup endpoint right away, so that netstack's internal state is consistent
		if open, err := gtcp.makeEndpoint( /*rst*/ false); err != nil || !open {
			log.E("ns: tcp: forwarder: connect src(%v) => dst(%v); open? %t, err(%v)", src, dst, open, err)
			if err == nil {
				err = errMissingEp
			}
			go h.Error(gtcp, src, dst, err) // error
			return
		}

		// must always handle it in a separate goroutine as it may block netstack
		// see: netstack/dispatcher.go:newReadvDispatcher
		go h.Proxy(gtcp, src, dst)
	})
}

func makeGTCPConn(req *tcp.ForwarderRequest, src, dst netip.AddrPort) *GTCPConn {
	// set sock-opts? github.com/xjasonlyu/tun2socks/blob/31468620e/core/tcp.go#L82
	return &GTCPConn{
		c:   core.NewZeroVolatile[*gonet.TCPConn](),
		ep:  core.NewZeroVolatile[tcpip.Endpoint](),
		src: src,
		dst: dst,
		req: req,
	}
}

func (g *GTCPConn) ok() bool {
	return g.conn() != nil
}

func (g *GTCPConn) conn() *gonet.TCPConn {
	return g.c.Load()
}

func (g *GTCPConn) endpoint() tcpip.Endpoint {
	return g.ep.Load()
}

func (g *GTCPConn) StatefulTeardown() (rst bool) {
	_, _ = g.synack(true) // establish circuit
	_ = g.Close()         // g.TCPConn.Close error always nil
	return true           // always rst
}

func (g *GTCPConn) Redo(rst bool) (open bool, err error) {
	if rst {
		g.complete(rst)
		return false, nil // closed
	}

	rst, err = g.synack(true)

	log.VV("ns: tcp: forwarder: redo src(%v) => dst(%v); fin? %t", g.LocalAddr(), g.RemoteAddr(), rst)
	return !rst, err
}

func (g *GTCPConn) makeEndpoint(rst bool) (open bool, err error) {
	if rst {
		g.complete(rst)
		return false, nil // closed
	}

	rst, err = g.synack(false)

	log.VV("ns: tcp: forwarder: proxy src(%v) => dst(%v); fin? %t", g.LocalAddr(), g.RemoteAddr(), rst)
	return !rst, err // open or closed
}

func (g *GTCPConn) complete(rst bool) {
	g.once.Do(func() {
		log.D("ns: tcp: forwarder: complete src(%v) => dst(%v); rst? %t", g.LocalAddr(), g.RemoteAddr(), rst)
		g.req.Complete(rst)
	})
}

func (g *GTCPConn) synack(complete bool) (rst bool, err error) {
	if g.ok() { // already setup
		return false, nil // open, err free
	}

	defer func() {
		if complete { // complete the request
			g.complete(rst)
		}
	}()

	wq := new(waiter.Queue)
	// the passive-handshake (SYN) may not successful for a non-existent route (say, ipv6)
	if ep, err := g.req.CreateEndpoint(wq); err != nil {
		log.E("ns: tcp: forwarder: synack src(%v) => dst(%v); err(%v)", g.LocalAddr(), g.RemoteAddr(), err)
		// prevent potential half-open TCP connection leak.
		// hopefully doesn't break happy-eyeballs datatracker.ietf.org/doc/html/rfc8305#section-5
		// ie, apps that expect network-unreachable ICMP msgs instead of TCP RSTs?
		// TCP RST here is indistinguishable to an app from being firewalled.
		return true, e(err) // close, err
	} else {
		g.ep.Store(ep)
		g.c.Store(gonet.NewTCPConn(wq, ep))
		return false, nil // open, err free
	}
}

// gonet conn local and remote addresses may be nil
// ref: github.com/tailscale/tailscale/blob/8c5c87be2/wgengine/netstack/netstack.go#L768-L775
// and: github.com/google/gvisor/blob/ffabadf0/pkg/tcpip/transport/tcp/endpoint.go#L2759
func (g *GTCPConn) LocalAddr() net.Addr {
	if c := g.conn(); c == nil {
		return net.TCPAddrFromAddrPort(g.src)
	} else { // client local addr is remote to the gonet adapter
		if addr := c.RemoteAddr(); addr != nil {
			return addr
		}
	}
	return net.TCPAddrFromAddrPort(g.src)
}

func (g *GTCPConn) RemoteAddr() net.Addr {
	if c := g.conn(); c == nil {
		return net.TCPAddrFromAddrPort(g.dst)
	} else { // client remote addr is local to the gonet adapter
		if addr := c.LocalAddr(); addr != nil {
			return addr
		}
	}
	return net.TCPAddrFromAddrPort(g.dst)
}

func (g *GTCPConn) Write(data []byte) (int, error) {
	if c := g.conn(); c == nil {
		return 0, netError(g, "tcp", "write", io.ErrClosedPipe)
	} else {
		return c.Write(data)
	}
}

func (g *GTCPConn) Read(data []byte) (int, error) {
	if c := g.conn(); c == nil {
		return 0, netError(g, "tcp", "read", io.ErrNoProgress)
	} else {
		return c.Read(data)
	}
}

func (g *GTCPConn) CloseWrite() error {
	if c := g.conn(); c == nil {
		return netError(g, "tcp", "close", net.ErrClosed)
	} else {
		return c.CloseWrite()
	}
}

func (g *GTCPConn) CloseRead() error {
	if c := g.conn(); c == nil {
		return netError(g, "tcp", "close", net.ErrClosed)
	} else {
		return c.CloseRead()
	}
}

func (g *GTCPConn) SetDeadline(t time.Time) error {
	if c := g.conn(); c != nil {
		return c.SetDeadline(t)
	} else {
		return nil // no-op to confirm with netstack's gonet impl
	}
}

func (g *GTCPConn) SetReadDeadline(t time.Time) error {
	if c := g.conn(); c != nil {
		return c.SetReadDeadline(t)
	}
	return nil // no-op to confirm with netstack's gonet impl
}

func (g *GTCPConn) SetWriteDeadline(t time.Time) error {
	if c := g.conn(); c != nil {
		return c.SetWriteDeadline(t)
	}
	return nil // no-op to confirm with netstack's gonet impl
}

// Abort aborts the connection by sending a RST segment.
func (g *GTCPConn) Abort() {
	if ep := g.endpoint(); ep != nil {
		ep.Abort()
	}
	core.Close(g.conn())
}

func (g *GTCPConn) Close() error {
	g.Abort()
	// g.conn.Close always returns nil; see gonet.TCPConn.Close
	return nil
}

// from: netstack gonet
func netError(c net.Conn, proto, op string, err error) *net.OpError {
	return &net.OpError{
		Op:     op,
		Net:    proto,
		Source: c.LocalAddr(),
		Addr:   c.RemoteAddr(),
		Err:    err,
	}
}
