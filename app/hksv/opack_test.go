package hksv

import (
	"bytes"
	"reflect"
	"testing"
)

func TestOPACKEncodeScalars(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []byte
	}{
		{"nil", nil, []byte{0x04}},
		{"true", true, []byte{0x01}},
		{"false", false, []byte{0x02}},
		{"minus-one", -1, []byte{0x07}},
		{"zero", 0, []byte{0x08}},
		{"thirty-nine", 39, []byte{0x2f}},
		{"forty (int8)", 40, []byte{0x30, 0x28}},
		{"neg (int8)", -2, []byte{0x30, 0xfe}},
		{"int16", 300, []byte{0x31, 0x2c, 0x01}},
		{"int32", 70000, []byte{0x32, 0x70, 0x11, 0x01, 0x00}},
		{"forced-int64", Int64(5), []byte{0x33, 0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{"empty string", "", []byte{0x40}},
		{"short string", "hello", append([]byte{0x45}, []byte("hello")...)},
		{"empty data", []byte{}, []byte{0x70}},
		{"short data", []byte{0xaa, 0xbb}, []byte{0x72, 0xaa, 0xbb}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := OPACKEncode(c.in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !bytes.Equal(got, c.want) {
				t.Fatalf("got %x, want %x", got, c.want)
			}
		})
	}
}

func TestOPACKStringLength8(t *testing.T) {
	s := string(bytes.Repeat([]byte("a"), 40))
	got, err := OPACKEncode(s)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != tagUTF8Len8 || got[1] != 40 {
		t.Fatalf("bad header: %x", got[:2])
	}
	if string(got[2:]) != s {
		t.Fatalf("payload mismatch")
	}
}

func TestOPACKDict(t *testing.T) {
	// An HDS control/hello response header.
	d := Dict{
		{"protocol", "control"},
		{"response", "hello"},
		{"id", Int64(42)},
		{"status", Int64(0)},
	}
	b, err := OPACKEncode(d)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 0xE0+4 {
		t.Fatalf("dict tag = %#x, want %#x", b[0], 0xE0+4)
	}
	got, err := OPACKDecode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("decoded type %T", got)
	}
	if m["protocol"] != "control" || m["response"] != "hello" {
		t.Fatalf("strings wrong: %#v", m)
	}
	if m["id"] != int64(42) || m["status"] != int64(0) {
		t.Fatalf("ints wrong: id=%#v status=%#v", m["id"], m["status"])
	}
}

func TestOPACKRoundTripNested(t *testing.T) {
	// Shaped like a dataSend "data" event message.
	msg := Dict{
		{"streamId", 7},
		{"packets", []any{
			Dict{
				{"data", []byte{0x00, 0x01, 0x02, 0x03}},
				{"metadata", Dict{
					{"dataType", "mediaInitialization"},
					{"dataSequenceNumber", 1},
					{"dataChunkSequenceNumber", 1},
					{"isLastDataChunk", true},
					{"dataTotalSize", 4},
				}},
			},
		}},
		{"endOfStream", false},
	}
	b, err := OPACKEncode(msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OPACKDecode(b)
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["streamId"] != int64(7) {
		t.Fatalf("streamId = %#v", m["streamId"])
	}
	packets := m["packets"].([]any)
	if len(packets) != 1 {
		t.Fatalf("packets len = %d", len(packets))
	}
	pkt := packets[0].(map[string]any)
	if !bytes.Equal(pkt["data"].([]byte), []byte{0x00, 0x01, 0x02, 0x03}) {
		t.Fatalf("data = %#v", pkt["data"])
	}
	meta := pkt["metadata"].(map[string]any)
	if meta["dataType"] != "mediaInitialization" || meta["isLastDataChunk"] != true {
		t.Fatalf("metadata = %#v", meta)
	}
}

func TestOPACKArrayTerminated(t *testing.T) {
	// 13 elements forces the terminated array form (0xDF ... 0x03).
	arr := make([]any, 13)
	for i := range arr {
		arr[i] = i
	}
	b, err := OPACKEncode(arr)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != tagArrTerm {
		t.Fatalf("tag = %#x, want %#x", b[0], tagArrTerm)
	}
	if b[len(b)-1] != tagTerminator {
		t.Fatalf("missing terminator")
	}
	got, err := OPACKDecode(b)
	if err != nil {
		t.Fatal(err)
	}
	out := got.([]any)
	if len(out) != 13 || out[12] != int64(12) {
		t.Fatalf("out = %#v", out)
	}
}

func TestOPACKDecodeCompression(t *testing.T) {
	// Hand-built: dict {"a": "value", "b": <compr 1>} where index 1 is "value".
	// Tracked order while decoding: [0]="a", [1]="value", [2]="b", ref -> "value".
	in := []byte{
		0xE0 + 2,  // dict, 2 pairs
		0x41, 'a', // key "a"
		0x45, 'v', 'a', 'l', 'u', 'e', // value "value"  (tracked index 1)
		0x41, 'b', // key "b"           (tracked index 2)
		0xA0 + 1, // compression ref index 1 -> "value"
	}
	got, err := OPACKDecode(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := got.(map[string]any)
	if m["a"] != "value" || m["b"] != "value" {
		t.Fatalf("compression not resolved: %#v", m)
	}
}

func TestOPACKDecodeTruncated(t *testing.T) {
	if _, err := OPACKDecode([]byte{tagInt32, 0x01}); err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestOPACKMapRoundTrip(t *testing.T) {
	in := map[string]any{"x": 1, "y": "z"}
	b, err := OPACKEncode(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OPACKDecode(b)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"x": int64(1), "y": "z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}
