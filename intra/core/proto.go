// Copyright (c) 2023 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package core

import (
	"net"
	"time"
)

// from: github.com/eycorsican/go-tun2socks/blob/301549c435/core/conn.go#LL3C9-L3C9

// TCPConn abstracts a TCP connection comming from TUN. This connection
// should be handled by a registered TCP proxy handler. It's important
// to note that callback members are called from lwIP, they are already
// in the lwIP thread when they are called, that is, they are holding
// the lwipMutex.
type TCPConn interface {
	// RemoteAddr returns the destination network address.
	RemoteAddr() net.Addr

	// LocalAddr returns the local client network address.
	LocalAddr() net.Addr

	// Read reads data comming from TUN, note that it reads from an
	// underlying pipe that the writer writes in the lwip thread,
	// write op blocks until previous written data is consumed, one
	// should read out all data as soon as possible.
	Read(data []byte) (int, error)

	// Write writes data to TUN.
	Write(data []byte) (int, error)

	// Close closes the connection.
	Close() error

	// CloseWrite closes the writing side by sending a FIN
	// segment to local peer. That means we can write no further
	// data to TUN.
	CloseWrite() error

	// CloseRead closes the reading side. That means we can no longer
	// read more from TUN.
	CloseRead() error

	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// TCPConn abstracts a UDP connection comming from TUN. This connection
// should be handled by a registered UDP proxy handler.
type UDPConn interface {
	// LocalAddr returns the local client network address.
	LocalAddr() *net.UDPAddr
	RemoteAddr() *net.UDPAddr

	// ReceiveTo will be called when data arrives from TUN, and the received
	// data should be sent to addr.
	ReceiveTo(data []byte, addr *net.UDPAddr) error

	// WriteFrom writes data to TUN, addr will be set as source address of
	// UDP packets that output to TUN.
	WriteFrom(data []byte, addr *net.UDPAddr) (int, error)

	// Close closes the connection.
	Close() error
}
