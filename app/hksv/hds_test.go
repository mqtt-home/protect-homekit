package hksv

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestDeriveHDSKeysDeterministic(t *testing.T) {
	var shared [32]byte
	for i := range shared {
		shared[i] = byte(i)
	}
	cSalt := bytes.Repeat([]byte{0xAA}, 32)
	aSalt := bytes.Repeat([]byte{0xBB}, 32)

	k1, err := deriveHDSKeys(shared, cSalt, aSalt)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := deriveHDSKeys(shared, cSalt, aSalt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1.accessoryToController, k2.accessoryToController) ||
		!bytes.Equal(k1.controllerToAccessory, k2.controllerToAccessory) {
		t.Fatal("derivation not deterministic")
	}
	if bytes.Equal(k1.accessoryToController, k1.controllerToAccessory) {
		t.Fatal("directional keys must differ")
	}
	if len(k1.accessoryToController) != 32 {
		t.Fatalf("key len = %d", len(k1.accessoryToController))
	}
	// Salt order matters: swapping salts must change the keys.
	k3, _ := deriveHDSKeys(shared, aSalt, cSalt)
	if bytes.Equal(k1.accessoryToController, k3.accessoryToController) {
		t.Fatal("salt order should affect derivation")
	}
}

// pairedConns returns two hdsConns wired together over net.Pipe with mirrored
// directional keys, so a frame written by one decrypts on the other.
func pairedConns(t *testing.T) (accessory, controller *hdsConn, closeFn func()) {
	t.Helper()
	var shared [32]byte
	for i := range shared {
		shared[i] = byte(i + 1)
	}
	keys, err := deriveHDSKeys(shared, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	acc, err := newHDSConn(a, keys)
	if err != nil {
		t.Fatal(err)
	}
	// The controller side mirrors the key roles.
	ctrl, err := newHDSConn(b, hdsKeys{
		accessoryToController: keys.controllerToAccessory,
		controllerToAccessory: keys.accessoryToController,
	})
	if err != nil {
		t.Fatal(err)
	}
	return acc, ctrl, func() { a.Close(); b.Close() }
}

func TestHDSFrameRoundTrip(t *testing.T) {
	acc, ctrl, done := pairedConns(t)
	defer done()

	payloads := [][]byte{
		[]byte("first frame"),
		bytes.Repeat([]byte{0x5A}, 5000), // spans multiple TCP reads
		{},                               // empty payload
	}

	go func() {
		for _, p := range payloads {
			if err := acc.writeFrame(p); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()

	for i, want := range payloads {
		got, err := ctrl.readFrame()
		if err != nil {
			t.Fatalf("frame %d read: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d: got %d bytes, want %d", i, len(got), len(want))
		}
	}
}

func TestHDSNonceCountersAdvance(t *testing.T) {
	acc, ctrl, done := pairedConns(t)
	defer done()

	// Two frames in a row exercise counter advancement; a stale counter would
	// fail the second decrypt.
	go func() {
		_ = acc.writeFrame([]byte("one"))
		_ = acc.writeFrame([]byte("two"))
	}()
	if _, err := ctrl.readFrame(); err != nil {
		t.Fatal(err)
	}
	got, err := ctrl.readFrame()
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if string(got) != "two" {
		t.Fatalf("got %q", got)
	}
	if acc.sendCtr != 2 || ctrl.recvCtr != 2 {
		t.Fatalf("counters: send=%d recv=%d", acc.sendCtr, ctrl.recvCtr)
	}
}

func TestHDSTamperedFrameRejected(t *testing.T) {
	var shared [32]byte
	keys, _ := deriveHDSKeys(shared, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	var buf bytes.Buffer
	enc, _ := newHDSConn(&buf, keys)
	if err := enc.writeFrame([]byte("secret")); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	raw[len(raw)-1] ^= 0xFF // corrupt the auth tag

	dec, _ := newHDSConn(readWriter{Reader: bytes.NewReader(raw), Writer: io.Discard}, keys)
	if _, err := dec.readFrame(); err == nil {
		t.Fatal("expected decrypt failure on tampered frame")
	}
}

// readWriter adapts a separate reader and writer into an io.ReadWriter.
type readWriter struct {
	io.Reader
	io.Writer
}

func TestEncodeDecodeMessageRoundTrip(t *testing.T) {
	header := Dict{
		{"protocol", "dataSend"},
		{"request", "open"},
		{"id", Int64(1234)},
	}
	msg := Dict{
		{"target", "controller"},
		{"type", "ipcamera.recording"},
		{"streamId", 9},
	}
	payload, err := encodeMessage(header, msg)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decodeMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if dec.kind != msgRequest || dec.protocol != "dataSend" || dec.topic != "open" {
		t.Fatalf("header wrong: %+v", dec)
	}
	if dec.id != 1234 {
		t.Fatalf("id = %d", dec.id)
	}
	if dec.message["target"] != "controller" || dec.message["streamId"] != int64(9) {
		t.Fatalf("message wrong: %#v", dec.message)
	}
}

func TestDecodeMessageEmptyBody(t *testing.T) {
	header := Dict{{"protocol", "control"}, {"request", "hello"}, {"id", Int64(1)}}
	payload, err := encodeMessage(header, Dict{})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decodeMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if dec.protocol != "control" || dec.topic != "hello" || len(dec.message) != 0 {
		t.Fatalf("unexpected: %+v", dec)
	}
}
