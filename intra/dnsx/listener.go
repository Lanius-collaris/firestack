// Copyright (c) 2022 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package dnsx

// Summary is a summary of a DNS transaction, reported when it is complete.
type Summary struct {
	Type        string  // dnscrypt, dns53, doh
	ID          string  // transport id
	Latency     float64 // Response (or failure) latency in seconds
	QName       string  // query domain
	QType       int     // A, AAAA, SVCB, HTTPS, etc.
	RData       string  // response data, usually a csv of ips
	RCode       int     // response code
	RTtl        int     // response ttl
	Server      string
	RelayServer string
	Status      int
	Blocklists  string // csv separated list of blocklists names, if any.
}

func (s *Summary) CopyInto(other *Summary) {
	other.Type = s.Type
	other.ID = s.ID
	other.Latency = s.Latency
	other.QName = s.QName
	other.QType = s.QType
	other.RData = s.RData
	other.RCode = s.RCode
	other.RTtl = s.RTtl
	other.Server = s.Server
	other.RelayServer = s.RelayServer
	other.Status = s.Status
	other.Blocklists = s.Blocklists
}

// Listener receives Summaries.
type Listener interface {
	OnQuery(domain string, suggested string) string
	OnResponse(*Summary)
}
