// OPACK is Apple's compact binary serialization used inside the HomeKit Data
// Stream (HDS) transport that carries HomeKit Secure Video recordings. Each
// value is prefixed by a single tag byte; small integers, short strings and
// short byte slices embed their length (or value) in the tag itself. All
// multi-byte integers and floats are little-endian, except UUIDs which are
// big-endian.
//
// This implementation covers the subset HDS uses: nil, booleans, integers,
// float64, UTF-8 strings, byte slices, arrays and dictionaries. It never emits
// compression back-references (spec-compliant — they are optional on the
// sender side) but does resolve them on decode, since a controller may use
// them. The tag table and encoder thresholds mirror HAP-NodeJS'
// DataStreamParser.
package hksv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Int64 forces a value to be OPACK-encoded as a 64-bit integer (tag 0x33)
// regardless of magnitude. HDS message headers require the "id" and "status"
// fields to be int64.
type Int64 int64

// KV is one ordered dictionary entry. Ordered dictionaries (Dict) keep a
// deterministic wire layout, which keeps encoding testable; the controller
// treats dictionaries as unordered.
type KV struct {
	K string
	V any
}

// Dict is an ordered OPACK dictionary used when building outgoing messages.
type Dict []KV

// Get returns the value for key k and whether it was present.
func (d Dict) Get(k string) (any, bool) {
	for _, kv := range d {
		if kv.K == k {
			return kv.V, true
		}
	}
	return nil, false
}

// OPACK tag bytes (subset). See package doc for the full scheme.
const (
	tagTrue        = 0x01
	tagFalse       = 0x02
	tagTerminator  = 0x03
	tagNull        = 0x04
	tagUUID        = 0x05
	tagDate        = 0x06
	tagIntMinusOne = 0x07
	tagIntRange0   = 0x08 // 0x08..0x2F encode integers 0..39
	tagIntRange39  = 0x2F
	tagInt8        = 0x30
	tagInt16       = 0x31
	tagInt32       = 0x32
	tagInt64       = 0x33
	tagFloat32     = 0x35
	tagFloat64     = 0x36
	tagUTF8Range0  = 0x40 // 0x40..0x60 embed string length 0..32
	tagUTF8Range32 = 0x60
	tagUTF8Len8    = 0x61
	tagUTF8Len16   = 0x62
	tagUTF8Len32   = 0x63
	tagUTF8Len64   = 0x64
	tagUTF8NulTerm = 0x6F
	tagDataRange0  = 0x70 // 0x70..0x90 embed data length 0..32
	tagDataRange32 = 0x90
	tagDataLen8    = 0x91
	tagDataLen16   = 0x92
	tagDataLen32   = 0x93
	tagDataLen64   = 0x94
	tagDataTerm    = 0x9F
	tagComprStart  = 0xA0 // 0xA0..0xCF back-reference to a prior value
	tagComprStop   = 0xCF
	tagArrRange0   = 0xD0 // 0xD0..0xDE fixed-count arrays 0..14
	tagArrRange14  = 0xDE
	tagArrTerm     = 0xDF
	tagDictRange0  = 0xE0 // 0xE0..0xEE fixed-count dictionaries 0..14
	tagDictRange14 = 0xEE
	tagDictTerm    = 0xEF
)

// OPACKEncode serializes v to OPACK. Supported Go types: nil, bool, string,
// []byte, all int/uint kinds, Int64, float32/float64, []any, Dict and
// map[string]any.
func OPACKEncode(v any) ([]byte, error) {
	var buf []byte
	b, err := opackEncode(buf, v)
	return b, err
}

func opackEncode(buf []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return append(buf, tagNull), nil
	case bool:
		if t {
			return append(buf, tagTrue), nil
		}
		return append(buf, tagFalse), nil
	case Int64:
		return encodeInt64Sized(buf, int64(t)), nil
	case int:
		return encodeNumber(buf, int64(t)), nil
	case int8:
		return encodeNumber(buf, int64(t)), nil
	case int16:
		return encodeNumber(buf, int64(t)), nil
	case int32:
		return encodeNumber(buf, int64(t)), nil
	case int64:
		return encodeNumber(buf, t), nil
	case uint8:
		return encodeNumber(buf, int64(t)), nil
	case uint16:
		return encodeNumber(buf, int64(t)), nil
	case uint32:
		return encodeNumber(buf, int64(t)), nil
	case uint:
		return encodeNumber(buf, int64(t)), nil
	case uint64:
		return encodeNumber(buf, int64(t)), nil
	case float32:
		buf = append(buf, tagFloat32)
		return binary.LittleEndian.AppendUint32(buf, math.Float32bits(t)), nil
	case float64:
		buf = append(buf, tagFloat64)
		return binary.LittleEndian.AppendUint64(buf, math.Float64bits(t)), nil
	case string:
		return encodeString(buf, t), nil
	case []byte:
		return encodeData(buf, t), nil
	case []any:
		return encodeArray(buf, t)
	case Dict:
		return encodeDict(buf, t)
	case map[string]any:
		d := make(Dict, 0, len(t))
		for k, val := range t {
			d = append(d, KV{k, val})
		}
		return encodeDict(buf, d)
	default:
		return nil, fmt.Errorf("opack: unsupported type %T", v)
	}
}

// encodeNumber picks the smallest integer tag that fits n, matching the
// encoder in the reference implementation.
func encodeNumber(buf []byte, n int64) []byte {
	switch {
	case n == -1:
		return append(buf, tagIntMinusOne)
	case n >= 0 && n <= 39:
		return append(buf, byte(tagIntRange0+n))
	case n >= math.MinInt8 && n <= math.MaxInt8:
		return append(buf, tagInt8, byte(n))
	case n >= math.MinInt16 && n <= math.MaxInt16:
		buf = append(buf, tagInt16)
		return binary.LittleEndian.AppendUint16(buf, uint16(n))
	case n >= math.MinInt32 && n <= math.MaxInt32:
		buf = append(buf, tagInt32)
		return binary.LittleEndian.AppendUint32(buf, uint32(n))
	default:
		return encodeInt64Sized(buf, n)
	}
}

func encodeInt64Sized(buf []byte, n int64) []byte {
	buf = append(buf, tagInt64)
	return binary.LittleEndian.AppendUint64(buf, uint64(n))
}

func encodeString(buf []byte, s string) []byte {
	n := len(s)
	switch {
	case n <= 32:
		buf = append(buf, byte(tagUTF8Range0+n))
	case n <= math.MaxUint8:
		buf = append(buf, tagUTF8Len8, byte(n))
	case n <= math.MaxUint16:
		buf = append(buf, tagUTF8Len16)
		buf = binary.LittleEndian.AppendUint16(buf, uint16(n))
	case n <= math.MaxUint32:
		buf = append(buf, tagUTF8Len32)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(n))
	default:
		buf = append(buf, tagUTF8Len64)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(n))
	}
	return append(buf, s...)
}

func encodeData(buf, d []byte) []byte {
	n := len(d)
	switch {
	case n <= 32:
		buf = append(buf, byte(tagDataRange0+n))
	case n <= math.MaxUint8:
		buf = append(buf, tagDataLen8, byte(n))
	case n <= math.MaxUint16:
		buf = append(buf, tagDataLen16)
		buf = binary.LittleEndian.AppendUint16(buf, uint16(n))
	case n <= math.MaxUint32:
		buf = append(buf, tagDataLen32)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(n))
	default:
		buf = append(buf, tagDataLen64)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(n))
	}
	return append(buf, d...)
}

func encodeArray(buf []byte, arr []any) ([]byte, error) {
	if len(arr) <= 12 {
		buf = append(buf, byte(tagArrRange0+len(arr)))
	} else {
		buf = append(buf, tagArrTerm)
	}
	var err error
	for _, e := range arr {
		if buf, err = opackEncode(buf, e); err != nil {
			return nil, err
		}
	}
	if len(arr) > 12 {
		buf = append(buf, tagTerminator)
	}
	return buf, nil
}

func encodeDict(buf []byte, d Dict) ([]byte, error) {
	if len(d) <= 14 {
		buf = append(buf, byte(tagDictRange0+len(d)))
	} else {
		buf = append(buf, tagDictTerm)
	}
	var err error
	for _, kv := range d {
		buf = encodeString(buf, kv.K)
		if buf, err = opackEncode(buf, kv.V); err != nil {
			return nil, err
		}
	}
	if len(d) > 14 {
		buf = append(buf, tagTerminator)
	}
	return buf, nil
}

// terminator is the sentinel decode returns for tag 0x03, used to end
// terminated arrays and dictionaries.
type terminatorT struct{}

var terminator = terminatorT{}

var errTruncated = errors.New("opack: truncated input")

// opackDecoder tracks decoded leaf values so compression back-references
// (0xA0..0xCF) can be resolved, exactly as the reference decoder does.
type opackDecoder struct {
	b       []byte
	pos     int
	tracked []any
}

// OPACKDecode parses a single OPACK value. Dictionaries decode to
// map[string]any, arrays to []any, integers to int64, strings to string, byte
// slices to []byte.
func OPACKDecode(b []byte) (any, error) {
	d := &opackDecoder{b: b}
	v, err := d.decode()
	if err != nil {
		return nil, err
	}
	if v == terminator {
		return nil, errors.New("opack: unexpected terminator at top level")
	}
	return v, nil
}

func (d *opackDecoder) u8() (byte, error) {
	if d.pos >= len(d.b) {
		return 0, errTruncated
	}
	v := d.b[d.pos]
	d.pos++
	return v, nil
}

func (d *opackDecoder) take(n int) ([]byte, error) {
	if n < 0 || d.pos+n > len(d.b) {
		return nil, errTruncated
	}
	v := d.b[d.pos : d.pos+n]
	d.pos += n
	return v, nil
}

func (d *opackDecoder) track(v any) any {
	d.tracked = append(d.tracked, v)
	return v
}

func (d *opackDecoder) decode() (any, error) {
	tag, err := d.u8()
	if err != nil {
		return nil, err
	}

	switch {
	case tag == tagTerminator:
		return terminator, nil
	case tag == tagNull:
		return nil, nil
	case tag == tagTrue:
		return d.track(true), nil
	case tag == tagFalse:
		return d.track(false), nil
	case tag == tagIntMinusOne:
		return d.track(int64(-1)), nil
	case tag >= tagIntRange0 && tag <= tagIntRange39:
		return d.track(int64(tag - tagIntRange0)), nil
	case tag == tagInt8:
		v, err := d.take(1)
		if err != nil {
			return nil, err
		}
		return d.track(int64(int8(v[0]))), nil
	case tag == tagInt16:
		v, err := d.take(2)
		if err != nil {
			return nil, err
		}
		return d.track(int64(int16(binary.LittleEndian.Uint16(v)))), nil
	case tag == tagInt32:
		v, err := d.take(4)
		if err != nil {
			return nil, err
		}
		return d.track(int64(int32(binary.LittleEndian.Uint32(v)))), nil
	case tag == tagInt64:
		v, err := d.take(8)
		if err != nil {
			return nil, err
		}
		return d.track(int64(binary.LittleEndian.Uint64(v))), nil
	case tag == tagFloat32:
		v, err := d.take(4)
		if err != nil {
			return nil, err
		}
		return d.track(float64(math.Float32frombits(binary.LittleEndian.Uint32(v)))), nil
	case tag == tagFloat64:
		v, err := d.take(8)
		if err != nil {
			return nil, err
		}
		return d.track(math.Float64frombits(binary.LittleEndian.Uint64(v))), nil
	case tag == tagUUID:
		v, err := d.take(16)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 16)
		copy(out, v)
		return d.track(out), nil
	case tag == tagDate:
		v, err := d.take(8)
		if err != nil {
			return nil, err
		}
		return d.track(math.Float64frombits(binary.LittleEndian.Uint64(v))), nil
	case tag >= tagUTF8Range0 && tag <= tagUTF8Range32:
		return d.readString(int(tag - tagUTF8Range0))
	case tag == tagUTF8Len8:
		return d.readStringN(1)
	case tag == tagUTF8Len16:
		return d.readStringN(2)
	case tag == tagUTF8Len32:
		return d.readStringN(4)
	case tag == tagUTF8Len64:
		return d.readStringN(8)
	case tag == tagUTF8NulTerm:
		return d.readUntil(0x00, true)
	case tag == tagDataTerm:
		return d.readUntil(tagTerminator, false)
	case tag >= tagDataRange0 && tag <= tagDataRange32:
		return d.readData(int(tag - tagDataRange0))
	case tag == tagDataLen8:
		return d.readDataN(1)
	case tag == tagDataLen16:
		return d.readDataN(2)
	case tag == tagDataLen32:
		return d.readDataN(4)
	case tag == tagDataLen64:
		return d.readDataN(8)
	case tag >= tagComprStart && tag <= tagComprStop:
		idx := int(tag - tagComprStart)
		if idx >= len(d.tracked) {
			return nil, fmt.Errorf("opack: compression index %d out of range", idx)
		}
		return d.tracked[idx], nil
	case tag >= tagArrRange0 && tag <= tagArrRange14:
		return d.readArray(int(tag - tagArrRange0))
	case tag == tagArrTerm:
		return d.readArrayTerminated()
	case tag >= tagDictRange0 && tag <= tagDictRange14:
		return d.readDict(int(tag - tagDictRange0))
	case tag == tagDictTerm:
		return d.readDictTerminated()
	default:
		return nil, fmt.Errorf("opack: unknown tag 0x%02x at offset %d", tag, d.pos-1)
	}
}

func (d *opackDecoder) lenN(n int) (int, error) {
	v, err := d.take(n)
	if err != nil {
		return 0, err
	}
	switch n {
	case 1:
		return int(v[0]), nil
	case 2:
		return int(binary.LittleEndian.Uint16(v)), nil
	case 4:
		return int(binary.LittleEndian.Uint32(v)), nil
	case 8:
		return int(binary.LittleEndian.Uint64(v)), nil
	}
	return 0, fmt.Errorf("opack: bad length size %d", n)
}

func (d *opackDecoder) readString(n int) (any, error) {
	v, err := d.take(n)
	if err != nil {
		return nil, err
	}
	return d.track(string(v)), nil
}

func (d *opackDecoder) readStringN(lenBytes int) (any, error) {
	n, err := d.lenN(lenBytes)
	if err != nil {
		return nil, err
	}
	return d.readString(n)
}

// readUntil consumes bytes up to (and discarding) the delimiter. asString
// controls whether the result is tracked as a string or a byte slice.
func (d *opackDecoder) readUntil(delim byte, asString bool) (any, error) {
	start := d.pos
	for d.pos < len(d.b) {
		if d.b[d.pos] == delim {
			raw := d.b[start:d.pos]
			d.pos++ // skip delimiter
			if asString {
				return d.track(string(raw)), nil
			}
			out := make([]byte, len(raw))
			copy(out, raw)
			return d.track(out), nil
		}
		d.pos++
	}
	return nil, errTruncated
}

func (d *opackDecoder) readData(n int) (any, error) {
	v, err := d.take(n)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, v)
	return d.track(out), nil
}

func (d *opackDecoder) readDataN(lenBytes int) (any, error) {
	n, err := d.lenN(lenBytes)
	if err != nil {
		return nil, err
	}
	return d.readData(n)
}

func (d *opackDecoder) readArray(n int) (any, error) {
	arr := make([]any, 0, n)
	for range n {
		e, err := d.decode()
		if err != nil {
			return nil, err
		}
		if e == terminator {
			return nil, errors.New("opack: unexpected terminator in array")
		}
		arr = append(arr, e)
	}
	return arr, nil
}

func (d *opackDecoder) readArrayTerminated() (any, error) {
	arr := []any{}
	for {
		e, err := d.decode()
		if err != nil {
			return nil, err
		}
		if e == terminator {
			return arr, nil
		}
		arr = append(arr, e)
	}
}

func (d *opackDecoder) readDict(n int) (any, error) {
	m := make(map[string]any, n)
	for range n {
		if err := d.decodePair(m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (d *opackDecoder) readDictTerminated() (any, error) {
	m := map[string]any{}
	for {
		key, err := d.decode()
		if err != nil {
			return nil, err
		}
		if key == terminator {
			return m, nil
		}
		ks, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("opack: dictionary key is %T, want string", key)
		}
		val, err := d.decode()
		if err != nil {
			return nil, err
		}
		m[ks] = val
	}
}

func (d *opackDecoder) decodePair(m map[string]any) error {
	key, err := d.decode()
	if err != nil {
		return err
	}
	ks, ok := key.(string)
	if !ok {
		return fmt.Errorf("opack: dictionary key is %T, want string", key)
	}
	val, err := d.decode()
	if err != nil {
		return err
	}
	m[ks] = val
	return nil
}
