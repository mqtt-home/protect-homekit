package protect

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

func frame(t *testing.T, frameType, format byte, deflate bool, payload []byte) []byte {
	t.Helper()

	body := payload
	if deflate {
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		body = buf.Bytes()
	}

	header := make([]byte, frameHeaderLen)
	header[0] = frameType
	header[1] = format
	if deflate {
		header[2] = 1
	}
	binary.BigEndian.PutUint32(header[4:8], uint32(len(body)))
	return append(header, body...)
}

func TestDecodeUpdateMessage(t *testing.T) {
	action := []byte(`{"action":"update","id":"cam1","modelKey":"camera","newUpdateId":"u1"}`)
	payload := []byte(`{"isMotionDetected":true}`)

	msg := append(frame(t, frameTypeAction, payloadFormatJSON, false, action),
		frame(t, frameTypePayload, payloadFormatJSON, false, payload)...)

	decoded, err := DecodeUpdateMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Action.Action != "update" || decoded.Action.ModelKey != "camera" || decoded.Action.ID != "cam1" {
		t.Errorf("unexpected action: %+v", decoded.Action)
	}
	if string(decoded.Payload) != string(payload) {
		t.Errorf("payload = %s, want %s", decoded.Payload, payload)
	}
}

func TestDecodeUpdateMessageDeflated(t *testing.T) {
	action := []byte(`{"action":"update","id":"cam2","modelKey":"camera"}`)
	payload := []byte(`{"lastRing":1751630000000}`)

	msg := append(frame(t, frameTypeAction, payloadFormatJSON, true, action),
		frame(t, frameTypePayload, payloadFormatJSON, true, payload)...)

	decoded, err := DecodeUpdateMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Action.ID != "cam2" {
		t.Errorf("id = %s, want cam2", decoded.Action.ID)
	}
	if string(decoded.Payload) != string(payload) {
		t.Errorf("payload = %s, want %s", decoded.Payload, payload)
	}
}

func TestDecodeUpdateMessageErrors(t *testing.T) {
	// Too short.
	if _, err := DecodeUpdateMessage([]byte{1, 2, 3}); err == nil {
		t.Error("expected error for short message")
	}

	// Truncated payload.
	action := frame(t, frameTypeAction, payloadFormatJSON, false, []byte(`{"action":"update"}`))
	if _, err := DecodeUpdateMessage(action[:len(action)-2]); err == nil {
		t.Error("expected error for truncated frame")
	}

	// Missing payload frame.
	if _, err := DecodeUpdateMessage(action); err == nil {
		t.Error("expected error for missing payload frame")
	}

	// Wrong frame order.
	payload := frame(t, frameTypePayload, payloadFormatJSON, false, []byte(`{}`))
	if _, err := DecodeUpdateMessage(append(payload, action...)); err == nil {
		t.Error("expected error for wrong frame order")
	}
}

func TestCameraUnmarshalKeepsRawChannels(t *testing.T) {
	data := []byte(`{
		"id": "abc",
		"name": "Front",
		"channels": [
			{"id": 0, "name": "High", "enabled": true, "isRtspEnabled": false, "someUnknownField": 42}
		]
	}`)

	var cam Camera
	if err := cam.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}
	if cam.Name != "Front" || len(cam.Channels) != 1 || cam.Channels[0].Name != "High" {
		t.Errorf("typed fields not parsed: %+v", cam)
	}
	if !bytes.Contains(cam.ChannelsRaw, []byte("someUnknownField")) {
		t.Errorf("raw channels lost unknown fields: %s", cam.ChannelsRaw)
	}
}
