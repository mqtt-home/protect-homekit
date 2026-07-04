package protect

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// The Protect realtime updates websocket sends binary messages made of two
// frames — an action frame followed by a data frame. Each frame starts with
// an 8-byte header:
//
//	offset 0: frame type (1 = action, 2 = payload)
//	offset 1: payload format (1 = JSON, 2 = UTF-8 string, 3 = buffer)
//	offset 2: deflated (1 = zlib-compressed payload)
//	offset 3: unknown
//	offset 4: payload length (uint32, big endian)
//
// This layout is not officially documented by Ubiquiti; it matches the format
// reverse-engineered by hjdhjd/unifi-protect and has been stable for years.
const frameHeaderLen = 8

const (
	frameTypeAction  byte = 1
	frameTypePayload byte = 2

	payloadFormatJSON byte = 1
)

// Action describes what changed: e.g. {action: "update", modelKey: "camera",
// id: "<cameraId>"}.
type Action struct {
	Action      string `json:"action"`
	ID          string `json:"id"`
	ModelKey    string `json:"modelKey"`
	NewUpdateID string `json:"newUpdateId"`
}

// UpdateMessage is one decoded websocket message: the action plus the raw
// JSON payload (a partial object of the model identified by the action).
type UpdateMessage struct {
	Action  Action
	Payload json.RawMessage
}

// DecodeUpdateMessage parses a raw binary websocket message into the action
// and its payload.
func DecodeUpdateMessage(data []byte) (*UpdateMessage, error) {
	actionType, actionBody, rest, err := decodeFrame(data)
	if err != nil {
		return nil, fmt.Errorf("action frame: %w", err)
	}
	if actionType != frameTypeAction {
		return nil, fmt.Errorf("expected action frame (1), got type %d", actionType)
	}

	payloadType, payloadBody, _, err := decodeFrame(rest)
	if err != nil {
		return nil, fmt.Errorf("payload frame: %w", err)
	}
	if payloadType != frameTypePayload {
		return nil, fmt.Errorf("expected payload frame (2), got type %d", payloadType)
	}

	var action Action
	if err := json.Unmarshal(actionBody, &action); err != nil {
		return nil, fmt.Errorf("parsing action JSON: %w", err)
	}

	return &UpdateMessage{Action: action, Payload: payloadBody}, nil
}

// decodeFrame reads one header+payload frame and returns the frame type, the
// (inflated) payload and the remaining bytes.
func decodeFrame(data []byte) (frameType byte, payload []byte, rest []byte, err error) {
	if len(data) < frameHeaderLen {
		return 0, nil, nil, fmt.Errorf("frame too short: %d bytes", len(data))
	}

	frameType = data[0]
	deflated := data[2] == 1
	size := binary.BigEndian.Uint32(data[4:8])

	if uint32(len(data)-frameHeaderLen) < size {
		return 0, nil, nil, fmt.Errorf("frame payload truncated: header says %d, have %d", size, len(data)-frameHeaderLen)
	}

	payload = data[frameHeaderLen : frameHeaderLen+int(size)]
	rest = data[frameHeaderLen+int(size):]

	if deflated {
		r, zerr := zlib.NewReader(bytes.NewReader(payload))
		if zerr != nil {
			return 0, nil, nil, fmt.Errorf("zlib: %w", zerr)
		}
		defer r.Close()
		payload, zerr = io.ReadAll(r)
		if zerr != nil {
			return 0, nil, nil, fmt.Errorf("inflating payload: %w", zerr)
		}
	}

	return frameType, payload, rest, nil
}
