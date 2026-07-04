package protect

import "encoding/json"

// Bootstrap is the subset of GET /proxy/protect/api/bootstrap this bridge
// needs. LastUpdateID feeds the realtime updates websocket.
type Bootstrap struct {
	AuthUserID   string   `json:"authUserId"`
	LastUpdateID string   `json:"lastUpdateId"`
	NVR          NVR      `json:"nvr"`
	Cameras      []Camera `json:"cameras"`
}

type NVR struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mac      string `json:"mac"`
	Version  string `json:"version"`
	Firmware string `json:"firmwareVersion"`
	Ports    Ports  `json:"ports"`
}

type Ports struct {
	RTSP  int `json:"rtsp"`
	RTSPS int `json:"rtsps"`
}

type Camera struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	Mac              string `json:"mac"`
	Host             string `json:"host"`
	State            string `json:"state"` // "CONNECTED" when online
	FirmwareVersion  string `json:"firmwareVersion"`
	IsMicEnabled     bool   `json:"isMicEnabled"`
	IsMotionDetected bool   `json:"isMotionDetected"`
	// VideoCodec is "h264" or "h265" (Protect "enhanced encoding"). HomeKit
	// streaming only supports H.264.
	VideoCodec   string       `json:"videoCodec"`
	LastMotion   int64        `json:"lastMotion"`
	LastRing     int64        `json:"lastRing"`
	FeatureFlags FeatureFlags `json:"featureFlags"`
	Channels     []Channel    `json:"channels"`
	// ChannelsRaw preserves the untouched channel objects so an RTSP-enable
	// PATCH can send them back without dropping fields this bridge doesn't
	// model.
	ChannelsRaw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps a raw copy of "channels" next to the typed one.
func (c *Camera) UnmarshalJSON(data []byte) error {
	type alias Camera
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	var raw struct {
		Channels json.RawMessage `json:"channels"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = Camera(a)
	c.ChannelsRaw = raw.Channels
	return nil
}

func (c Camera) IsDoorbell() bool {
	return c.FeatureFlags.IsDoorbell
}

// UsesH264 reports whether the camera streams HomeKit-compatible H.264.
// An empty codec (older firmware without the field) is assumed to be H.264.
func (c Camera) UsesH264() bool {
	return c.VideoCodec == "" || c.VideoCodec == "h264"
}

func (c Camera) Connected() bool {
	return c.State == "CONNECTED"
}

type FeatureFlags struct {
	IsDoorbell bool `json:"isDoorbell"`
	HasMic     bool `json:"hasMic"`
	HasSpeaker bool `json:"hasSpeaker"`
}

// Channel is one of the camera's fixed-quality streams (High/Medium/Low).
type Channel struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
	IsRtspEnabled bool   `json:"isRtspEnabled"`
	RtspAlias     string `json:"rtspAlias"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
	Bitrate       int    `json:"bitrate"`
	// IDRInterval is the keyframe interval in seconds. It bounds how long a
	// new viewer waits for the first renderable frame on a copied stream.
	IDRInterval int `json:"idrInterval"`
}

// CameraPatch is a partial camera object received over the updates websocket.
// Pointer fields distinguish "not part of this update" from zero values.
type CameraPatch struct {
	State            *string `json:"state"`
	IsMotionDetected *bool   `json:"isMotionDetected"`
	LastMotion       *int64  `json:"lastMotion"`
	LastRing         *int64  `json:"lastRing"`
}
