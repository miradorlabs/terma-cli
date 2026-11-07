package sensor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"
)

// A QUIC connection's first packets (Initial) are encrypted with keys anyone can derive
// from the packet itself (RFC 9001 §5.2), so the ClientHello in them, and its server
// name, is as readable as TLS's over TCP. Only QUIC version 1 is read.

var quicV1Salt = []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a}

// quicHellos reassembles ClientHellos split over Initial packets, by destination
// connection id; one ClientHello can outgrow a packet (post-quantum key shares do).
type quicHellos struct {
	mu   sync.Mutex
	bufs map[string]*quicHello
}

type quicHello struct {
	data map[uint64][]byte // CRYPTO frame data by offset
	seen time.Time
}

var quicState = &quicHellos{bufs: map[string]*quicHello{}}

// QUICInitialSNI is the server name a client's QUIC Initial packet, with those of the same
// connection before it, asks for; "" until the ClientHello is whole.
func QUICInitialSNI(pkt []byte) string { return quicState.feed(pkt) }

func (q *quicHellos) feed(pkt []byte) string {
	dcid, frames, ok := openInitial(pkt)
	if !ok {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	for k, b := range q.bufs {
		if now.Sub(b.seen) > 10*time.Second {
			delete(q.bufs, k)
		}
	}
	b := q.bufs[string(dcid)]
	if b == nil {
		if len(q.bufs) > 1024 {
			return ""
		}
		b = &quicHello{data: map[uint64][]byte{}}
		q.bufs[string(dcid)] = b
	}
	b.seen = now
	for off, d := range cryptoFrames(frames) {
		b.data[off] = d
	}
	hello := contiguous(b.data)
	if len(hello) < 4 || hello[0] != 0x01 {
		return ""
	}
	need := int(hello[1])<<16 | int(hello[2])<<8 | int(hello[3]) + 4
	if len(hello) < need {
		return ""
	}
	delete(q.bufs, string(dcid))
	// A TLS record header around the handshake message, as ClientHelloSNI reads it.
	rec := append([]byte{0x16, 0x03, 0x01, byte(need >> 8), byte(need)}, hello[:need]...)
	return ClientHelloSNI(rec)
}

// openInitial removes a client Initial packet's header protection and decrypts it,
// returning its destination connection id and its frames.
func openInitial(p []byte) (dcid, frames []byte, ok bool) {
	if len(p) < 7 || p[0]&0xc0 != 0xc0 || (p[0]>>4)&0x03 != 0 || binary.BigEndian.Uint32(p[1:]) != 1 {
		return nil, nil, false
	}
	off := 5
	dl := int(p[off])
	off++
	if off+dl > len(p) {
		return nil, nil, false
	}
	dcid = p[off : off+dl]
	off += dl
	if off >= len(p) {
		return nil, nil, false
	}
	off += 1 + int(p[off]) // source connection id
	tokenLen, n := varint(p[off:])
	if n == 0 {
		return nil, nil, false
	}
	off += n + int(tokenLen)
	length, n := varint(p[off:])
	if n == 0 {
		return nil, nil, false
	}
	off += n
	pnOff := off
	if pnOff+4+16 > len(p) || pnOff+int(length) > len(p) {
		return nil, nil, false
	}
	key, iv, hp := initialKeys(dcid)
	block, err := aes.NewCipher(hp)
	if err != nil {
		return nil, nil, false
	}
	mask := make([]byte, 16)
	block.Encrypt(mask, p[pnOff+4:pnOff+20])
	hdr := append([]byte{}, p[:pnOff+4]...)
	hdr[0] ^= mask[0] & 0x0f
	pnLen := int(hdr[0]&0x03) + 1
	var pn uint64
	for i := 0; i < pnLen; i++ {
		hdr[pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(hdr[pnOff+i])
	}
	hdr = hdr[:pnOff+pnLen]
	nonce := append([]byte{}, iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	kb, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, false
	}
	aead, err := cipher.NewGCM(kb)
	if err != nil {
		return nil, nil, false
	}
	frames, err = aead.Open(nil, nonce, p[pnOff+pnLen:pnOff+int(length)], hdr)
	return dcid, frames, err == nil
}

func initialKeys(dcid []byte) (key, iv, hp []byte) {
	secret, _ := hkdf.Extract(sha256.New, dcid, quicV1Salt)
	client := expandLabel(secret, "client in", 32)
	return expandLabel(client, "quic key", 16), expandLabel(client, "quic iv", 12), expandLabel(client, "quic hp", 16)
}

// expandLabel is TLS 1.3's HKDF-Expand-Label with an empty context.
func expandLabel(secret []byte, label string, n int) []byte {
	full := "tls13 " + label
	info := append([]byte{byte(n >> 8), byte(n), byte(len(full))}, full...)
	info = append(info, 0)
	out, _ := hkdf.Expand(sha256.New, secret, string(info), n)
	return out
}

// cryptoFrames is the CRYPTO frames' data by offset, read until a frame an Initial packet
// does not carry.
func cryptoFrames(f []byte) map[uint64][]byte {
	out := map[uint64][]byte{}
	for len(f) > 0 {
		typ, n := varint(f)
		if n == 0 {
			return out
		}
		f = f[n:]
		switch typ {
		case 0x00, 0x01: // padding, ping
		case 0x02, 0x03: // ack: largest, delay, range count, first range, ranges, ecn
			var vals [4]uint64
			for i := range vals {
				if vals[i], n = varint(f); n == 0 {
					return out
				}
				f = f[n:]
			}
			skip := int(vals[2]) * 2
			if typ == 0x03 {
				skip += 3
			}
			for i := 0; i < skip; i++ {
				if _, n = varint(f); n == 0 {
					return out
				}
				f = f[n:]
			}
		case 0x06: // crypto: offset, length, data
			off, n1 := varint(f)
			if n1 == 0 {
				return out
			}
			l, n2 := varint(f[n1:])
			if n2 == 0 || n1+n2+int(l) > len(f) {
				return out
			}
			out[off] = f[n1+n2 : n1+n2+int(l)]
			f = f[n1+n2+int(l):]
		default:
			return out
		}
	}
	return out
}

// contiguous joins data from offset 0 for as long as it runs without a gap.
func contiguous(data map[uint64][]byte) []byte {
	var out []byte
	for {
		d, ok := data[uint64(len(out))]
		if !ok || len(d) == 0 {
			return out
		}
		out = append(out, d...)
	}
}

func varint(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0
	}
	v := uint64(b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n
}
