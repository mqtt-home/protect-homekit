package hksv

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// makeBox builds a minimal MP4 box with a 32-bit size.
func makeBox(typ string, payload []byte) []byte {
	size := boxHeaderSize + len(payload)
	b := make([]byte, boxHeaderSize)
	binary.BigEndian.PutUint32(b[0:4], uint32(size))
	copy(b[4:8], typ)
	return append(b, payload...)
}

func TestSplitFragments(t *testing.T) {
	ftyp := makeBox("ftyp", []byte("isom"))
	moov := makeBox("moov", bytes.Repeat([]byte{0x01}, 40))
	moof1 := makeBox("moof", bytes.Repeat([]byte{0x02}, 20))
	mdat1 := makeBox("mdat", bytes.Repeat([]byte{0x03}, 100))
	moof2 := makeBox("moof", bytes.Repeat([]byte{0x04}, 20))
	mdat2 := makeBox("mdat", bytes.Repeat([]byte{0x05}, 200))

	var stream []byte
	stream = append(stream, ftyp...)
	stream = append(stream, moov...)
	stream = append(stream, moof1...)
	stream = append(stream, mdat1...)
	stream = append(stream, moof2...)
	stream = append(stream, mdat2...)

	var init []byte
	var frags [][]byte
	err := splitFragments(bytes.NewReader(stream),
		func(b []byte) error { init = b; return nil },
		func(b []byte) error { frags = append(frags, b); return nil })
	if err != nil {
		t.Fatal(err)
	}

	wantInit := append(append([]byte{}, ftyp...), moov...)
	if !bytes.Equal(init, wantInit) {
		t.Fatalf("init segment wrong (%d bytes)", len(init))
	}
	if len(frags) != 2 {
		t.Fatalf("expected 2 fragments, got %d", len(frags))
	}
	if !bytes.Equal(frags[0], append(append([]byte{}, moof1...), mdat1...)) {
		t.Fatal("fragment 1 mismatch")
	}
	if !bytes.Equal(frags[1], append(append([]byte{}, moof2...), mdat2...)) {
		t.Fatal("fragment 2 mismatch")
	}
}

func TestSplitFragmentsWithStyp(t *testing.T) {
	// Some muxers prefix each segment with styp.
	ftyp := makeBox("ftyp", []byte("isom"))
	moov := makeBox("moov", []byte{0x01})
	styp1 := makeBox("styp", []byte("msdh"))
	moof1 := makeBox("moof", []byte{0x02})
	mdat1 := makeBox("mdat", []byte{0x03})
	styp2 := makeBox("styp", []byte("msdh"))
	moof2 := makeBox("moof", []byte{0x04})
	mdat2 := makeBox("mdat", []byte{0x05})

	stream := bytes.Join([][]byte{ftyp, moov, styp1, moof1, mdat1, styp2, moof2, mdat2}, nil)

	var frags [][]byte
	err := splitFragments(bytes.NewReader(stream),
		func(b []byte) error { return nil },
		func(b []byte) error { frags = append(frags, b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != 2 {
		t.Fatalf("expected 2 fragments, got %d", len(frags))
	}
	if !bytes.HasPrefix(frags[0][4:], []byte("styp")) {
		t.Fatalf("fragment should start with styp")
	}
}

func TestSplitFragmentsLargesize(t *testing.T) {
	// A moov using the 64-bit largesize form.
	payload := bytes.Repeat([]byte{0xAB}, 32)
	moov := make([]byte, boxHeaderSize+8+len(payload))
	binary.BigEndian.PutUint32(moov[0:4], 1) // size==1 -> largesize
	copy(moov[4:8], "moov")
	binary.BigEndian.PutUint64(moov[8:16], uint64(len(moov)))
	copy(moov[16:], payload)

	ftyp := makeBox("ftyp", []byte("isom"))
	moof := makeBox("moof", []byte{0x02})
	mdat := makeBox("mdat", []byte{0x03})
	stream := bytes.Join([][]byte{ftyp, moov, moof, mdat}, nil)

	var init []byte
	var frags [][]byte
	err := splitFragments(bytes.NewReader(stream),
		func(b []byte) error { init = b; return nil },
		func(b []byte) error { frags = append(frags, b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(init, append(append([]byte{}, ftyp...), moov...)) {
		t.Fatal("largesize init mismatch")
	}
	if len(frags) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(frags))
	}
}

func TestSplitFragmentsTruncatedInit(t *testing.T) {
	// ftyp but no moov -> unexpected EOF.
	stream := makeBox("ftyp", []byte("isom"))
	err := splitFragments(bytes.NewReader(stream),
		func(b []byte) error { return nil },
		func(b []byte) error { return nil })
	if err == nil {
		t.Fatal("expected error for missing moov")
	}
}
