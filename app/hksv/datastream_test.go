package hksv

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

// fakeRecorder yields a fixed set of fragments.
type fakeRecorder struct {
	frags []Fragment
	err   error
}

func (f *fakeRecorder) StartRecording(ctx context.Context) (<-chan Fragment, error) {
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan Fragment, len(f.frags))
	for _, fr := range f.frags {
		ch <- fr
	}
	close(ch)
	return ch, nil
}

// ctrlEnd wraps the controller side of a paired HDS connection for tests.
type ctrlEnd struct {
	hc *hdsConn
}

func (c *ctrlEnd) sendMsg(t *testing.T, header Dict, msg any) {
	t.Helper()
	payload, err := encodeMessage(header, msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.hc.writeFrame(payload); err != nil {
		t.Fatal(err)
	}
}

func (c *ctrlEnd) recvMsg(t *testing.T) *hdsMessage {
	t.Helper()
	payload, err := c.hc.readFrame()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	m, err := decodeMessage(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func startSession(t *testing.T, rec Recorder) (*ctrlEnd, func()) {
	t.Helper()
	var shared [32]byte
	for i := range shared {
		shared[i] = byte(i + 7)
	}
	keys, err := deriveHDSKeys(shared, bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	accConn, ctrlConn := net.Pipe()

	accHDS, _ := newHDSConn(accConn, keys)
	ctrlHDS, _ := newHDSConn(ctrlConn, hdsKeys{
		accessoryToController: keys.controllerToAccessory,
		controllerToAccessory: keys.accessoryToController,
	})

	sess := &dataStreamSession{
		conn:       accHDS,
		recorder:   rec,
		cameraName: "test",
		canRecord:  func() bool { return true },
		hasConfig:  func() bool { return true },
	}
	ctx, cancel := context.WithCancel(context.Background())
	go sess.run(ctx, accConn)

	return &ctrlEnd{hc: ctrlHDS}, func() {
		cancel()
		accConn.Close()
		ctrlConn.Close()
	}
}

func TestRecordingProtocolFlow(t *testing.T) {
	initSeg := []byte("INIT-ftyp-moov")
	bigFrag := bytes.Repeat([]byte{0xAB}, maxChunkSize+100) // forces 2 chunks
	lastFrag := []byte("final-moof-mdat")

	rec := &fakeRecorder{frags: []Fragment{
		{Data: initSeg},
		{Data: bigFrag},
		{Data: lastFrag, IsLast: true},
	}}

	ctrl, done := startSession(t, rec)
	defer done()

	// 1. hello
	ctrl.sendMsg(t, Dict{{"protocol", "control"}, {"request", "hello"}, {"id", Int64(1)}}, Dict{})
	resp := ctrl.recvMsg(t)
	if resp.protocol != "control" || resp.topic != "hello" || resp.kind != msgResponse || resp.id != 1 || resp.status != 0 {
		t.Fatalf("bad hello response: %+v", resp)
	}

	// 2. open
	ctrl.sendMsg(t, Dict{{"protocol", "dataSend"}, {"request", "open"}, {"id", Int64(2)}},
		Dict{{"target", "controller"}, {"type", "ipcamera.recording"}, {"streamId", 5}})
	openResp := ctrl.recvMsg(t)
	if openResp.topic != "open" || openResp.kind != msgResponse || openResp.status != 0 {
		t.Fatalf("bad open response: %+v", openResp)
	}
	if st, _ := asInt64(openResp.message["status"]); st != 0 {
		t.Fatalf("open message status = %v", openResp.message["status"])
	}

	// 3. collect data events, reassembling chunks per dataSequenceNumber.
	type frag struct {
		data     []byte
		dataType string
		eos      bool
	}
	frags := map[int64]*frag{}
	var order []int64
	sawEOS := false
	for !sawEOS {
		m := ctrl.recvMsg(t)
		if m.protocol != "dataSend" || m.topic != "data" || m.kind != msgEvent {
			t.Fatalf("expected data event, got %+v", m)
		}
		if sid, _ := asInt64(m.message["streamId"]); sid != 5 {
			t.Fatalf("streamId = %v", m.message["streamId"])
		}
		packets := m.message["packets"].([]any)
		pkt := packets[0].(map[string]any)
		meta := pkt["metadata"].(map[string]any)
		seq, _ := asInt64(meta["dataSequenceNumber"])
		chunkSeq, _ := asInt64(meta["dataChunkSequenceNumber"])
		if _, ok := frags[seq]; !ok {
			frags[seq] = &frag{dataType: meta["dataType"].(string)}
			order = append(order, seq)
			// dataTotalSize must be present only on the first chunk.
			if _, ok := meta["dataTotalSize"]; !ok {
				t.Fatalf("seq %d chunk 1 missing dataTotalSize", seq)
			}
		} else if _, ok := meta["dataTotalSize"]; ok {
			t.Fatalf("seq %d chunk %d must not carry dataTotalSize", seq, chunkSeq)
		}
		frags[seq].data = append(frags[seq].data, pkt["data"].([]byte)...)
		if eos, ok := m.message["endOfStream"].(bool); ok && eos {
			frags[seq].eos = true
			sawEOS = true
		}
	}

	if len(order) != 3 {
		t.Fatalf("expected 3 fragments, got %d", len(order))
	}
	if frags[1].dataType != "mediaInitialization" || !bytes.Equal(frags[1].data, initSeg) {
		t.Fatalf("init fragment wrong: %+v", frags[1])
	}
	if frags[2].dataType != "mediaFragment" || !bytes.Equal(frags[2].data, bigFrag) {
		t.Fatalf("big fragment reassembly wrong (len %d)", len(frags[2].data))
	}
	if frags[3].dataType != "mediaFragment" || !bytes.Equal(frags[3].data, lastFrag) || !frags[3].eos {
		t.Fatalf("last fragment wrong: eos=%v", frags[3].eos)
	}
}

func TestRecordingOpenRejectedWhenNotAllowed(t *testing.T) {
	var shared [32]byte
	keys, _ := deriveHDSKeys(shared, bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32))
	accConn, ctrlConn := net.Pipe()
	accHDS, _ := newHDSConn(accConn, keys)
	ctrlHDS, _ := newHDSConn(ctrlConn, hdsKeys{
		accessoryToController: keys.controllerToAccessory,
		controllerToAccessory: keys.accessoryToController,
	})
	sess := &dataStreamSession{
		conn:       accHDS,
		recorder:   &fakeRecorder{},
		cameraName: "test",
		canRecord:  func() bool { return false }, // recording disabled
		hasConfig:  func() bool { return true },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sess.run(ctx, accConn)
	defer accConn.Close()
	defer ctrlConn.Close()

	ctrl := &ctrlEnd{hc: ctrlHDS}
	ctrl.sendMsg(t, Dict{{"protocol", "control"}, {"request", "hello"}, {"id", Int64(1)}}, Dict{})
	ctrl.recvMsg(t) // hello response

	ctrl.sendMsg(t, Dict{{"protocol", "dataSend"}, {"request", "open"}, {"id", Int64(2)}},
		Dict{{"target", "controller"}, {"type", "ipcamera.recording"}, {"streamId", 5}})
	resp := ctrl.recvMsg(t)
	if resp.status != hdsStatusProtocolSpecificError {
		t.Fatalf("expected protocol-specific error, got status %d", resp.status)
	}
	if st, _ := asInt64(resp.message["status"]); st != reasonNotAllowed {
		t.Fatalf("expected NOT_ALLOWED reason, got %v", resp.message["status"])
	}
}

func TestServerListenerPort(t *testing.T) {
	var shared [32]byte
	keys, _ := deriveHDSKeys(shared, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	srv, err := NewServer(keys, &fakeRecorder{}, "cam", func() bool { return true }, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if srv.Port() <= 0 || srv.Port() > 65535 {
		t.Fatalf("invalid port %d", srv.Port())
	}
	// Serve should return promptly when no controller connects (hello timeout is
	// long, so cancel instead).
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	srv.Serve(ctx) // returns when ctx cancels the accept
}
