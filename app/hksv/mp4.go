package hksv

// Fragmented-MP4 splitting. HKSV expects the recording as an initialization
// segment (ftyp + moov) followed by media fragments (each a moof + mdat that
// begins with a keyframe). ffmpeg, configured with empty_moov + frag_keyframe,
// emits exactly this stream; splitFragments carves the byte stream at box
// boundaries so each fragment can be handed to the data-stream sender.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// boxHeaderSize is the minimum MP4 box header: 4-byte size + 4-byte type.
const boxHeaderSize = 8

// mp4Box is one parsed top-level box with its full on-wire bytes.
type mp4Box struct {
	typ string
	raw []byte
}

// boxReader reads consecutive top-level MP4 boxes from a stream.
type boxReader struct {
	r *bufio.Reader
}

func newBoxReader(r io.Reader) *boxReader {
	return &boxReader{r: bufio.NewReaderSize(r, 64*1024)}
}

// next reads the next box. It returns io.EOF cleanly at a box boundary.
func (br *boxReader) next() (mp4Box, error) {
	header := make([]byte, boxHeaderSize)
	if _, err := io.ReadFull(br.r, header); err != nil {
		return mp4Box{}, err // io.EOF here means a clean end between boxes
	}

	size := uint64(binary.BigEndian.Uint32(header[0:4]))
	typ := string(header[4:8])

	var raw []byte
	switch size {
	case 1:
		// 64-bit largesize follows the header.
		ext := make([]byte, 8)
		if _, err := io.ReadFull(br.r, ext); err != nil {
			return mp4Box{}, fmt.Errorf("mp4: read largesize: %w", err)
		}
		size = binary.BigEndian.Uint64(ext)
		if size < boxHeaderSize+8 {
			return mp4Box{}, fmt.Errorf("mp4: invalid largesize %d", size)
		}
		raw = make([]byte, size)
		copy(raw, header)
		copy(raw[boxHeaderSize:], ext)
		if _, err := io.ReadFull(br.r, raw[boxHeaderSize+8:]); err != nil {
			return mp4Box{}, fmt.Errorf("mp4: read box body: %w", err)
		}
	case 0:
		// Extends to end of stream; read it all.
		body, err := io.ReadAll(br.r)
		if err != nil {
			return mp4Box{}, err
		}
		raw = append(header, body...)
	default:
		if size < boxHeaderSize {
			return mp4Box{}, fmt.Errorf("mp4: invalid box size %d", size)
		}
		raw = make([]byte, size)
		copy(raw, header)
		if _, err := io.ReadFull(br.r, raw[boxHeaderSize:]); err != nil {
			return mp4Box{}, fmt.Errorf("mp4: read box body: %w", err)
		}
	}

	return mp4Box{typ: typ, raw: raw}, nil
}

// splitFragments reads an fMP4 stream, invoking onInit once with the
// initialization segment (all boxes up to and including moov) and onFragment
// for every subsequent media fragment. It returns nil at a clean EOF.
func splitFragments(r io.Reader, onInit func([]byte) error, onFragment func([]byte) error) error {
	br := newBoxReader(r)

	// Accumulate the initialization segment: ftyp (+ optional free) + moov.
	var initSeg []byte
	sawMoov := false
	for !sawMoov {
		box, err := br.next()
		if err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF // stream ended before init segment
			}
			return err
		}
		initSeg = append(initSeg, box.raw...)
		if box.typ == "moov" {
			sawMoov = true
		}
	}
	if err := onInit(initSeg); err != nil {
		return err
	}

	// Group the remaining boxes into fragments. A fragment is an optional styp
	// followed by a moof and its mdat(s); the next styp — or the next moof when
	// the current fragment already has one — starts a new fragment.
	var frag []byte
	fragHasMoof := false
	flush := func() error {
		if len(frag) == 0 {
			return nil
		}
		f := frag
		frag = nil
		fragHasMoof = false
		return onFragment(f)
	}

	for {
		box, err := br.next()
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
		switch box.typ {
		case "mfra", "skip", "free":
			// Tail/padding boxes are not media fragments; ignore them.
			continue
		case "styp":
			if err := flush(); err != nil {
				return err
			}
		case "moof":
			if fragHasMoof {
				if err := flush(); err != nil {
					return err
				}
			}
			fragHasMoof = true
		}
		frag = append(frag, box.raw...)
	}
}
