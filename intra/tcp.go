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

// Derived from go-tun2socks's "direct" handler under the Apache 2.0 license.

package intra

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/celzero/firestack/intra/dnsx"
	"github.com/celzero/firestack/intra/log"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"

	"github.com/celzero/firestack/intra/core"
	"github.com/celzero/firestack/intra/ipn"
	"github.com/celzero/firestack/intra/netstack"
	"github.com/celzero/firestack/intra/protect"
	"github.com/celzero/firestack/intra/settings"
	"github.com/celzero/firestack/intra/split"
)

const (
	blocktime = 25 * time.Second
)

const (
	TCPOK = iota
	TCPEND
)

var (
	errTcpFirewalled = errors.New("tcp: firewalled")
	errTcpSetupConn  = errors.New("tcp: could not create conn")
	errTcpHandshake  = errors.New("tcp: handshake failed")
)

// TCPHandler is a core TCP handler that also supports DOH and splitting control.
type TCPHandler interface {
	netstack.GTCPConnHandler
}

type tcpHandler struct {
	TCPHandler
	resolver  dnsx.Resolver
	dialer    *net.Dialer
	tunMode   *settings.TunMode
	listener  SocketListener
	prox      ipn.Proxies
	fwtracker *core.ExpMap
	status    int
}

// NewTCPHandler returns a TCP forwarder with Intra-style behavior.
// Connections to `fakedns` are redirected to DOH.
// All other traffic is forwarded using `dialer`.
// `listener` is provided with a summary of each socket when it is closed.
func NewTCPHandler(resolver dnsx.Resolver, prox ipn.Proxies, tunMode *settings.TunMode, ctl protect.Controller, listener SocketListener) TCPHandler {
	d := protect.MakeNsDialer("tcph", ctl)
	h := &tcpHandler{
		resolver:  resolver,
		dialer:    d,
		tunMode:   tunMode,
		listener:  listener,
		prox:      prox,
		fwtracker: core.NewExpiringMap(),
		status:    TCPOK,
	}

	log.I("tcp: new handler created")
	return h
}

type ioinfo struct {
	bytes int64
	err   error
}

// TODO: Propagate TCP RST using local.Abort(), on appropriate errors.
func (h *tcpHandler) handleUpload(local core.TCPConn, remote core.TCPConn, ioch chan<- ioinfo) {
	ci := conn2str(local, remote)

	// io.copy does remote.ReadFrom(local)
	bytes, err := io.Copy(remote, local)
	log.D("tcp: handle-upload(%d) done(%v) b/w %s", bytes, err, ci)

	local.CloseRead()
	remote.CloseWrite()
	ioch <- ioinfo{bytes, err}
}

func conn2str(a net.Conn, b net.Conn) string {
	ar := a.RemoteAddr()
	br := b.RemoteAddr()
	al := a.LocalAddr()
	bl := b.LocalAddr()
	return fmt.Sprintf("a(%v->%v) => b(%v<-%v)", al, ar, bl, br)
}

func (h *tcpHandler) handleDownload(local core.TCPConn, remote core.TCPConn) (bytes int64, err error) {
	ci := conn2str(local, remote)

	bytes, err = io.Copy(local, remote)
	log.D("tcp: handle-download(%d) done(%v) b/w %s", bytes, err, ci)

	local.CloseWrite()
	remote.CloseRead()
	return
}

func (h *tcpHandler) forward(local net.Conn, remote net.Conn, summary *SocketSummary) {
	if h.status == TCPEND {
		log.D("tcp: forward(%v, %v): end", local, remote)
		return
	}

	localtcp := local.(core.TCPConn)   // conforms to net.TCPConn
	remotetcp := remote.(core.TCPConn) // conforms to net.TCPConn
	ioch := make(chan ioinfo)

	go h.handleUpload(localtcp, remotetcp, ioch)
	download, err := h.handleDownload(localtcp, remotetcp)

	ioi := <-ioch

	summary.Rx = download
	summary.Tx = ioi.bytes

	summary.done(err, ioi.err)
	go h.sendNotif(summary)
}

func filteredPort(addr net.Addr) int16 {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return -1
	}
	if port == "80" {
		return 80
	}
	if port == "443" {
		return 443
	}
	if port == "0" {
		return 0
	}
	if port == "53" {
		return 53
	}
	return -1
}

// must always be called from a goroutine
func (h *tcpHandler) sendNotif(summary *SocketSummary) {
	// sleep a bit to avoid scenario where kotlin-land
	// hasn't yet had the chance to persist info about
	// this conn (cid) to meaninfully process its summary
	time.Sleep(1 * time.Second)
	l := h.listener

	ok0 := h.status != TCPEND
	ok1 := l != nil
	ok2 := summary != nil
	ok3 := len(summary.ID) > 0
	log.V("tcp: sendNotif(%t, %t,%t,%t): %s", ok0, ok1, ok2, ok3, summary.str())
	if ok0 && ok1 && ok2 && ok3 {
		l.OnSocketClosed(summary)
	}
}

func (h *tcpHandler) dnsOverride(conn net.Conn, addr *net.TCPAddr) bool {
	// addr with zone information removed; see: netip.ParseAddrPort which h.resolver relies on
	addr2 := &net.TCPAddr{IP: addr.IP, Port: addr.Port}
	if h.resolver.IsDnsAddr(dnsx.NetTypeTCP, addr2.String()) {
		// conn closed by the resolver
		h.resolver.Serve(conn)
		return true
	}
	return false
}

func (h *tcpHandler) onFlow(localaddr *net.TCPAddr, target *net.TCPAddr, realips, domains, blocklists string) *Mark {
	// BlockModeNone returns false, BlockModeSink returns true
	if h.tunMode.BlockMode == settings.BlockModeSink {
		return optionsBlock
	} else if h.tunMode.BlockMode == settings.BlockModeNone {
		// todo: block-mode none should call into listener.Flow to determine upstream proxy
		return optionsBase
	}

	if len(realips) <= 0 || len(domains) <= 0 {
		log.D("onFlow: no realips(%s) or domains(%s), for src=%s dst=%s", realips, domains, localaddr, target)
	}

	// Implict: BlockModeFilter or BlockModeFilterProc
	uid := -1
	if h.tunMode.BlockMode == settings.BlockModeFilterProc {
		procEntry := settings.FindProcNetEntry("tcp", localaddr.IP, localaddr.Port, target.IP, target.Port)
		if procEntry != nil {
			uid = procEntry.UserID
		}
	}

	var proto int32 = 6 // tcp
	src := localaddr.String()
	dst := target.String()
	res := h.listener.Flow(proto, uid, src, dst, realips, domains, blocklists)

	if len(res.PID) <= 0 {
		log.W("tcp: empty flow from kt; using base")
		res.PID = ipn.Base
	}

	return res
}

func (h *tcpHandler) End() error {
	h.status = TCPEND
	return nil
}

// Proxy implements netstack.GTCPConnHandler
func (h *tcpHandler) Proxy(gconn *netstack.GTCPConn, src, target *net.TCPAddr) (open bool) {
	if h.status == TCPEND {
		log.D("tcp: proxy: end")
		return
	}

	const rst bool = true // tear down conn
	const ack bool = !rst // send synack
	var err error

	if src == nil || target == nil {
		log.E("tcp: nil addr %v -> %v", src, target)
		open = gconn.Connect(rst) // fin
		return
	}

	// alg happens after nat64, and so, alg knows nat-ed ips
	// ipx4 is un-nated (but same as target.IP when no nat64 is involved)
	realips, domains, blocklists := undoAlg(h.resolver, target.IP)
	ipx4 := maybeUndoNat64(h.resolver, realips, target.IP)

	// flow/dns-override are nat-aware, as in, they can deal with
	// nat-ed ips just fine, and so, use target as-is instead of ipx4
	res := h.onFlow(src, target, realips, domains, blocklists)

	pid, cid, uid := splitPidCidUid(res)
	s := tcpSummary(cid, pid, uid)

	defer func() {
		if !open {
			s.done(err)
			go h.sendNotif(s)
		} // else conn has been proxied, sendNotif is called by h.forward()
	}()

	if pid == ipn.Block {
		var secs uint32
		k := uid + target.String()
		if len(domains) > 0 {
			k = uid + domains
		}
		if secs = stall(h.fwtracker, k); secs > 0 {
			waittime := time.Duration(secs) * time.Second
			time.Sleep(waittime)
		}
		log.I("tcp: gconn firewalled from %s -> %s (dom: %s/ real: %s); stall? %ds", src, target, domains, realips, secs)
		open = gconn.Connect(rst) // fin
		err = errTcpFirewalled
		return
	}

	// handshake
	if open = gconn.Connect(ack); !open {
		log.E("tcp: gconn closed; no route %s -> %s", src, target)
		err = errTcpHandshake
		return
	}

	// dialers must connect to un-nated ips; overwrite target.IP with ipx4
	// but ipx4 might itself be an alg ip; so check if there's a real-ip to connect to
	target.IP = oneRealIp(realips, ipx4)

	if err = h.Handle(gconn, target, s); err != nil {
		log.E("tcp: proxy(%s -> %s) err: %v", src, target, err)
		open = false
		gconn.Close()
	}
	return
}

// TODO: Request upstream to make `conn` a `core.TCPConn` so we can avoid a type assertion.
func (h *tcpHandler) Handle(conn net.Conn, target *net.TCPAddr, summary *SocketSummary) (err error) {
	var px ipn.Proxy
	var pc protect.Conn

	pid := summary.PID

	if h.dnsOverride(conn, target) {
		return nil
	}

	if px, err = h.prox.GetProxy(pid); err != nil {
		return err
	}

	start := time.Now()
	var end time.Time
	var c net.Conn

	// ref: stackoverflow.com/questions/63656117
	// ref: stackoverflow.com/questions/40328025
	if pc, err = px.Dial(target.Network(), target.String()); err == nil {
		end = time.Now()
		switch uc := pc.(type) {
		// underlying conn must specifically be a tcp-conn
		case *net.TCPConn:
			c = uc
		case *gonet.TCPConn:
			c = uc
		case core.TCPConn:
			c = uc
		default:
			err = errTcpSetupConn
		}
	}

	if err != nil {
		log.W("tcp: err dialing proxy(%s) to dst(%v): %v", px.ID(), target, err)
		return err
	}

	// split client-hello if server-port is 443
	if px.ID() == ipn.Base {
		if port := filteredPort(target); port == 443 {
			timeout := split.CalcTimeout(start, end)
			tcpconn, ok := c.(*net.TCPConn)
			if !ok {
				log.W("tcp: err spliting; ipn.Base must return a net.TCPConn")
				return errTcpSetupConn
			}
			c = split.RetryingConn(px.Dialer(), target, timeout, tcpconn)
		}
	}

	summary.Rtt = int32(end.Sub(start).Seconds() * 1000)

	go h.forward(conn, c, summary)

	log.I("tcp: new conn via proxy(%s); src(%s) -> dst(%s)", px.ID(), conn.LocalAddr(), target)
	return nil
}

// TODO: move this to ipn.Ground
func stall(m *core.ExpMap, k string) (secs uint32) {
	if n := m.Get(k); n <= 0 {
		secs = 0 // no stall
	} else if n > 30 {
		secs = 30 // max up to 30s
	} else if n < 5 {
		secs = (rand.Uint32() % 5) + 1 // up to 5s
	} else {
		secs = n
	}
	// track uid->target for n secs, or 30s if n is 0
	life30s := ((29 + secs) % 30) + 1
	newlife := time.Duration(life30s) * time.Second
	m.Set(k, newlife)
	return
}

func maybeUndoNat64(pt dnsx.NAT64, ipscsv string, defaultip net.IP) net.IP {
	maybe64 := make([]netip.Addr, 0, 0)
	if ips := strings.Split(ipscsv, ","); len(ips) > 0 {
		for _, v := range ips {
			// len may be zero when ipscsv is "," or ""
			if len(v) > 0 {
				ip, err := netip.ParseAddr(v)
				ip = ip.Unmap()
				if err == nil && ip.Is6() && !ip.IsUnspecified() {
					maybe64 = append(maybe64, ip)
				}
			}
		}
	}
	for _, nip := range maybe64 {
		ip := nip.AsSlice()
		// TODO: need the actual ID of the transport that did nat64
		if pt.IsNat64(dnsx.Local464Resolver, ip) { // un-nat64, when dns64 done by local464-resolver
			// TODO: check if the network this process binds to has ipv4 connectivity
			ipx4 := pt.X64(dnsx.Local464Resolver, ip) // ipx4 may be nil
			if len(ipx4) < net.IPv4len {              // no nat?
				log.D("dns64: maybeUndoNat64: No local nat64 to ip4(%v) for ip6(%v)", ipx4, nip)
			} else {
				log.I("dns64: maybeUndoNat64: nat64 to ip4(%v) from ip6(%v)", ipx4, nip)
				return ipx4
			}
		} else {
			log.V("dns64: maybeUndoNat64: No local nat64 to for ip(%v)", nip)
		}
	}
	return defaultip
}

func netipFrom(ip net.IP) *netip.Addr {
	if addr, ok := netip.AddrFromSlice(ip); ok {
		addr = addr.Unmap()
		return &addr
	}
	return nil
}

func oneRealIp(realips string, dstip net.IP) net.IP {
	if len(realips) <= 0 {
		return dstip
	}
	// override alg-ip with the first real-ip
	if ips := strings.Split(realips, ","); len(ips) > 0 {
		for _, v := range ips {
			// len may be zero when realips is "," or ""
			if len(v) > 0 {
				ip := net.ParseIP(v)
				if !ip.IsUnspecified() {
					return ip
				}
			}
		}
	}
	return dstip
}

func undoAlg(r dnsx.Resolver, algip net.IP) (realips, domains, blocklists string) {
	dstip := netipFrom(algip)
	if gw := r.Gateway(); dstip.IsValid() && gw != nil {
		dst := dstip.AsSlice()
		domains = gw.PTR(dst)
		realips = gw.X(dst)
		blocklists = gw.RDNSBL(dst)
	} else {
		log.D("alg: undoAlg: no gw(%t) or alg-ip(%s)", gw == nil, algip)
	}
	return
}

func splitPidCidUid(decision *Mark) (pid, cid, uid string) {
	if decision == nil {
		return
	}
	return decision.PID, decision.CID, decision.UID
}
