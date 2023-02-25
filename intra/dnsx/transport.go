// Copyright (c) 2022 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package dnsx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/celzero/firestack/intra/ipn"
	"github.com/celzero/firestack/intra/log"
	"github.com/celzero/firestack/intra/settings"
	"github.com/celzero/firestack/intra/xdns"
	"github.com/k-sone/critbitgo"
	"github.com/miekg/dns"
)

const (
	// DNS transport types
	DOH      = "DNS-over-HTTPS"
	DNSCrypt = "DNSCrypt"
	DNS53    = "DNS"

	// special singleton DNS transports (IDs)
	System    = "System"    // network/os provided dns
	Default   = "Default"   // default (fallback) dns
	Preferred = "Preferred" // user preferred dns, primary for alg
	BlockFree = "BlockFree" // no local blocks; if not set, default is used
	BlockAll  = "BlockAll"  // all blocks
	Alg       = "Alg"       // dns application-level gateway
	DcProxy   = "DcProxy"   // dnscrypt.Proxy as a transport

	invalidQname = "invalid.query"
)

const (
	NetTypeUDP = "udp"
	NetTypeTCP = "tcp"
)

var (
	ErrNoDcProxy           = errors.New("no dnscrypt-proxy")
	errNoSuchTransport     = errors.New("missing transport")
	errBlockFreeTransport  = errors.New("block free transport")
	errNoRdns              = errors.New("no rdns")
	errRdnsLocalIncorrect  = errors.New("rdns local is not remote")
	errRdnsRemoteIncorrect = errors.New("rdns remote is not local")
	errTransportNotMult    = errors.New("not a multi-transport")
)

// Transport represents a DNS query transport.  This interface is exported by gobind,
// so it has to be very simple.
type Transport interface {
	// uniquely identifies this transport
	ID() string
	// one of DNS53, DOH, DNSCrypt, System
	Type() string
	// Given a DNS query (including ID), returns a DNS response with matching
	// ID, or an error if no response was received.  The error may be accompanied
	// by a SERVFAIL response if appropriate.
	Query(network string, q []byte, summary *Summary) ([]byte, error)
	// Return the server host address used to initialize this transport.
	GetAddr() string
	// State of the transport after previous query (see: queryerror.go)
	Status() int
}

// TransportMult is a hybrid: transport and a multi-transport.
type TransportMult interface {
	Mult
	Transport
}

type Conn interface {
	Read(b []byte) (n int, err error)
	Write(b []byte) (n int, err error)
	Close() error
}

type Mult interface {
	// Add adds a transport to this multi-transport.
	Add(t Transport) bool
	// Remove removes a transport from this multi-transport.
	Remove(id string) bool
	// Start starts a multi-transport, returns number of live-servers and errors if any.
	Start() (string, error)
	// Stop stops this multi-transport.
	Stop() error
	// Refresh re-registers transports and returns a csv of active ones.
	Refresh() (string, error)
	// LiveTransports returns a csv of active transports.
	LiveTransports() string
}

type Resolver interface {
	Mult
	RdnsResolver

	AddSystemDNS(t Transport) bool
	RemoveSystemDNS() int

	// special purpose pre-defined transports
	// Gateway implements a DNS ALG transport
	Gateway() Gateway
	// DcProxy implements a DNSCrypt multi-transport
	DcProxy() (TransportMult, error)
	// BlockAll implements a DNS transport that blocks all queries
	BlockAll() Transport

	IsDnsAddr(network, ipport string) bool
	Forward(q []byte) ([]byte, error)
	Serve(conn Conn)
}

type resolver struct {
	sync.RWMutex
	Resolver
	tunmode      *settings.TunMode
	tcpaddrs     []*net.TCPAddr
	udpaddrs     []*net.UDPAddr
	systemdns    []Transport
	transports   map[string]Transport
	pool         map[string]*oneTransport
	localdomains *critbitgo.Trie
	rdnsl        BraveDNS
	rdnsr        BraveDNS
	natpt        ipn.DNS64
	listener     Listener
}

func NewResolver(fakeaddrs string, tunmode *settings.TunMode, defaultdns Transport, l Listener, pt ipn.DNS64) Resolver {
	r := &resolver{
		listener:     l,
		natpt:        pt,
		transports:   make(map[string]Transport),
		pool:         make(map[string]*oneTransport),
		tunmode:      tunmode,
		localdomains: UndelegatedDomainsTrie(),
	}
	r.Add(defaultdns)
	r.Add(NewDNSGateway(defaultdns, r))
	r.loadaddrs(fakeaddrs)
	return r
}

type oneTransport struct {
	ipn.Resolver
	t Transport
}

func (r *resolver) Gateway() Gateway {
	// called from Add, so no lock
	if gw, ok := r.transports[Alg]; ok {
		return gw.(Gateway)
	}
	return nil
}

// Implements ipn.Exchange
func (one *oneTransport) Exchange(q []byte) (r []byte, err error) {
	ans1, err1 := one.t.Query(NetTypeUDP, q, &Summary{})
	if err1 != nil {
		return ans1, err1
	}
	// for doh, dns ans is never truncated
	if one.t.Type() == DOH {
		return ans1, err1
	}

	msg1 := &dns.Msg{}
	err1 = msg1.Unpack(ans1)
	if err != nil {
		return ans1, err1
	}
	if !msg1.Truncated {
		return ans1, err1
	}

	// else if: returned response is truncated dns ans, retry over tcp
	return one.t.Query(NetTypeTCP, q, &Summary{})
}

// Implements RdnsResolver
func (r *resolver) SetRdnsLocal(b BraveDNS) error {
	if b == nil {
		r.rdnsl = nil
	} else if b.OnDeviceBlock() {
		r.rdnsl = b
	} else {
		return errRdnsLocalIncorrect
	}
	return nil
}

func (r *resolver) SetRdnsRemote(b BraveDNS) error {
	if b == nil {
		r.rdnsr = nil
	} else if !b.OnDeviceBlock() {
		r.rdnsr = b
	} else {
		return errRdnsRemoteIncorrect
	}
	return nil
}

func (r *resolver) BlockFreeTransport() Transport {
	return r.transports[BlockFree]
}

func (r *resolver) GetRdnsLocal() BraveDNS {
	return r.rdnsl
}

func (r *resolver) GetRdnsRemote() BraveDNS {
	return r.rdnsr
}

func (r *resolver) AddSystemDNS(t Transport) bool {
	defer r.addSystemDnsIfAbsent(t)
	r.Lock()
	r.systemdns = append(r.systemdns, t)
	r.Unlock()
	return true
}

func (r *resolver) RemoveSystemDNS() int {
	defer r.Remove(System)
	r.Lock()
	d := len(r.systemdns)
	r.systemdns = nil
	r.Unlock()

	return d
}

// Implements Resolver
func (r *resolver) Add(t Transport) (ok bool) {

	// these IDs are reserved for internal use
	if isReserved(t.ID()) {
		log.Infof("dns: updating reserved transport %s", t.ID())
	}

	switch t.Type() {
	case DNS53:
		fallthrough
	case DNSCrypt:
		// DNSCrypt transports are also registered with DcProxy
		// remove if present
		r.Remove(t.ID())
		fallthrough
	case DOH:
		r.Lock()
		r.transports[t.ID()] = t
		r.pool[t.ID()] = &oneTransport{t: t}
		// if resetting default transport, update underlying transport for alg
		if gw := r.Gateway(); t.ID() == Preferred && gw != nil {
			gw.WithTransport(t)
		}
		r.Unlock()
		return true
	}
	return false
}

func (r *resolver) DcProxy() (TransportMult, error) {
	if t, ok := r.transports[DcProxy]; ok {
		if tm, ok := t.(TransportMult); ok {
			return tm, nil
		}
		return nil, errTransportNotMult
	}
	return nil, errNoSuchTransport
}

func (r *resolver) BlockAll() Transport {
	if t, ok := r.transports[BlockAll]; ok {
		return t
	}
	return nil
}

func (r *resolver) addSystemDnsIfAbsent(t Transport) (ok bool) {
	r.Lock()
	defer r.Unlock()
	if _, ok = r.transports[t.ID()]; !ok {
		// r.Add before r.registerSystemDns64, since r.pool must be populated
		ok1 := r.Add(t)
		go r.registerSystemDns64(r.pool[t.ID()])
		return ok1
	}
	return false
}

func (r *resolver) registerSystemDns64(ur ipn.Resolver) (ok bool) {
	return r.natpt.AddResolver(ipn.UnderlayResolver, ur)
}

func (r *resolver) Remove(id string) (ok bool) {
	r.Lock()
	defer r.Unlock()

	// these IDs are reserved for internal use
	if isReserved(id) {
		log.Infof("dns: removing reserved transport %s", id)
	}

	t, ok1 := r.transports[id]
	_, ok2 := r.pool[id]
	var ok3 bool
	if tm, err := r.DcProxy(); err == nil {
		ok3 = tm.Remove(id)
	}
	var ok4 bool
	if gw := r.Gateway(); gw != nil {
		ok4 = gw.WithoutTransport(t)
	}
	if ok1 {
		delete(r.transports, id)
	}
	if ok2 {
		delete(r.pool, id)
	}
	return ok1 || ok2 || ok3 || ok4
}

func (r *resolver) IsDnsAddr(network, ipport string) bool {
	if len(ipport) <= 0 {
		return false
	}
	return r.isDns(network, ipport)
}

func (r *resolver) Forward(q []byte) ([]byte, error) {
	starttime := time.Now()
	summary := &Summary{
		QName:  invalidQname,
		Status: Start,
	}
	// always call up to the listener
	defer func() {
		go r.listener.OnResponse(summary)
	}()

	msg, err := unpack(q)
	if err != nil {
		log.Warnf("not a dns packet %v", err)
		summary.Latency = time.Since(starttime).Seconds()
		summary.Status = BadQuery
		return nil, err
	}

	// figure out transport to use
	qname := qname(msg)
	summary.QName = qname
	summary.QType = qtype(msg)
	id := r.requiresSystem(qname)
	if len(id) > 0 {
		log.Infof("transport (udp): suggest system-dns %s for %s", id, qname)
	}
	id = r.listener.OnQuery(qname, id)
	t, onet := r.determineTransports(id)
	if t == nil || onet == nil {
		summary.Latency = time.Since(starttime).Seconds()
		summary.Status = TransportError
		return nil, errNoSuchTransport
	}

	// block skipped if the transport is alg/block-free
	res1, blocklists, err := r.blockQ(t, msg)
	if err == nil {
		b, e := res1.Pack()
		summary.Latency = time.Since(starttime).Seconds()
		summary.Status = Complete
		summary.Blocklists = blocklists
		summary.RData = xdns.GetInterestingRData(res1)
		return b, e
	}

	// query the transport
	summary.Type = t.Type()
	summary.ID = t.ID()
	res2, err := t.Query(NetTypeUDP, q, summary)
	algerr := isAlgErr(err)

	if algerr {
		log.Debugf("transport (udp): alg error %s for %s", err, qname)
	}
	// in the case of an alg transport, if there's no-alg,
	// err is set which should be ignored if res2 is not nil
	if err != nil && algerr {
		// summary latency, ips, response, status already set by transport t
		return res2, err
	}
	ans1, err := unpack(res2)
	if err != nil {
		summary.Status = BadResponse
		return res2, err
	}

	// block response if needed
	ans2, blocklistnames := r.blockA(t, msg, ans1, summary.Blocklists)
	if len(blocklistnames) > 0 {
		// summary latency, response, status, ips already set by transport t
		summary.Blocklists = blocklistnames
	}
	// overwrite response when blocked
	if ans2 != nil {
		ans1 = ans2
	}

	// override resp with dns64 if needed
	if onet != nil {
		d64 := r.natpt.D64(t.ID(), res2, onet)
		if len(d64) >= xdns.MinDNSPacketSize {
			return d64, nil
		}
	} else {
		log.Warnf("dns64: missing onetransport for %s", t.ID())
	}

	return ans1.Pack()
}

func (r *resolver) Serve(x Conn) {
	if c, ok := x.(io.ReadWriteCloser); ok {
		r.accept(c)
	}
}

func (r *resolver) determineTransports(id string) (Transport, *oneTransport) {
	r.RLock()
	defer r.RUnlock()

	if id == Alg {
		return r.transports[Alg], r.pool[Preferred]
	}
	if t, ok := r.transports[id]; ok {
		if onet, ok := r.pool[id]; ok {
			return t, onet
		}
	}

	return nil, nil
}

// Perform a query using the transport, and send the response to the writer.
func (r *resolver) forwardQuery(q []byte, c io.Writer) error {
	starttime := time.Now()
	summary := &Summary{
		QName:  invalidQname,
		Status: Start,
	}
	// always call up to the listener
	defer func() {
		go r.listener.OnResponse(summary)
	}()

	msg, err := unpack(q)
	if err != nil {
		log.Warnf("not a dns packet %v", err)
		summary.Latency = time.Since(starttime).Seconds()
		summary.Status = BadQuery
		return err
	}

	// figure out transport to use
	qname := qname(msg)
	summary.QName = qname
	summary.QType = qtype(msg)
	id := r.requiresSystem(qname)
	if len(id) > 0 {
		log.Infof("transport (udp): suggest system-dns %s for %s", id, qname)
	}
	id = r.listener.OnQuery(qname, id)
	// retrieve transport
	t, onet := r.determineTransports(id)
	if t == nil || onet == nil {
		summary.Latency = time.Since(starttime).Seconds()
		summary.Status = TransportError
		return errNoSuchTransport
	}

	// block query if needed (skipped for alg/block-free)
	res1, blocklists, err := r.blockQ(t, msg)
	if err == nil {
		b, e := res1.Pack()
		summary.Latency = time.Since(starttime).Seconds()
		summary.Status = Complete
		summary.Blocklists = blocklists
		summary.RData = xdns.GetInterestingRData(res1)
		writeto(c, b, len(b))
		return e
	}

	// query the transport
	summary.Type = t.Type()
	summary.ID = t.ID()
	res2, err := t.Query(NetTypeTCP, q, summary)
	algerr := isAlgErr(err)

	if algerr {
		log.Debugf("transport (tcp): alg error %s for %s", err, qname)
	}
	// in the case of an alg transport, if there's no-alg,
	// err is set which should be ignored if res2 is not nil
	if err != nil && !algerr {
		// summary latency, ips, response, status already set by transport t
		return err
	}
	ans1, qerr := unpack(res2)
	if qerr != nil {
		summary.Status = BadResponse
		return qerr
	}

	ans2, blocklistnames := r.blockA(t, msg, ans1, summary.Blocklists)
	// overwrite response when blocked
	if len(blocklistnames) > 0 {
		// summary latency, response, status, ips already set by transport t
		summary.Blocklists = blocklistnames
	}
	// overwrite response when blocked
	if ans2 != nil {
		ans1 = ans2
	}

	resp, qerr := ans1.Pack()
	if qerr != nil {
		summary.Status = BadResponse
		return qerr
	}
	rlen := len(resp)
	if rlen > xdns.MaxDNSPacketSize {
		summary.Status = BadResponse
		return fmt.Errorf("oversize response: %d", rlen)
	}

	// override resp with dns64 if needed
	if onet != nil {
		d64 := r.natpt.D64(t.ID(), res2, onet)
		rlen = len(resp)
		if rlen > xdns.MinDNSPacketSize {
			resp = d64
		}
	} else {
		log.Warnf("dns64: missing onetransport for %s", t.ID())
	}

	n, err := writeto(c, resp, rlen)
	if err != nil {
		summary.Status = InternalError
		return err
	}
	if n != rlen {
		summary.Status = InternalError
		return fmt.Errorf("incomplete response write: %d < %d", n, rlen)
	}
	return qerr
}

// Perform a query using the transport, send the response to the writer,
// and close the writer if there was an error.
func (r *resolver) forwardQueryAndCheck(q []byte, c io.WriteCloser) {
	if err := r.forwardQuery(q, c); err != nil {
		log.Warnf("Query forwarding failed: %v", err)
		c.Close()
	}
}

// Accept a DNS-over-TCP socket from a stub resolver, and connect the socket
// to this DNSTransport.
func (r *resolver) accept(c io.ReadWriteCloser) {
	defer c.Close()

	qlbuf := make([]byte, 2)
	for {
		n, err := c.Read(qlbuf)
		if n == 0 {
			log.Debugf("TCP query socket clean shutdown")
			break
		}
		if err != nil {
			log.Warnf("Error reading from TCP query socket: %v", err)
			break
		}
		// TODO: inform the listener?
		if n < 2 {
			log.Warnf("Incomplete query length")
			break
		}
		qlen := binary.BigEndian.Uint16(qlbuf)
		q := make([]byte, qlen)
		n, err = c.Read(q)
		if err != nil {
			log.Warnf("Error reading query: %v", err)
			break
		}
		if n != int(qlen) {
			log.Warnf("Incomplete query: %d < %d", n, qlen)
			break
		}
		go r.forwardQueryAndCheck(q, c)
	}
	// TODO: Cancel outstanding queries.
}

func isReserved(id string) (ok bool) {
	return id == Alg || id == DcProxy || id == BlockAll
}

func unpack(q []byte) (*dns.Msg, error) {
	msg := &dns.Msg{}
	err := msg.Unpack(q)
	return msg, err
}

func qname(msg *dns.Msg) string {
	n := xdns.QName(msg)
	n, _ = xdns.NormalizeQName(n)
	return n
}

func qtype(msg *dns.Msg) int {
	return int(xdns.QType(msg))
}

func (r *resolver) loadaddrs(csvaddr string) {
	r.fakeTcpAddr(csvaddr)
	r.fakeUdpAddr(csvaddr)
}

func writeto(w io.Writer, b []byte, l int) (int, error) {
	rlbuf := make([]byte, l+2)
	binary.BigEndian.PutUint16(rlbuf, uint16(l))
	copy(rlbuf[2:], b)
	// Use a combined write to ensure atomicity.
	// Otherwise, writes from two responses could be interleaved.
	return w.Write(rlbuf)
}

func (r *resolver) Start() (string, error) {
	if dc, err := r.DcProxy(); err == nil {
		return dc.Start()
	}
	return "", ErrNoDcProxy
}

func (r *resolver) Stop() error {
	if gw := r.Gateway(); gw != nil {
		gw.Stop()
	}
	if dc, err := r.DcProxy(); err == nil {
		return dc.Stop()
	}
	// nothing to stop / no error
	return nil
}

func (r *resolver) Refresh() (string, error) {
	s := map2csv(r.transports)
	if dc, err := r.DcProxy(); err == nil {
		if x, err := dc.Refresh(); err == nil {
			s += "," + x
		}
	}
	return trimcsv(s), nil
}

func (r *resolver) LiveTransports() string {
	s := map2csv(r.transports)
	if dc, err := r.DcProxy(); err == nil {
		x := dc.LiveTransports()
		if len(x) > 0 {
			s += x
		}
	}
	return trimcsv(s)
}

func map2csv(ts map[string]Transport) string {
	s := ""
	for _, t := range ts {
		s += t.ID() + ","
	}
	return trimcsv(s)
}

func trimcsv(s string) string {
	return strings.Trim(s, ",")
}
