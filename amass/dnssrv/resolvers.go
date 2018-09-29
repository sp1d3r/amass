// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package dnssrv

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/OWASP/Amass/amass/core"
	"github.com/OWASP/Amass/amass/utils"
	"github.com/miekg/dns"
)

var (
	// Public & free DNS servers
	PublicResolvers = []string{
		"1.1.1.1:53",     // Cloudflare
		"8.8.8.8:53",     // Google
		"64.6.64.6:53",   // Verisign
		"77.88.8.8:53",   // Yandex.DNS
		"74.82.42.42:53", // Hurricane Electric
		"1.0.0.1:53",     // Cloudflare Secondary
		"8.8.4.4:53",     // Google Secondary
		"77.88.8.1:53",   // Yandex.DNS Secondary
	}

	CustomResolvers = []string{}

	NumOfFileDescriptors int
	MaxConnections       *utils.Semaphore
	ExchangeTimes        chan time.Time
	ErrorTimes           chan time.Time
	WindowDuration       time.Duration = time.Second
)

func init() {
	NumOfFileDescriptors = (GetFileLimit() / 10) * 9
	MaxConnections = utils.NewSemaphore(NumOfFileDescriptors)
	ExchangeTimes = make(chan time.Time, int(float32(NumOfFileDescriptors)*1.5))
	ErrorTimes = make(chan time.Time, int(float32(NumOfFileDescriptors)*1.5))
	go ModifyMaxConnections()
}

func ModifyMaxConnections() {
	var count int
	var xchgWin, errWin []time.Time

	last := time.Now()
	// Start off with a reasonable load to the
	// network, and adjust based on performance
	if NumOfFileDescriptors > 4096 {
		count = NumOfFileDescriptors - 4096
		go MaxConnections.Acquire(count)
	}

	t := time.NewTicker(WindowDuration)
	defer t.Stop()

	for {
		select {
		case xchg := <-ExchangeTimes:
			xchgWin = append(xchgWin, xchg)
		case err := <-ErrorTimes:
			errWin = append(errWin, err)
		case <-t.C:
			end := time.Now()
			total := numInWindow(last, end, xchgWin)
			failures := numInWindow(last, end, errWin)
			// Check if we must reduce the number of simultaneous connections
			potential := NumOfFileDescriptors - count
			delta := potential / 4
			// Reduce if 10 percent or more of the connections timeout
			if result := analyzeConnResults(total, failures); result == 1 {
				count -= delta
				MaxConnections.Release(delta)
			} else if result == -1 && potential > 256 {
				count += delta
				go MaxConnections.Acquire(delta)
			}
			// Remove all the old slice elements
			last = end
			xchgWin = []time.Time{}
			errWin = []time.Time{}
		}
	}
}

func analyzeConnResults(total, failures int) int {
	if failures == 0 {
		return 1
	}

	frac := total / failures
	if frac == 0 {
		return -1
	}

	percent := 100 / frac
	if percent >= 20 {
		return -1
	} else if percent <= 10 {
		return 1
	}
	return 0
}

func numInWindow(x, y time.Time, s []time.Time) int {
	var count int

	for _, v := range s {
		if v.Before(x) || v.After(y) {
			continue
		}
		count++
	}
	return count
}

// NextResolverAddress - Requests the next server
func NextResolverAddress() string {
	rnd := rand.Int()
	if l := len(CustomResolvers); l > 0 {
		return CustomResolvers[rnd%l]
	}

	idx := rnd % len(PublicResolvers)
	return PublicResolvers[idx]
}

func SetCustomResolvers(resolvers []string) {
	for _, r := range resolvers {
		addr := r

		parts := strings.Split(addr, ":")
		if len(parts) == 1 && parts[0] == addr {
			addr += ":53"
		}

		CustomResolvers = utils.UniqueAppend(CustomResolvers, addr)
	}
}

func Resolve(name, qtype string) ([]core.DNSAnswer, error) {
	qt, err := textToTypeNum(qtype)
	if err != nil {
		return nil, err
	}

	tries := 3
	if qtype == "NS" || qtype == "MX" || qtype == "SOA" || qtype == "SPF" {
		tries = 7
	} else if qtype == "TXT" {
		tries = 10
	}

	var ans []core.DNSAnswer
	for i := 0; i < tries; i++ {
		var again bool
		var conn net.Conn

		d := &net.Dialer{}
		conn, err = d.Dial("udp", NextResolverAddress())
		if err != nil {
			time.Sleep(WindowDuration)
			continue
		}

		ans, err, again = ExchangeConn(conn, name, qt)
		conn.Close()
		if !again {
			break
		}
		time.Sleep(WindowDuration)
	}
	return ans, err
}

func Reverse(addr string) (string, string, error) {
	var name, ptr string

	ip := net.ParseIP(addr)
	if len(ip.To4()) == net.IPv4len {
		ptr = utils.ReverseIP(addr) + ".in-addr.arpa"
	} else if len(ip) == net.IPv6len {
		ptr = utils.IPv6NibbleFormat(utils.HexString(ip)) + ".ip6.arpa"
	} else {
		return ptr, "", fmt.Errorf("Invalid IP address parameter: %s", addr)
	}

	answers, err := Resolve(ptr, "PTR")
	if err != nil {
		return ptr, name, err
	}

	for _, a := range answers {
		if a.Type == 12 {
			name = removeLastDot(a.Data)
			break
		}
	}

	if name == "" {
		err = fmt.Errorf("PTR record not found for IP address: %s", addr)
	} else if strings.HasSuffix(name, ".in-addr.arpa") || strings.HasSuffix(name, ".ip6.arpa") {
		err = fmt.Errorf("Invalid target in PTR record answer: %s", name)
	}
	return ptr, name, err
}

// ExchangeConn - Encapsulates miekg/dns usage
func ExchangeConn(conn net.Conn, name string, qtype uint16) ([]core.DNSAnswer, error, bool) {
	var err error
	var r *dns.Msg
	var answers []core.DNSAnswer

	co := &dns.Conn{Conn: conn}
	msg := QueryMessage(name, qtype)
	ExchangeTimes <- time.Now()

	co.SetWriteDeadline(time.Now().Add(WindowDuration))
	if err = co.WriteMsg(msg); err != nil {
		ErrorTimes <- time.Now()
		return nil, fmt.Errorf("DNS error: Failed to write query msg: %v", err), true
	}

	co.SetReadDeadline(time.Now().Add(WindowDuration))
	r, err = co.ReadMsg()
	if err != nil {
		ErrorTimes <- time.Now()
		return nil, fmt.Errorf("DNS error: Failed to read query response: %v", err), true
	}
	// Check that the query was successful
	if r != nil && r.Rcode != dns.RcodeSuccess {
		again := true
		if r.Rcode == 3 {
			again = false
		}
		return nil, fmt.Errorf("DNS query for %s, type %d returned error %d", name, qtype, r.Rcode), again
	}

	for _, a := range ExtractRawData(r, qtype) {
		answers = append(answers, core.DNSAnswer{
			Name: name,
			Type: int(qtype),
			TTL:  0,
			Data: strings.TrimSpace(a),
		})
	}

	if len(answers) == 0 {
		return nil, fmt.Errorf("DNS query for %s, type %d returned 0 records", name, qtype), false
	}
	return answers, nil, false
}

func QueryMessage(name string, qtype uint16) *dns.Msg {
	m := &dns.Msg{
		MsgHdr: dns.MsgHdr{
			Authoritative:     false,
			AuthenticatedData: false,
			CheckingDisabled:  false,
			RecursionDesired:  true,
			Opcode:            dns.OpcodeQuery,
			Id:                dns.Id(),
			Rcode:             dns.RcodeSuccess,
		},
		Question: make([]dns.Question, 1),
	}
	m.Question[0] = dns.Question{
		Name:   dns.Fqdn(name),
		Qtype:  qtype,
		Qclass: uint16(dns.ClassINET),
	}
	m.Extra = append(m.Extra, setupOptions())
	return m
}

// setupOptions - Returns the EDNS0_SUBNET option for hiding our location
func setupOptions() *dns.OPT {
	e := &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        1,
		SourceNetmask: 0,
		SourceScope:   0,
		Address:       net.ParseIP("0.0.0.0").To4(),
	}

	return &dns.OPT{
		Hdr: dns.RR_Header{
			Name:   ".",
			Rrtype: dns.TypeOPT,
		},
		Option: []dns.EDNS0{e},
	}
}

func ZoneTransfer(domain, sub, server string) ([]string, error) {
	var results []string

	a, err := Resolve(server, "A")
	if err != nil {
		return results, fmt.Errorf("DNS A record query error: %s: %v", server, err)
	}
	addr := a[0].Data

	// Set the maximum time allowed for making the connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr+":53")
	if err != nil {
		return results, fmt.Errorf("Zone xfr error: Failed to obtain TCP connection to %s: %v", addr+":53", err)
	}
	defer conn.Close()

	xfr := &dns.Transfer{
		Conn:        &dns.Conn{Conn: conn},
		ReadTimeout: 10 * time.Second,
	}

	m := &dns.Msg{}
	m.SetAxfr(dns.Fqdn(sub))

	in, err := xfr.In(m, "")
	if err != nil {
		return results, fmt.Errorf("DNS zone transfer error: %s: %v", addr+":53", err)
	}

	for en := range in {
		names := getXfrNames(en)
		if names == nil {
			continue
		}

		for _, name := range names {
			n := name[:len(name)-1]

			results = append(results, n)
		}
	}
	return results, nil
}

//-------------------------------------------------------------------------------------------------
// Support functions
//-------------------------------------------------------------------------------------------------

func getXfrNames(en *dns.Envelope) []string {
	var names []string

	if en.Error != nil {
		return nil
	}

	for _, a := range en.RR {
		var name string

		switch v := a.(type) {
		case *dns.A:
			name = v.Hdr.Name
		case *dns.AAAA:
			name = v.Hdr.Name
		case *dns.NS:
			name = v.Ns
		case *dns.CNAME:
			name = v.Hdr.Name
		case *dns.SRV:
			name = v.Hdr.Name
		case *dns.TXT:
			name = v.Hdr.Name
		default:
			continue
		}

		names = append(names, name)
	}
	return names
}

func textToTypeNum(text string) (uint16, error) {
	var qtype uint16

	switch text {
	case "CNAME":
		qtype = dns.TypeCNAME
	case "A":
		qtype = dns.TypeA
	case "AAAA":
		qtype = dns.TypeAAAA
	case "PTR":
		qtype = dns.TypePTR
	case "NS":
		qtype = dns.TypeNS
	case "MX":
		qtype = dns.TypeMX
	case "TXT":
		qtype = dns.TypeTXT
	case "SOA":
		qtype = dns.TypeSOA
	case "SPF":
		qtype = dns.TypeSPF
	case "SRV":
		qtype = dns.TypeSRV
	}

	if qtype == 0 {
		return qtype, fmt.Errorf("DNS message type '%s' not supported", text)
	}
	return qtype, nil
}

func ExtractRawData(msg *dns.Msg, qtype uint16) []string {
	var data []string

	for _, a := range msg.Answer {
		if a.Header().Rrtype == qtype {
			switch qtype {
			case dns.TypeA:
				if t, ok := a.(*dns.A); ok {
					data = append(data, utils.CopyString(t.A.String()))
				}
			case dns.TypeAAAA:
				if t, ok := a.(*dns.AAAA); ok {
					data = append(data, utils.CopyString(t.AAAA.String()))
				}
			case dns.TypeCNAME:
				if t, ok := a.(*dns.CNAME); ok {
					data = append(data, utils.CopyString(t.Target))
				}
			case dns.TypePTR:
				if t, ok := a.(*dns.PTR); ok {
					data = append(data, utils.CopyString(t.Ptr))
				}
			case dns.TypeNS:
				if t, ok := a.(*dns.NS); ok {
					data = append(data, realName(t.Hdr)+","+removeLastDot(t.Ns))
				}
			case dns.TypeMX:
				if t, ok := a.(*dns.MX); ok {
					data = append(data, utils.CopyString(t.Mx))
				}
			case dns.TypeTXT:
				if t, ok := a.(*dns.TXT); ok {
					var all string

					for _, piece := range t.Txt {
						all += piece + " "
					}
					data = append(data, all)
				}
			case dns.TypeSOA:
				if t, ok := a.(*dns.SOA); ok {
					data = append(data, t.Ns+" "+t.Mbox)
				}
			case dns.TypeSPF:
				if t, ok := a.(*dns.SPF); ok {
					var all string

					for _, piece := range t.Txt {
						all += piece + " "
					}
					data = append(data, all)
				}
			case dns.TypeSRV:
				if t, ok := a.(*dns.SRV); ok {
					data = append(data, utils.CopyString(t.Target))
				}
			}
		}
	}
	return data
}

func realName(hdr dns.RR_Header) string {
	pieces := strings.Split(hdr.Name, " ")

	return removeLastDot(pieces[len(pieces)-1])
}

func removeLastDot(name string) string {
	sz := len(name)

	if sz > 0 && name[sz-1] == '.' {
		return name[:sz-1]
	}
	return name
}
