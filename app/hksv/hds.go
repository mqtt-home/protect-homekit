package hksv

// HomeKit Data Stream (HDS) transport. HKSV recordings travel over a dedicated
// encrypted TCP connection rather than the RTP path used for live streaming.
// After the controller writes SetupDataStreamTransport, it connects to a port
// the accessory advertises; both sides derive per-stream ChaCha20-Poly1305 keys
// from the HAP Pair-Verify shared secret and exchange length-prefixed frames.
//
// Frame layout (plaintext header authenticated as AAD):
//
//	+--------+----------------+---------------------+----------+
//	| type=1 | payloadLen(3)  | ciphertext(len)     | tag(16)  |
//	| 1 byte | 3 bytes BE     | len bytes           | 16 bytes |
//	+--------+----------------+---------------------+----------+
//
// The nonce is a 64-bit little-endian counter left-padded to 12 bytes; the
// accessory keeps independent send/receive counters, both starting at zero.
// After decryption the payload is: [headerLen:1][header OPACK][message OPACK].

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	hdsFrameType     = 0x01
	hdsMaxPayloadLen = 0xFFFFF // 1048575, the 24-bit length ceiling
	hdsHeaderLen     = 4       // type(1) + length(3)
)

// hdsKeys are the directional transport keys derived for one data stream.
type hdsKeys struct {
	accessoryToController []byte // encrypts frames the accessory sends
	controllerToAccessory []byte // decrypts frames the accessory receives
}

// deriveHDSKeys computes the two directional keys from the Pair-Verify shared
// secret and the salts exchanged in SetupDataStreamTransport. The salt is the
// controller salt followed by the accessory salt; the info strings and SHA-512
// HKDF match the HKSV specification.
func deriveHDSKeys(shared [32]byte, controllerSalt, accessorySalt []byte) (hdsKeys, error) {
	salt := make([]byte, 0, len(controllerSalt)+len(accessorySalt))
	salt = append(salt, controllerSalt...)
	salt = append(salt, accessorySalt...)

	a2c, err := hkdf.Key(sha512.New, shared[:], salt, "HDS-Read-Encryption-Key", 32)
	if err != nil {
		return hdsKeys{}, fmt.Errorf("derive read key: %w", err)
	}
	c2a, err := hkdf.Key(sha512.New, shared[:], salt, "HDS-Write-Encryption-Key", 32)
	if err != nil {
		return hdsKeys{}, fmt.Errorf("derive write key: %w", err)
	}
	return hdsKeys{accessoryToController: a2c, controllerToAccessory: c2a}, nil
}

// hdsConn frames and encrypts an HDS connection. It is not safe for concurrent
// writers; callers serialize sends (the recording loop is single-writer).
type hdsConn struct {
	rw      io.ReadWriter
	encAEAD cipher.AEAD
	decAEAD cipher.AEAD
	sendCtr uint64
	recvCtr uint64
}

func newHDSConn(rw io.ReadWriter, keys hdsKeys) (*hdsConn, error) {
	enc, err := chacha20poly1305.New(keys.accessoryToController)
	if err != nil {
		return nil, err
	}
	dec, err := chacha20poly1305.New(keys.controllerToAccessory)
	if err != nil {
		return nil, err
	}
	return &hdsConn{rw: rw, encAEAD: enc, decAEAD: dec}, nil
}

// nonce12 builds the 12-byte nonce: four zero bytes followed by the counter in
// little-endian.
func nonce12(counter uint64) []byte {
	var n [12]byte
	binary.LittleEndian.PutUint64(n[4:], counter)
	return n[:]
}

// writeFrame encrypts and sends one payload as a single HDS frame.
func (c *hdsConn) writeFrame(payload []byte) error {
	if len(payload) > hdsMaxPayloadLen {
		return fmt.Errorf("hds: payload %d exceeds max %d", len(payload), hdsMaxPayloadLen)
	}
	header := make([]byte, hdsHeaderLen)
	header[0] = hdsFrameType
	putUint24BE(header[1:], uint32(len(payload)))

	sealed := c.encAEAD.Seal(nil, nonce12(c.sendCtr), payload, header)
	c.sendCtr++

	frame := make([]byte, 0, hdsHeaderLen+len(sealed))
	frame = append(frame, header...)
	frame = append(frame, sealed...)
	if _, err := c.rw.Write(frame); err != nil {
		return err
	}
	return nil
}

// readFrame reads and decrypts one HDS frame, returning the plaintext payload.
func (c *hdsConn) readFrame() ([]byte, error) {
	header := make([]byte, hdsHeaderLen)
	if _, err := io.ReadFull(c.rw, header); err != nil {
		return nil, err
	}
	if header[0] != hdsFrameType {
		return nil, fmt.Errorf("hds: unexpected frame type 0x%02x", header[0])
	}
	length := int(uint24BE(header[1:]))
	if length > hdsMaxPayloadLen {
		return nil, fmt.Errorf("hds: frame length %d exceeds max", length)
	}

	sealed := make([]byte, length+chacha20poly1305.Overhead)
	if _, err := io.ReadFull(c.rw, sealed); err != nil {
		return nil, err
	}

	plaintext, err := c.decAEAD.Open(nil, nonce12(c.recvCtr), sealed, header)
	if err != nil {
		// Leave recvCtr untouched on failure so a bad frame doesn't desync the
		// stream, matching the reference implementation.
		return nil, fmt.Errorf("hds: decrypt frame: %w", err)
	}
	c.recvCtr++
	return plaintext, nil
}

func putUint24BE(b []byte, v uint32) {
	b[0] = byte(v >> 16)
	b[1] = byte(v >> 8)
	b[2] = byte(v)
}

func uint24BE(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

// --- Message layer -------------------------------------------------------

// HDS message types, discriminated by which header key is present.
const (
	msgEvent = iota
	msgRequest
	msgResponse
)

// hdsStatus values used in response headers.
const (
	hdsStatusSuccess               = 0
	hdsStatusProtocolSpecificError = 6
)

// hdsMessage is a decoded HDS protocol message: a header dictionary plus an
// OPACK message body.
type hdsMessage struct {
	kind     int
	protocol string
	topic    string
	id       int64
	status   int64
	message  map[string]any
}

var errShortPayload = errors.New("hds: payload shorter than declared header")

// decodeMessage splits a decrypted payload into its header and message OPACK
// dictionaries and classifies the message.
func decodeMessage(payload []byte) (*hdsMessage, error) {
	if len(payload) < 1 {
		return nil, errShortPayload
	}
	headerLen := int(payload[0])
	if 1+headerLen > len(payload) {
		return nil, errShortPayload
	}
	headerRaw := payload[1 : 1+headerLen]
	messageRaw := payload[1+headerLen:]

	hv, err := OPACKDecode(headerRaw)
	if err != nil {
		return nil, fmt.Errorf("hds: decode header: %w", err)
	}
	header, ok := hv.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("hds: header is %T, want dict", hv)
	}

	m := &hdsMessage{}
	if p, ok := header["protocol"].(string); ok {
		m.protocol = p
	}
	switch {
	case has(header, "event"):
		m.kind = msgEvent
		m.topic, _ = header["event"].(string)
	case has(header, "request"):
		m.kind = msgRequest
		m.topic, _ = header["request"].(string)
	case has(header, "response"):
		m.kind = msgResponse
		m.topic, _ = header["response"].(string)
	}
	if id, ok := asInt64(header["id"]); ok {
		m.id = id
	}
	if st, ok := asInt64(header["status"]); ok {
		m.status = st
	}

	// The message body may be empty (e.g. the hello handshake).
	if len(messageRaw) > 0 {
		mv, err := OPACKDecode(messageRaw)
		if err != nil {
			return nil, fmt.Errorf("hds: decode message: %w", err)
		}
		if md, ok := mv.(map[string]any); ok {
			m.message = md
		}
	}
	if m.message == nil {
		m.message = map[string]any{}
	}
	return m, nil
}

// encodeMessage assembles the wire payload from a header dictionary and message
// value (which is OPACK-encoded, typically a Dict).
func encodeMessage(header Dict, message any) ([]byte, error) {
	hb, err := OPACKEncode(header)
	if err != nil {
		return nil, err
	}
	if len(hb) > 255 {
		return nil, fmt.Errorf("hds: header too long (%d bytes)", len(hb))
	}
	mb, err := OPACKEncode(message)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 0, 1+len(hb)+len(mb))
	payload = append(payload, byte(len(hb)))
	payload = append(payload, hb...)
	payload = append(payload, mb...)
	return payload, nil
}

func has(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

// asInt64 coerces an OPACK-decoded numeric to int64.
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	default:
		return 0, false
	}
}
