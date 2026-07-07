package hksv

// Minimal HomeKit TLV8 codec for the HKSV configuration characteristics.
// Layout: each item is type(1) length(1) value(length). Values longer than 255
// bytes are split into consecutive fragments of the same type (all but the last
// exactly 255 bytes). Repeated items of the same type (lists) are separated by a
// zero-delimiter item (type 0, length 0). This mirrors HAP-NodeJS' tlv.encode /
// tlv.decode, which the HomeKit controller uses.

import (
	"encoding/binary"
	"fmt"
)

const tlvDelimiterType = 0x00

// tlvWriter accumulates TLV8 items.
type tlvWriter struct {
	buf []byte
}

func newTLV() *tlvWriter { return &tlvWriter{} }

// addByte writes a single-byte value.
func (w *tlvWriter) addByte(typ, v byte) *tlvWriter {
	return w.addBytes(typ, []byte{v})
}

// addUint16 writes a little-endian uint16.
func (w *tlvWriter) addUint16(typ byte, v uint16) *tlvWriter {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	return w.addBytes(typ, b[:])
}

// addInt32 writes a little-endian int32 (used for millisecond/bitrate fields).
func (w *tlvWriter) addInt32(typ byte, v int32) *tlvWriter {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return w.addBytes(typ, b[:])
}

// addBytes writes a value, fragmenting it across 255-byte items if needed.
func (w *tlvWriter) addBytes(typ byte, v []byte) *tlvWriter {
	if len(v) == 0 {
		w.buf = append(w.buf, typ, 0)
		return w
	}
	for len(v) > 0 {
		n := min(len(v), 255)
		w.buf = append(w.buf, typ, byte(n))
		w.buf = append(w.buf, v[:n]...)
		v = v[n:]
	}
	return w
}

// addNested writes a sub-TLV as the value of typ.
func (w *tlvWriter) addNested(typ byte, sub *tlvWriter) *tlvWriter {
	return w.addBytes(typ, sub.bytes())
}

// addList writes each item under typ, separated by zero-delimiters (the HomeKit
// list encoding).
func (w *tlvWriter) addList(typ byte, items [][]byte) *tlvWriter {
	if len(items) == 0 {
		w.buf = append(w.buf, typ, 0)
		return w
	}
	for i, item := range items {
		if i > 0 {
			w.buf = append(w.buf, tlvDelimiterType, 0)
		}
		w.addBytes(typ, item)
	}
	return w
}

func (w *tlvWriter) bytes() []byte { return w.buf }

// tlvMap holds decoded TLV items keyed by type. Fragmented values (consecutive
// items of the same type) are concatenated; zero-delimiters reset the run so
// list items stay separate in list().
type tlvMap struct {
	first map[byte][]byte
	lists map[byte][][]byte
}

// first returns the first (fragmentation-merged) value for a type.
func (m tlvMap) get(typ byte) ([]byte, bool) {
	v, ok := m.first[typ]
	return v, ok
}

// list returns every value written for a type (list decoding).
func (m tlvMap) list(typ byte) [][]byte { return m.lists[typ] }

// byteVal returns the first value's leading byte, or def if absent/empty.
func (m tlvMap) byteVal(typ byte, def byte) byte {
	if v, ok := m.first[typ]; ok && len(v) > 0 {
		return v[0]
	}
	return def
}

// uint16 returns the first value decoded as little-endian uint16.
func (m tlvMap) uint16(typ byte, def uint16) uint16 {
	if v, ok := m.first[typ]; ok && len(v) >= 2 {
		return binary.LittleEndian.Uint16(v)
	}
	return def
}

// int32 returns the first value decoded as little-endian int32.
func (m tlvMap) int32(typ byte, def int32) int32 {
	if v, ok := m.first[typ]; ok && len(v) >= 4 {
		return int32(binary.LittleEndian.Uint32(v))
	}
	return def
}

// parseTLV decodes a TLV8 buffer.
func parseTLV(b []byte) (tlvMap, error) {
	m := tlvMap{first: map[byte][]byte{}, lists: map[byte][][]byte{}}
	i := 0
	prevType := -1      // type of the immediately preceding item
	prevWas255 := false // whether that item's fragment was a full 255 bytes
	for i < len(b) {
		if i+2 > len(b) {
			return tlvMap{}, fmt.Errorf("tlv: truncated header at %d", i)
		}
		typ := b[i]
		l := int(b[i+1])
		i += 2
		if i+l > len(b) {
			return tlvMap{}, fmt.Errorf("tlv: value overruns buffer at %d", i)
		}
		val := b[i : i+l]
		i += l

		if typ == tlvDelimiterType && l == 0 {
			prevType = -1 // delimiter ends any fragmentation run
			prevWas255 = false
			continue
		}

		if int(typ) == prevType && prevWas255 {
			// Continuation fragment of the previous value.
			m.first[typ] = append(m.first[typ], val...)
			last := len(m.lists[typ]) - 1
			m.lists[typ][last] = append(m.lists[typ][last], val...)
		} else {
			if _, ok := m.first[typ]; !ok {
				m.first[typ] = append([]byte(nil), val...)
			}
			m.lists[typ] = append(m.lists[typ], append([]byte(nil), val...))
		}
		prevType = int(typ)
		prevWas255 = l == 255
	}
	return m, nil
}
