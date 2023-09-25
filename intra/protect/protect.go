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

package protect

import (
	"net"
	"net/netip"
	"syscall"

	"github.com/celzero/firestack/intra/log"
)

type Options struct {
	// PID is the ID of the proxy to forward the connection to.
	PID string
	// CID is the ID of this connection.
	CID string
	// UID is the ID of the app which owns this connection.
	UID string
}

// Controller provides answers to filter network traffic.
type Controller interface {
	// Flow is called on a new connection setup; return "proxyid,connid" to forward the connection
	// to a pre-registered proxy; "Base" to allow the connection; "Block" to block the connection.
	// "connid" is used to uniquely identify a connection across all proxies, and a summary of the
	// connection is sent back to a pre-registered listener.
	// protocol is 6 for TCP, 17 for UDP, 1 for ICMP.
	// uid is -1 in case owner-uid of the connection couldn't be determined.
	// src and dst are string'd representation of net.TCPAddr and net.UDPAddr.
	// origdsts is a comma-separated list of original source IPs, this may be same as dst.
	// domains is a comma-separated list of domain names associated with origsrcs, if any.
	// blocklists is a comma-separated list of blocklist names, if any.
	Flow(protocol int32, uid int, src, dst, origdsts, domains, blocklists string) *Options
	// Calls in to javaland asking it to bind fd to any internet-capable IPv4 interface.
	Bind4(who string, fd int)
	// Calls in to javaland asking it to bind fd to any internet-capable IPv6 interface.
	// also: github.com/lwip-tcpip/lwip/blob/239918c/src/core/ipv6/ip6.c#L68
	Bind6(who string, fd int)
}

type Protector interface {
	// Returns ip to bind given a network, n
	UIP(n string) []byte
}

func networkBinder(who string, ctl Controller) func(string, string, syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) (err error) {
		dst, err := netip.ParseAddrPort(address)
		log.D("control: net(%s), orig(%s/%w), bind(%s)", network, dst, err, who)
		return c.Control(func(fd uintptr) {
			sock := int(fd)
			switch network {
			case "tcp6", "udp6":
				ctl.Bind6(who, sock)
			case "tcp4", "udp4":
				ctl.Bind4(who, sock)
			case "tcp", "udp":
				if dst.Addr().Is6() {
					ctl.Bind6(who, sock)
				} else {
					ctl.Bind4(who, sock)
				}
			default:
				// no-op
			}
		})
	}
}

func ipBinder(p Protector) func(string, string, syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) (err error) {
		src := p.UIP(network)
		ipaddr, _ := netip.AddrFromSlice(src)
		origaddr, err := netip.ParseAddrPort(address)
		origport := int(origaddr.Port())
		log.D("control: net(%s), orig(%s/%w), bind(%s)", network, origaddr, err, ipaddr)
		if err != nil {
			return err
		}

		bind6 := func(fd uintptr) error {
			sc := &syscall.SockaddrInet6{Addr: ipaddr.As16(), Port: origport}
			return syscall.Bind(int(fd), sc)
		}
		bind4 := func(fd uintptr) error {
			sc := &syscall.SockaddrInet4{Addr: ipaddr.As4(), Port: origport}
			return syscall.Bind(int(fd), sc)
		}

		return c.Control(func(fd uintptr) {
			if origaddr.Addr().IsUnspecified() {
				return
			}

			switch network {
			case "tcp6", "udp6":
				// TODO: zone := origaddr.Addr().Zone()
				err = bind6(fd)
			case "tcp4", "udp4":
				err = bind4(fd)
			case "tcp", "udp":
				if origaddr.Addr().Is6() {
					err = bind6(fd)
				} else {
					err = bind4(fd)
				}
			default:
				// no-op
			}
			if err != nil {
				log.E("protect: fail to bind ip(%s) to socket %v", ipaddr, err)
			}
		})
	}
}

// Creates a dialer that binds to a particular ip.
func MakeDialer(p Protector) *net.Dialer {
	if p == nil {
		return MakeDefaultDialer()
	}
	d := &net.Dialer{
		Control: ipBinder(p),
	}
	return d
}

// Creates a listener that binds to a particular ip.
func MakeListenConfig(p Protector) *net.ListenConfig {
	if p == nil {
		return MakeDefaultListenConfig()
	}
	return &net.ListenConfig{
		Control: ipBinder(p),
	}
}

// Creates a dialer that can bind to any active interface.
func MakeNsDialer(who string, c Controller) *net.Dialer {
	if c == nil {
		return MakeDefaultDialer()
	}
	d := &net.Dialer{
		Control: networkBinder(who, c),
	}
	return d
}

// Creates a RDial that can bind to any active interface.
func MakeNsRDial(who string, c Controller) *RDial {
	if c != nil {
		d := &net.Dialer{
			Control: networkBinder(who, c),
		}
		return &RDial{
			Dialer: d,
		}
	} else {
		return &RDial{
			Dialer: MakeDefaultDialer(),
		}
	}
}

// Creates a listener that can bind to any active interface.
func MakeNsListenConfig(who string, c Controller) *net.ListenConfig {
	if c == nil {
		return MakeDefaultListenConfig()
	}
	return &net.ListenConfig{
		Control: networkBinder(who, c),
	}
}

// Creates a plain old dialer
func MakeDefaultDialer() *net.Dialer {
	return &net.Dialer{}
}

// Creates a plain old listener
func MakeDefaultListenConfig() *net.ListenConfig {
	return &net.ListenConfig{}
}
