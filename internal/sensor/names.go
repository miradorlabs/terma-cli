package sensor

import (
	"encoding/binary"
	"net/netip"
	"strings"
)

// Packet is the part of a captured IP packet the names come from.
type Packet struct {
	Src, Dst netip.AddrPort
	TCP      bool
	Payload  []byte
}

// ParseIP reads an IPv4 or IPv6 packet carrying TCP or UDP.
func ParseIP(b []byte) (Packet, bool) {
	if len(b) < 1 {
		return Packet{}, false
	}
	var (
		p        Packet
		src, dst netip.Addr
		proto    byte
		rest     []byte
	)
	switch b[0] >> 4 {
	case 4:
		ihl := int(b[0]&0x0f) * 4
		if len(b) < ihl || ihl < 20 {
			return p, false
		}
		proto, src, dst = b[9], netip.AddrFrom4([4]byte(b[12:16])), netip.AddrFrom4([4]byte(b[16:20]))
		rest = b[ihl:]
	case 6:
		if len(b) < 40 {
			return p, false
		}
		proto, src, dst = b[6], netip.AddrFrom16([16]byte(b[8:24])).Unmap(), netip.AddrFrom16([16]byte(b[24:40])).Unmap()
		rest = b[40:]
	default:
		return p, false
	}
	if len(rest) < 8 {
		return p, false
	}
	sport, dport := binary.BigEndian.Uint16(rest), binary.BigEndian.Uint16(rest[2:])
	switch proto {
	case 6:
		doff := int(rest[12]>>4) * 4
		if len(rest) < doff {
			return p, false
		}
		p.TCP, p.Payload = true, rest[doff:]
	case 17:
		p.Payload = rest[8:]
	default:
		return p, false
	}
	p.Src, p.Dst = netip.AddrPortFrom(src, sport), netip.AddrPortFrom(dst, dport)
	return p, true
}

// Names hands sink the hosts a captured packet names: a DNS answer's addresses under the
// name that was asked for, or the server name a TLS ClientHello asks for.
func Names(p Packet, sink Sink) {
	payload := p.Payload
	if p.Src.Port() == 53 {
		if p.TCP { // DNS over TCP prefixes the message with its length
			if len(payload) < 2 {
				return
			}
			payload = payload[2:]
		}
		host, addrs := DNSAnswer(payload)
		for _, a := range addrs {
			sink.Name(a, host)
		}
		return
	}
	if p.TCP {
		if host := ClientHelloSNI(payload); host != "" {
			sink.Hello(p.Src, p.Dst, host)
		}
		return
	}
	if p.Dst.Port() == 443 {
		if host := QUICInitialSNI(payload); host != "" {
			sink.Hello(p.Src, p.Dst, host)
		}
	}
}

// DNSAnswer is the question's name and the A and AAAA addresses a DNS response gives it.
func DNSAnswer(m []byte) (string, []netip.Addr) {
	if len(m) < 12 || m[2]&0x80 == 0 { // not a response
		return "", nil
	}
	qd, an := int(binary.BigEndian.Uint16(m[4:])), int(binary.BigEndian.Uint16(m[6:]))
	off := 12
	var host string
	for i := 0; i < qd; i++ {
		name, n, ok := dnsName(m, off)
		if !ok {
			return "", nil
		}
		if i == 0 {
			host = name
		}
		off = n + 4
	}
	var addrs []netip.Addr
	for i := 0; i < an && off < len(m); i++ {
		_, n, ok := dnsName(m, off)
		if !ok || n+10 > len(m) {
			break
		}
		typ := binary.BigEndian.Uint16(m[n:])
		rdlen := int(binary.BigEndian.Uint16(m[n+8:]))
		rd := n + 10
		if rd+rdlen > len(m) {
			break
		}
		switch {
		case typ == 1 && rdlen == 4:
			addrs = append(addrs, netip.AddrFrom4([4]byte(m[rd:rd+4])))
		case typ == 28 && rdlen == 16:
			addrs = append(addrs, netip.AddrFrom16([16]byte(m[rd:rd+16])).Unmap())
		}
		off = rd + rdlen
	}
	return host, addrs
}

// dnsName reads the name at off, following compression pointers, and returns where the
// record continues after it.
func dnsName(m []byte, off int) (string, int, bool) {
	var labels []string
	end, jumped := 0, false
	for hops := 0; hops < 32; hops++ {
		if off >= len(m) {
			return "", 0, false
		}
		l := int(m[off])
		switch {
		case l == 0:
			if !jumped {
				end = off + 1
			}
			return strings.Join(labels, "."), end, true
		case l&0xc0 == 0xc0:
			if off+1 >= len(m) {
				return "", 0, false
			}
			if !jumped {
				end = off + 2
			}
			jumped = true
			off = int(binary.BigEndian.Uint16(m[off:])) & 0x3fff
		default:
			if off+1+l > len(m) {
				return "", 0, false
			}
			labels = append(labels, string(m[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", 0, false
}

// ClientHelloSNI is the server_name a TLS ClientHello record asks for, if the record is
// whole in b.
func ClientHelloSNI(b []byte) string {
	// record header (5), handshake header (4), version (2), random (32)
	if len(b) < 43 || b[0] != 0x16 || b[5] != 0x01 {
		return ""
	}
	p := 43
	skip := func(lenBytes int) bool {
		if p+lenBytes > len(b) {
			return false
		}
		n := 0
		for i := 0; i < lenBytes; i++ {
			n = n<<8 | int(b[p+i])
		}
		p += lenBytes + n
		return p <= len(b)
	}
	if !skip(1) || !skip(2) || !skip(1) { // session id, cipher suites, compression
		return ""
	}
	if p+2 > len(b) {
		return ""
	}
	end := min(p+2+int(binary.BigEndian.Uint16(b[p:])), len(b))
	p += 2
	for p+4 <= end {
		typ, n := binary.BigEndian.Uint16(b[p:]), int(binary.BigEndian.Uint16(b[p+2:]))
		p += 4
		if typ == 0 && p+5 <= end { // server_name: list length, name type, name length
			l := int(binary.BigEndian.Uint16(b[p+3:]))
			if p+5+l <= end && b[p+2] == 0 {
				return string(b[p+5 : p+5+l])
			}
			return ""
		}
		p += n
	}
	return ""
}
