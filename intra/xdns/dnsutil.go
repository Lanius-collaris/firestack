// Copyright (c) 2020 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This file incorporates work covered by the following copyright and
// permission notice:
//
//    ISC License
//
//    Copyright (c) 2018-2021
//    Frank Denis <j at pureftpd dot org>

package xdns

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"unicode/utf8"

	"github.com/celzero/firestack/intra/log"
	"github.com/miekg/dns"
)

func AsMsg(packet []byte) *dns.Msg {
	msg := &dns.Msg{}
	if err := msg.Unpack(packet); err != nil {
		return nil
	}
	return msg
}

func Request4FromResponse6(msg6 *dns.Msg) *dns.Msg {
	msg4 := &dns.Msg{
		Compress: true,
	}
	msg4.SetQuestion(QName(msg6), dns.TypeA)
	msg4.RecursionDesired = true
	msg4.CheckingDisabled = false
	msg4.AuthenticatedData = false
	msg4.Authoritative = false
	msg4.Id = msg6.Id
	return msg4
}

func Request4FromRequest6(msg6 *dns.Msg) *dns.Msg {
	msg4 := msg6.Copy()
	msg4.SetQuestion(QName(msg6), dns.TypeA)
	return msg4
}

func EmptyResponseFromMessage(srcMsg *dns.Msg) *dns.Msg {
	dstMsg := dns.Msg{
		MsgHdr:   srcMsg.MsgHdr,
		Compress: true,
	}
	dstMsg.Question = srcMsg.Question
	dstMsg.Response = true
	if srcMsg.RecursionDesired {
		dstMsg.RecursionAvailable = true
	}
	dstMsg.RecursionDesired = false
	dstMsg.CheckingDisabled = false
	dstMsg.AuthenticatedData = false
	if edns0 := srcMsg.IsEdns0(); edns0 != nil {
		dstMsg.SetEdns0(edns0.UDPSize(), edns0.Do())
	}
	return &dstMsg
}

func TruncatedResponse(packet []byte) ([]byte, error) {
	srcMsg := dns.Msg{}
	if err := srcMsg.Unpack(packet); err != nil {
		return nil, err
	}
	dstMsg := EmptyResponseFromMessage(&srcMsg)
	dstMsg.Truncated = true
	return dstMsg.Pack()
}

func HasTCFlag(packet []byte) bool {
	return packet[2]&2 == 2
}

func QName(msg *dns.Msg) string {
	if msg != nil && len(msg.Question) > 0 {
		return msg.Question[0].Name
	}
	return ""
}

func QType(msg *dns.Msg) uint16 {
	if msg != nil && len(msg.Question) > 0 {
		return msg.Question[0].Qtype
	}
	return dns.TypeNone
}

func Rcode(msg *dns.Msg) int {
	if msg != nil {
		return msg.Rcode
	}
	return dns.RcodeFormatError
}

func RTtl(msg *dns.Msg) int {
	maxttl := uint32(0)
	if msg == nil || len(msg.Answer) <= 0 {
		return int(maxttl)
	}
	for _, a := range msg.Answer {
		if a.Header().Ttl > 0 {
			ttl := a.Header().Ttl
			if maxttl < ttl {
				maxttl = ttl
			}
		}
	}
	return int(maxttl)
}

func GetInterestingRData(msg *dns.Msg) string {
	if msg == nil {
		return ""
	}
	var ipcsv string
	ip4s := IPHints(msg, dns.SVCB_IPV4HINT)
	ip6s := IPHints(msg, dns.SVCB_IPV6HINT)
	data := make([]string, 0)
	if len(ip4s) > 0 {
		data = append(data, netips2str(ip4s)...)
	}
	if len(ip6s) > 0 {
		data = append(data, netips2str(ip6s)...)
	}
	if len(data) > 0 {
		ipcsv += strings.Join(data, ",")
	}
	for _, a := range msg.Answer {
		switch r := a.(type) {
		case *dns.A:
			if len(ipcsv) > 0 {
				ipcsv += "," + ip2str(r.A)
			} else {
				ipcsv += ip2str(r.A)
			}
		case *dns.AAAA:
			if len(ipcsv) > 0 {
				ipcsv += "," + ip2str(r.AAAA)
			} else {
				ipcsv += ip2str(r.AAAA)
			}
		case *dns.NS:
			return r.Ns
		case *dns.TXT:
			if len(r.Txt) > 0 {
				return r.Txt[0]
			}
			return r.String()
		case *dns.SOA:
			return r.Mbox
		case *dns.HINFO:
			return r.Os
		case *dns.SRV:
			return r.Target
		case *dns.CAA:
			return r.Value
		case *dns.MX:
			return r.Mx
		case *dns.RP:
			return r.Mbox
		case *dns.DNSKEY:
			return r.PublicKey
		case *dns.DS:
			return r.Digest
		case *dns.RRSIG:
			return r.SignerName
		case *dns.SVCB:
			// already handled
			continue
		case *dns.HTTPS:
			// already handled
			continue
		case *dns.NSEC:
			return r.NextDomain
		case *dns.NSEC3:
			return r.NextDomain
		case *dns.NSEC3PARAM:
			return r.Salt
		case *dns.TLSA:
			return r.Certificate
		case *dns.OPT:
			return r.String()
		case *dns.APL:
			return r.String()
		case *dns.SSHFP:
			return r.FingerPrint
		case *dns.DNAME:
			return r.Target
		case *dns.NAPTR:
			return r.Service
		case *dns.CERT:
			return r.Certificate
		case *dns.DLV:
			return r.Digest
		case *dns.DHCID:
			return r.Digest
		case *dns.SMIMEA:
			return r.Certificate
		case *dns.NINFO:
			if len(r.ZSData) > 0 {
				return r.ZSData[0]
			}
			return r.String()
		case *dns.RKEY:
			return r.PublicKey
		case *dns.TKEY:
			return r.OtherData
		case *dns.TSIG:
			return r.OtherData
		case *dns.URI:
			return r.Target
		case *dns.HIP:
			return r.PublicKey
		case *dns.CDS:
			return r.Digest
		case *dns.OPENPGPKEY:
			return r.PublicKey
		case *dns.SPF:
			if len(r.Txt) > 0 {
				return r.Txt[0]
			}
			return r.String()
		case *dns.NSAPPTR:
			return r.Ptr
		case *dns.TALINK:
			return r.NextName
		case *dns.CSYNC:
			return r.String()
		case *dns.ZONEMD:
			return r.Digest
		default:
			// no-op
			continue
		}
	}
	return ipcsv
}

func Targets(msg *dns.Msg) []string {
	touched := make(map[string]interface{})
	var targets []string
	if qname, err := NormalizeQName(QName(msg)); err == nil {
		targets = append(targets, qname)
		touched[qname] = nil
	}
	for _, a := range msg.Answer {
		var target string
		switch r := a.(type) {
		case *dns.A:
			target = r.Header().Name
		case *dns.AAAA:
			target = r.Header().Name
		case *dns.CNAME:
			target = r.Target
		case *dns.SVCB:
			if r.Priority == 0 {
				target = r.Target
			}
		case *dns.HTTPS:
			if r.Priority == 0 {
				target = r.Target
			}
		default:
			// no-op
		}
		if len(target) <= 0 {
			continue
		} else if target, err := NormalizeQName(target); err == nil {
			if _, ok := touched[target]; !ok {
				targets = append(targets, target)
				touched[target] = nil
			}
		}
	}
	return targets
}

func NormalizeQName(str string) (string, error) {
	if len(str) == 0 || str == "." {
		return ".", nil
	}
	hasUpper := false
	str = strings.TrimSuffix(str, ".")
	strLen := len(str)
	for i := 0; i < strLen; i++ {
		c := str[i]
		if c >= utf8.RuneSelf {
			return str, errors.New("query name is not an ASCII string")
		}
		hasUpper = hasUpper || ('A' <= c && c <= 'Z')
	}
	if !hasUpper {
		return str, nil
	}
	var b strings.Builder
	b.Grow(len(str))
	for i := 0; i < strLen; i++ {
		c := str[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

func RemoveEDNS0Options(msg *dns.Msg) bool {
	edns0 := msg.IsEdns0()
	if edns0 == nil {
		return false
	}
	edns0.Option = []dns.EDNS0{}
	return true
}

func AddEDNS0PaddingIfNoneFound(msg *dns.Msg, unpaddedPacket []byte, paddingLen int) ([]byte, error) {
	edns0 := msg.IsEdns0()
	if edns0 == nil {
		msg.SetEdns0(uint16(MaxDNSPacketSize), false)
		edns0 = msg.IsEdns0()
		if edns0 == nil {
			return unpaddedPacket, nil
		}
	}
	for _, option := range edns0.Option {
		if option.Option() == dns.EDNS0PADDING {
			return unpaddedPacket, nil
		}
	}
	ext := new(dns.EDNS0_PADDING)
	padding := make([]byte, paddingLen)
	for i := range padding {
		padding[i] = 'X'
	}
	ext.Padding = padding[:paddingLen]
	edns0.Option = append(edns0.Option, ext)
	return msg.Pack()
}

func BlockResponseFromMessage(q []byte) (*dns.Msg, error) {
	r := &dns.Msg{}
	if err := r.Unpack(q); err != nil {
		return r, err
	}
	return RefusedResponseFromMessage(r)
}

func RefusedResponseFromMessage(srcMsg *dns.Msg) (dstMsg *dns.Msg, err error) {
	if srcMsg == nil {
		return nil, errors.New("empty source dns message")
	}
	dstMsg = EmptyResponseFromMessage(srcMsg)
	dstMsg.Rcode = dns.RcodeSuccess
	ttl := BlockTTL

	questions := srcMsg.Question
	if len(questions) == 0 {
		return
	}

	question := questions[0]
	sendHInfoResponse := true

	if question.Qtype == dns.TypeA {
		rr := new(dns.A)
		rr.Hdr = dns.RR_Header{
			Name:   question.Name,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		}
		rr.A = ip4.To4()
		if rr.A != nil {
			dstMsg.Answer = []dns.RR{rr}
			sendHInfoResponse = false
		}
	} else if question.Qtype == dns.TypeAAAA {
		rr := new(dns.AAAA)
		rr.Hdr = dns.RR_Header{
			Name:   question.Name,
			Rrtype: dns.TypeAAAA,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		}
		rr.AAAA = ip6.To16()
		if rr.AAAA != nil {
			dstMsg.Answer = []dns.RR{rr}
			sendHInfoResponse = false
		}
	} else if IsSVCBQuestion(&question) {
		rrh := new(dns.HTTPS)
		rrh.Hdr = dns.RR_Header{
			Name:   question.Name,
			Rrtype: dns.TypeHTTPS,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		}
		// TODO: instead use a self-referential "."?
		// some random 63-char string
		rrh.Target = fakedomain
		rra := new(dns.A)
		rra.Hdr = dns.RR_Header{
			Name:   fakedomain,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		}
		rra.A = ip4.To4()
		dstMsg.Answer = []dns.RR{rrh, rra}
		sendHInfoResponse = false
	}

	if sendHInfoResponse {
		hinfo := new(dns.HINFO)
		hinfo.Hdr = dns.RR_Header{
			Name:   question.Name,
			Rrtype: dns.TypeHINFO,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		}
		hinfo.Cpu = "These are not the queries you are"
		hinfo.Os = "looking for"
		dstMsg.Answer = []dns.RR{hinfo}
	}

	return
}

func HasRcodeSuccess(msg *dns.Msg) bool {
	return msg != nil && msg.Rcode == dns.RcodeSuccess
}

func HasAnyAnswer(msg *dns.Msg) bool {
	return msg != nil && len(msg.Answer) > 0
}

func HasAAnswer(msg *dns.Msg) bool {
	for _, answer := range msg.Answer {
		if answer.Header().Rrtype == dns.TypeA {
			rec, ok := answer.(*dns.A)
			if ok && len(rec.A) >= net.IPv4len {
				return true
			}
		}
	}
	return false
}

func HasAAAAAnswer(msg *dns.Msg) bool {
	for _, answer := range msg.Answer {
		if answer.Header().Rrtype == dns.TypeAAAA {
			rec, ok := answer.(*dns.AAAA)
			if ok && len(rec.AAAA) == net.IPv6len {
				return true
			}
		}
	}
	return false
}

func SubstAAAARecords(out *dns.Msg, subip6s []*netip.Addr, ttl int) bool {
	// substitute ips in any a / aaaa records
	touched := make(map[string]interface{})
	rrs := make([]dns.RR, 0)
	ip6 := subip6s[0].String()
	for _, answer := range out.Answer {
		switch rec := answer.(type) {
		case *dns.AAAA:
			// one aaaa rec per name
			if _, ok := touched[rec.Hdr.Name]; !ok {
				name := rec.Hdr.Name
				// fixme: use different ips for different names
				touched[rec.Hdr.Name] = nil
				if aaaanew := MakeAAAARecord(name, ip6, ttl); aaaanew != nil {
					rrs = append(rrs, aaaanew)
				} else {
					log.Debugf("dnsutil: subst AAAA rec fail for %s %s %d", name, ip6, ttl)
				}
			}
		default:
			// append cnames and other records as is
			rrs = append(rrs, rec)
		}
	}
	if len(rrs) > 0 {
		out.Answer = rrs
	}
	return len(touched) > 0
}

func SubstARecords(out *dns.Msg, subip4s []*netip.Addr, ttl int) bool {
	// substitute ips in any a / aaaa records
	touched := make(map[string]interface{})
	rrs := make([]dns.RR, 0)
	ip4 := subip4s[0].Unmap().String()
	for _, answer := range out.Answer {
		switch rec := answer.(type) {
		case *dns.A:
			// one a rec per name
			if _, ok := touched[rec.Hdr.Name]; !ok {
				name := rec.Hdr.Name
				// fixme: use different ips for different names
				touched[rec.Hdr.Name] = nil
				if anew := MakeARecord(name, ip4, ttl); anew != nil {
					rrs = append(rrs, anew)
				} else {
					log.Debugf("dnsutil: subst A rec fail for %s %s %d", name, ip4, ttl)
				}
			}
		default:
			// append cnames and other records as is
			rrs = append(rrs, rec)
		}
	}
	if len(rrs) > 0 {
		out.Answer = rrs
	}
	return len(touched) > 0
}

func SubstSVCBRecordIPs(out *dns.Msg, x dns.SVCBKey, subiphints []*netip.Addr, ttl int) bool {
	// substitute ip hints in https / svcb records
	i := 0
	for _, answer := range out.Answer {
		switch rec := answer.(type) {
		case *dns.SVCB:
			for j, kv := range rec.Value {
				k := kv.Key()
				// replace with a single ip hint
				if k == x && x == dns.SVCB_IPV6HINT {
					rec.Value[j] = &dns.SVCBIPv6Hint{
						Hint: []net.IP{subiphints[i].AsSlice()},
					}
					rec.Hdr.Ttl = uint32(ttl)
					i += 1
				} else if k == x && x == dns.SVCB_IPV4HINT {
					rec.Value[j] = &dns.SVCBIPv4Hint{
						Hint: []net.IP{subiphints[i].AsSlice()},
					}
					rec.Hdr.Ttl = uint32(ttl)
					i += 1
				}
			}
		case *dns.HTTPS:
			for j, kv := range rec.Value {
				k := kv.Key()
				// replace with a single ip hint
				if k == x && x == dns.SVCB_IPV6HINT {
					rec.Value[j] = &dns.SVCBIPv6Hint{
						Hint: []net.IP{subiphints[i].AsSlice()},
					}
					rec.Hdr.Ttl = uint32(ttl)
					i += 1
				} else if k == x && x == dns.SVCB_IPV4HINT {
					rec.Value[j] = &dns.SVCBIPv4Hint{
						Hint: []net.IP{subiphints[i].AsSlice()},
					}
					rec.Hdr.Ttl = uint32(ttl)
					i += 1
				}
			}
		}
	}
	return i > 0
}

func IPHints(msg *dns.Msg, x dns.SVCBKey) []*netip.Addr {
	if !HasSVCBQuestion(msg) {
		return nil
	}
	// extract ip hints from https / svcb records
	// https://tools.ietf.org/html/draft-ietf-dnsop-svcb-https-02#section-8.1
	ips := []*netip.Addr{}
	for _, answer := range msg.Answer {
		if !(answer.Header().Rrtype == dns.TypeHTTPS) && !(answer.Header().Rrtype == dns.TypeSVCB) {
			continue
		}
		switch rec := answer.(type) {
		case *dns.SVCB:
			for _, kv := range rec.Value {
				if kv.Key() != x {
					continue
				}
				// ipcsv may be "<nil>" or a csv of ips
				ipcsv := kv.String()
				for _, ipstr := range strings.Split(ipcsv, ",") {
					if v, err := netip.ParseAddr(ipstr); err == nil {
						ips = append(ips, &v)
					} else {
						log.Warnf("svcb: could not parse iphint %v", ipstr)
					}
				}
			}
		case *dns.HTTPS:
			for _, kv := range rec.Value {
				if kv.Key() != x {
					continue
				}
				// ipcsv may be "<nil>" or a csv of ips
				ipcsv := kv.String()
				for _, ipstr := range strings.Split(ipcsv, ",") {
					if v, err := netip.ParseAddr(ipstr); err == nil {
						ips = append(ips, &v)
					} else {
						log.Warnf("https: could not parse iphint %v", ipstr)
					}
				}
			}
		}
	}
	return ips
}

func AAnswer(msg *dns.Msg) []*netip.Addr {
	a4 := []*netip.Addr{}
	for _, answer := range msg.Answer {
		if answer.Header().Rrtype == dns.TypeA {
			if rec, ok := answer.(*dns.A); ok {
				if ipaddr, ok := netip.AddrFromSlice(rec.A); ok {
					a4 = append(a4, &ipaddr)
				}
			}
		}
	}
	return a4
}

func AAAAAnswer(msg *dns.Msg) []*netip.Addr {
	a6 := []*netip.Addr{}
	for _, answer := range msg.Answer {
		if answer.Header().Rrtype == dns.TypeAAAA {
			if rec, ok := answer.(*dns.AAAA); ok {
				if ipaddr, ok := netip.AddrFromSlice(rec.AAAA); ok {
					a6 = append(a6, &ipaddr)
				}
			}
		}
	}
	return a6
}

func HasAAAAQuestion(msg *dns.Msg) bool {
	q := msg.Question[0]
	return q.Qclass == dns.ClassINET && q.Qtype == dns.TypeAAAA
}

func HasAQuestion(msg *dns.Msg) bool {
	q := msg.Question[0]
	return q.Qclass == dns.ClassINET && q.Qtype == dns.TypeA
}

func IsSVCBQuestion(q *dns.Question) bool {
	return q.Qclass == dns.ClassINET && (q.Qtype == dns.TypeHTTPS || q.Qtype == dns.TypeSVCB)
}

func HasAQuadAQuestion(msg *dns.Msg) bool {
	return HasAAAAQuestion(msg) || HasAQuestion(msg)
}

func HasSVCBQuestion(msg *dns.Msg) bool {
	q := msg.Question[0]
	return IsSVCBQuestion(&q)
}

func MakeARecord(name string, ip4 string, expiry int) dns.RR {
	if len(ip4) <= 0 || len(name) <= 0 {
		return nil
	}
	ttl := uint32(expiry)

	b := net.ParseIP(ip4)
	if len(b) <= 0 {
		return nil
	}

	rec := new(dns.A)
	rec.Hdr = dns.RR_Header{
		Name:   name,
		Rrtype: dns.TypeA,
		Class:  dns.ClassINET,
		Ttl:    ttl,
	}
	rec.A = b
	return rec
}

func MakeAAAARecord(name string, ip6 string, expiry int) dns.RR {
	if len(ip6) <= 0 || len(name) <= 0 {
		return nil
	}
	ttl := uint32(expiry)

	b := net.ParseIP(ip6)
	if len(b) <= 0 {
		return nil
	}

	rec := new(dns.AAAA)
	rec.Hdr = dns.RR_Header{
		Name:   name,
		Rrtype: dns.TypeAAAA,
		Class:  dns.ClassINET,
		Ttl:    ttl,
	}
	rec.AAAA = b
	return rec
}

func MaybeToQuadA(answer dns.RR, prefix *net.IPNet) dns.RR {
	header := answer.Header()
	if header.Rrtype != dns.TypeA {
		return answer
	}
	ipv4 := answer.(*dns.A).A.To4()
	// TODO: refuse to translate bogons
	if ipv4 == nil {
		return nil
	}
	ttl := uint32(300) // 5 minutes
	if ttl > header.Ttl {
		ttl = header.Ttl
	}

	ipv6 := ip4to6(prefix, ipv4)

	trec := new(dns.AAAA)
	trec.Hdr = dns.RR_Header{
		Name:   header.Name,
		Rrtype: dns.TypeAAAA,
		Class:  header.Class,
		Ttl:    ttl,
	}
	trec.AAAA = ipv6
	return trec
}

func ToIp6Hint(answer dns.RR, prefix *net.IPNet) dns.RR {
	header := answer.Header()
	var kv []dns.SVCBKeyValue
	if header.Rrtype == dns.TypeHTTPS {
		kv = answer.(*dns.HTTPS).Value
	} else if header.Rrtype == dns.TypeSVCB {
		kv = answer.(*dns.SVCB).Value
	} else {
		log.Warnf("toIp6Hint: Not a svcb/https record/1")
		return nil
	}

	if len(kv) <= 0 {
		return nil
	}
	ttl := uint32(300) // 5 minutes

	hint4 := make([]string, 0)
	rest := make([]dns.SVCBKeyValue, 0)
	for _, x := range kv {
		if x.Key() == dns.SVCB_IPV6HINT {
			// ipv6hint found, no need to translate ipv4s
			return nil
		} else if x.Key() == dns.SVCB_IPV4HINT {
			ipstr := x.String()
			if len(ipstr) <= 0 {
				continue
			}
			hint4 = append(hint4, strings.Split(ipstr, ",")...)
		} else {
			rest = append(rest, x)
		}
	}

	hint6 := new(dns.SVCBIPv6Hint)
	for _, x := range hint4 {
		ip4 := net.ParseIP(x)
		if ip4 == nil {
			log.Warnf("dnsutil: invalid https/svcb ipv4hint %s", x)
			continue
		}
		hint6.Hint = append(hint6.Hint, ip4to6(prefix, ip4))
	}

	if header.Rrtype == dns.TypeSVCB {
		trec := new(dns.SVCB)
		trec.Hdr = dns.RR_Header{
			Name:   header.Name,
			Rrtype: header.Rrtype,
			Class:  header.Class,
			Ttl:    ttl,
		}
		trec.Value = append(rest, hint6)
		return trec
	} else if header.Rrtype == dns.TypeHTTPS {
		trec := new(dns.HTTPS)
		trec.Hdr = dns.RR_Header{
			Name:   header.Name,
			Rrtype: header.Rrtype,
			Class:  header.Class,
			Ttl:    ttl,
		}
		trec.Value = append(rest, hint6)
		return trec
	} else {
		// should never happen
		log.Errorf("toIp6Hint: Not a svcb/https record/2")
		return nil
	}
}

func ip4to6(prefix6 *net.IPNet, ip4 net.IP) net.IP {
	ip6 := make(net.IP, net.IPv6len)
	copy(ip6, prefix6.IP)
	n, _ := prefix6.Mask.Size()
	ipShift := n / 8
	for i := 0; i < net.IPv4len; i++ {
		// skip byte 8, datatracker.ietf.org/doc/html/rfc6052#section-2.2
		if ipShift+i == 8 {
			ipShift++
		}
		ip6[ipShift+i] = ip4[i]
	}
	return ip6
}

func AQuadAUnspecified(msg *dns.Msg) bool {
	ans := msg.Answer
	for _, rr := range ans {
		switch v := rr.(type) {
		case *dns.AAAA:
			if net.IPv6zero.Equal(v.AAAA) {
				return true
			}
		case *dns.A:
			if net.IPv4zero.Equal(v.A) {
				return true
			}
		}
	}
	return false
}

// Servfail returns a SERVFAIL response to the query q.
func Servfail(q []byte) []byte {
	msg := &dns.Msg{}
	if err := msg.Unpack(q); err != nil {
		log.Warnf("Error reading q for servfail: %v", err)
		return nil
	}
	msg.Response = true
	msg.RecursionAvailable = true
	msg.Rcode = dns.RcodeServerFailure
	msg.Extra = nil
	b, err := msg.Pack()
	if err != nil {
		log.Warnf("Error constructing servfail: %v", err)
	}
	return b
}

// GetBlocklistStampHeaderKey returns the http-header key for blocklists stamp
func GetBlocklistStampHeaderKey() string {
	return http.CanonicalHeaderKey(blocklistHeaderKey)
}

func netips2str(addrs []*netip.Addr) []string {
	var str []string
	for _, x := range addrs {
		str = append(str, x.String())
	}
	return str
}

func ip2str(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}
