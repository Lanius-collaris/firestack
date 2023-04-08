// Copyright (c) 2023 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This file incorporates work covered by the following copyright and
// permission notice:
//
//    MIT No Attribution
//
//    Copyright 2022 National Technology & Engineering Solutions of Sandia, LLC
//    (NTESS). Under the terms of Contract DE-NA0003525 with NTESS, the U.S.
//    Government retains certain rights in this software.

package netstack

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/celzero/firestack/intra/log"
	neticmp "golang.org/x/net/icmp"
	netipv4 "golang.org/x/net/ipv4"
	netipv6 "golang.org/x/net/ipv6"

	"gvisor.dev/gvisor/pkg/bufferv2"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
)

// from: github.com/voiceflow/telepresence/blob/720d328be4/pkg/vif/icmp/packet.go#L31

const (
	NetworkUnreachable = iota
	HostUnreachable
	ProtocolUnreachable
	PortUnreachable
	// ...
)

// from: github.com/sandialabs/wiretap/blob/3ba102719/src/transport/icmp/icmp.go#L1

type icmpv2 struct {
	*preroutingMatch
	ep    stack.LinkEndpoint
	s     *stack.Stack
	h     GICMPHandler
	rule4 stack.Rule
	rule6 stack.Rule
}

// preroutingMatch matches packets in the prerouting stage and clones:
// packet into channel for processing.
type preroutingMatch struct {
	msgs chan stack.PacketBufferPtr
}

// When a new ICMP message hits the prerouting stage, the packet is cloned
// to the ICMP handler and dropped here.
func (m preroutingMatch) Match(hook stack.Hook, packet stack.PacketBufferPtr, inputInterfaceName, outputInterfaceName string) (matches bool, hotdrop bool) {
	if hook == stack.Prerouting {
		m.msgs <- packet
		return false, true
	}

	return false, false
}

// handleICMP proxies ICMP messages using whatever means it can with the permissions this binary
// has on the system.
func setupIcmpHandlerV2(s *stack.Stack, ep stack.LinkEndpoint, icmpHandler GICMPHandler) {
	// create iptables rule that drops icmp, but clones packet and sends it to this handler.
	headerFilter4 := stack.IPHeaderFilter{
		Protocol:      icmp.ProtocolNumber4,
		CheckProtocol: true,
	}

	headerFilter6 := stack.IPHeaderFilter{
		Protocol:      icmp.ProtocolNumber6,
		CheckProtocol: true,
	}

	match := preroutingMatch{
		msgs: make(chan stack.PacketBufferPtr),
	}

	rule4 := stack.Rule{
		Filter:   headerFilter4,
		Matchers: []stack.Matcher{match},
		Target: &stack.DropTarget{
			NetworkProtocol: ipv4.ProtocolNumber,
		},
	}

	rule6 := stack.Rule{
		Filter:   headerFilter6,
		Matchers: []stack.Matcher{match},
		Target: &stack.DropTarget{
			NetworkProtocol: ipv6.ProtocolNumber,
		},
	}

	tr := &icmpv2{
		preroutingMatch: &match,
		ep:              ep,
		s:               s,
		h:               icmpHandler,
		rule4:           rule4,
		rule6:           rule6,
	}

	tr.trap()
	go tr.serve()

	log.D("Transport: ICMP listener up")
}

func (tr *icmpv2) trap() {
	tid := stack.NATID
	for6 := true
	for4 := false
	// get a copy of the current rules table
	table4 := tr.s.IPTables().GetTable(tid, for4)
	table6 := tr.s.IPTables().GetTable(tid, for6)
	// append our rule to the front of the table
	table4.Rules = append([]stack.Rule{tr.rule4}, table4.Rules...)
	table6.Rules = append([]stack.Rule{tr.rule6}, table6.Rules...)
	// replace the existing rules table
	tr.s.IPTables().ReplaceTable(tid, table4, for4)
	tr.s.IPTables().ReplaceTable(tid, table4, for6)
}

func (tr *icmpv2) serve() {
	for {
		pkt := <-tr.msgs
		go tr.handleMessage(pkt.Clone())
	}
}

// handleICMPMessage parses ICMP packets and proxies them if possible.
func (tr *icmpv2) handleMessage(pkt stack.PacketBufferPtr) {
	defer pkt.DecRef()

	// Parse ICMP packet type.
	netHeader := pkt.Network()
	l4bytes := netHeader.Payload()

	isip4 := is4(netHeader.SourceAddress().String())
	if isip4 {
		icmpin := header.ICMPv4(l4bytes)
		src := udpaddr(netHeader.SourceAddress(), icmpin.SourcePort())
		dst := udpaddr(netHeader.DestinationAddress(), icmpin.DestinationPort())
		log.D("icmpv2: ICMPv6 %v -> %v", src, dst)
		switch icmpin.Type() {
		case header.ICMPv4Echo:
			tr.handleEcho(src, dst, pkt)
		default:
			log.W("icmpv2: ICMPv4 type unimplemented: %s", icmpin.Type())
		}
	} else {
		icmpin := header.ICMPv6(l4bytes)
		src := udpaddr(netHeader.SourceAddress(), icmpin.SourcePort())
		dst := udpaddr(netHeader.DestinationAddress(), icmpin.DestinationPort())
		log.D("icmpv2: ICMPv6 %v -> %v", src, dst)
		switch icmpin.Type() {
		case header.ICMPv6EchoRequest:
			tr.handleEcho(src, dst, pkt)
		default:
			log.W("icmpv2: ICMPv6 type not implemented: %s", icmpin.Type())
		}
	}

}

// handleICMPEcho tries to send ICMP echo requests to the true destination however it can.
// If successful, it sends an echo response to the peer.
func (tr *icmpv2) handleEcho(src, dst *net.UDPAddr, pkt stack.PacketBufferPtr) {
	var ok bool
	if ok = tr.h.PingOnce(src, dst, tr.pkt2bytes(pkt)); !ok {
		log.W("icmpv2: ICMP echo ping failed for %v -> %v", src, dst)
		tr.sendUnreachable(dst, src, pkt)
	} else {
		tr.sendEchoResponse(src, dst, pkt)
	}
}

// sendICMPEchoResponse sends an echo response to the peer with a spoofed source address.
func (tr *icmpv2) sendEchoResponse(src, dst *net.UDPAddr, pkt stack.PacketBufferPtr) error {
	var response []byte
	var ipHeader []byte
	var err error

	netHeader := pkt.Network()

	isip4 := is4(netHeader.DestinationAddress().String())

	if isip4 {
		l4 := header.ICMPv4(netHeader.Payload())
		// Create ICMP response and marshal it
		typ := netipv4.ICMPTypeEchoReply
		response, err = (&neticmp.Message{
			Type: typ,
			// TODO: get the code from the response packet?
			Code: 0, // Echo reply
			Body: &neticmp.Echo{
				ID:   int(l4.Ident()),
				Seq:  int(l4.Sequence()),
				Data: l4.Payload(),
			},
		}).Marshal(nil)
		if err != nil {
			log.W("icmpv2: ICMPv4; failed to marshal response: %v", err)
			return err
		}

		// Assert type to get network header bytes.
		ipv4Header, ok := netHeader.(header.IPv4)
		if !ok {
			errstr := "icmpv2: could not assert network header as IPv4 header"
			log.W(errstr)
			return errors.New(errstr)
		}
		// Swap source and destination addresses from original request.
		srcaddr := ipv4Header.DestinationAddress()
		ipv4Header.SetDestinationAddress(ipv4Header.SourceAddress())
		ipv4Header.SetSourceAddress(srcaddr)
		ipHeader = ipv4Header
	} else {
		l4 := header.ICMPv6(netHeader.Payload())
		srcip := asip(netHeader.DestinationAddress().String())
		dstip := asip(netHeader.SourceAddress().String())
		typ := netipv6.ICMPTypeEchoReply
		response, err = (&neticmp.Message{
			Type: typ,
			// TODO: get the code from the response packet?
			Code: 0, // Echo reply
			Body: &neticmp.Echo{
				ID:   int(l4.Ident()),
				Seq:  int(l4.Sequence()),
				Data: l4.Payload(),
			},
		}).Marshal(neticmp.IPv6PseudoHeader(srcip, dstip))
		if err != nil {
			log.W("icmpv2: ICMPv6; failed to marshal response: %v", err)
			return err
		}

		// Assert type to get network header bytes.
		ipv6Header, ok := netHeader.(header.IPv6)
		if !ok {
			errstr := "icmpv2: could not assert network header as IPv6 header"
			log.W(errstr)
			return errors.New(errstr)
		}
		// Swap source and destination addresses from original request.
		srcaddr := ipv6Header.DestinationAddress()
		ipv6Header.SetDestinationAddress(ipv6Header.SourceAddress())
		ipv6Header.SetSourceAddress(srcaddr)
		ipHeader = ipv6Header
	}

	res := append(ipHeader, response...)
	payload := bufferv2.MakeWithData(res)
	respkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: payload})
	defer respkt.DecRef()

	log.D("icmpv2: response: type %v/%v sz[%d] from %v <- %v", len(res), src, dst)

	var pout stack.PacketBufferList
	pout.PushBack(respkt)
	if _, err := tr.ep.WritePackets(pout); err != nil {
		log.E("icmpv2: err writing upstream res [%v <- %v] to tun %v", src, dst, err)
		return fmt.Errorf("icmpv2: err writing upstream res to tun: %v", err)
	}
	return nil
}

// ref: stackoverflow.com/a/26949038, stackoverflow.com/a/27087317
// and: archive.is/F2HB2
func (tr *icmpv2) sendUnreachable(src, dst *net.UDPAddr, pkt stack.PacketBufferPtr) error {
	var err error
	var icmpLayer []byte
	var ipLayer []byte

	const code = NetworkUnreachable
	netHeader := pkt.Network()

	isip4 := !is4(netHeader.DestinationAddress().String())

	if isip4 {
		l4 := header.ICMPv4(netHeader.Payload())
		l4.SetChecksum(0)
		l4payload := l4.Payload()
		ipv4Header, ok := netHeader.(header.IPv4)
		if !ok {
			errstr := "icmpv2: ICMPv4 unreachable: could not cat network header"
			log.W(errstr)
			return errors.New(errstr)
		}
		icmpLayer, err = (&neticmp.Message{
			Type: netipv4.ICMPTypeDestinationUnreachable,
			Code: NetworkUnreachable,
			Body: &neticmp.DstUnreach{
				Data: append(ipv4Header, l4[:len(l4)-len(l4payload)]...),
			},
		}).Marshal(nil)

		// include header + 64 bits of original payload
		// origSz := origHdr.HeaderLen() + 8
		// if origSz > len(icmpLayer) {
		//	origSz = len(icmpLayer)
		// }
		// icmpLayer = icmpLayer[:origSz]
		// checksum
		// Swap source and destination addresses from original request.
		srcaddr := ipv4Header.DestinationAddress()
		ipv4Header.SetDestinationAddress(ipv4Header.SourceAddress())
		ipv4Header.SetSourceAddress(srcaddr)
		ipLayer = ipv4Header
	} else {
		l4 := header.ICMPv6(netHeader.Payload())
		l4.SetChecksum(0)
		l4payload := l4.Payload()
		ipv6Header, ok := netHeader.(header.IPv6)
		if !ok {
			errstr := "icmpv2: ICMPv6 unreachable: could not cast network header"
			log.W(errstr)
			return errors.New(errstr)
		}
		icmpLayer, err = (&neticmp.Message{
			Type: netipv6.ICMPTypeDestinationUnreachable,
			Code: code,
			Body: &neticmp.DstUnreach{
				Data: append(ipv6Header, l4[:len(l4)-len(l4payload)]...),
			},
		}).Marshal(nil)

		// const IPv6MinMTU = 1280 // From RFC 2460, section 5
		// const HeaderLen = 8     // in bits, same for both IPv4 and IPv6
		// include as much of invoking packet as possible without the ICMPv6 packet
		// exceeding the minimum IPv6 MTU
		// origSz := origHdr.HeaderLen() + origHdr.PayloadLen()
		// if HeaderLen+origSz > IPv6MinMTU {
		//	origSz = IPv6MinMTU - HeaderLen
		// }
		// icmpLayer = icmpLayer[:origSz]
		// checksum
		srcaddr := ipv6Header.DestinationAddress()
		ipv6Header.SetDestinationAddress(ipv6Header.SourceAddress())
		ipv6Header.SetSourceAddress(srcaddr)
		ipLayer = ipv6Header
	}

	if err != nil {
		log.W("icmpv2: failed to marshal response:", err)
		return err
	}

	res := append(ipLayer, icmpLayer...)
	payload := bufferv2.MakeWithData(res)
	respkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: payload})
	defer respkt.DecRef()

	log.D("icmpv2: response: type %v/%v sz[%d] from %v <- %v", len(res), src, dst)

	var pout stack.PacketBufferList
	pout.PushBack(respkt)
	if _, err := tr.ep.WritePackets(pout); err != nil {
		log.E("icmpv2: err writing upstream res [%v <- %v] to tun %v", src, dst, err)
		return fmt.Errorf("icmpv2: err writing upstream res to tun: %v", err)
	}
	return nil
}

func is4(addr string) bool {
	if ip, err := netip.ParseAddr(addr); err == nil {
		return ip.Is4()
	}
	return false
}

func asip(addr string) net.IP {
	return net.ParseIP(addr)
}

func udpaddr(addr tcpip.Address, port uint16) *net.UDPAddr {
	ip := net.ParseIP(addr.String())
	return &net.UDPAddr{IP: ip, Port: int(port)}
}

func (tr *icmpv2) pkt2bytes(pkt stack.PacketBufferPtr) []byte {
	// return pkt.Network().Payload()
	r := make([]byte, tr.ep.MTU())
	din := bufferv2.MakeWithData(r)
	din.Append(pkt.TransportHeader().View())
	l7 := pkt.Data().ToBuffer()
	din.Merge(&l7)
	return din.Flatten()
}
