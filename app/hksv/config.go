package hksv

// Builders and parsers for the HKSV configuration characteristic values
// (SupportedDataStreamTransportConfiguration, SetupDataStreamTransport,
// Supported*RecordingConfiguration, SelectedCameraRecordingConfiguration). All
// are TLV8; the type IDs and value encodings follow the HKSV specification.

import "fmt"

// Data-stream transport TLV types.
const (
	transferTransportConfiguration = 1 // SupportedDataStreamTransportConfiguration outer
	transportTypeInner             = 1 // inner TRANSPORT_TYPE
	transportHomeKitDataStream     = 0 // HOMEKIT_DATA_STREAM

	setupCommandType   = 1 // SetupDataStreamTransport request: SESSION_COMMAND_TYPE
	setupTransportType = 2 // request: TRANSPORT_TYPE
	setupControllerKey = 3 // request: CONTROLLER_KEY_SALT

	setupStatus         = 1 // response: STATUS
	setupSessionParams  = 2 // response: TRANSPORT_TYPE_SESSION_PARAMETERS
	setupAccessoryKey   = 3 // response: ACCESSORY_KEY_SALT
	sessionParamTCPPort = 1 // session params: TCP_LISTENING_PORT

	commandStartSession = 0
	dataStreamStatusOK  = 0
)

// Recording configuration TLV types.
const (
	recPrebufferLength = 1
	recEventTrigger    = 2
	recMediaContainers = 3

	mediaContainerType   = 1
	mediaContainerParams = 2
	mediaFragmentLength  = 1
	mediaContainerMP4    = 0 // FRAGMENTED_MP4

	videoCodecConfiguration = 1
	videoCodecType          = 1
	videoCodecParameters    = 2
	videoAttributes         = 3

	videoProfileID      = 1
	videoLevel          = 2
	videoBitrate        = 3
	videoIFrameInterval = 4

	videoWidth     = 1
	videoHeight    = 2
	videoFrameRate = 3

	audioCodecConfiguration = 1
	audioCodecType          = 1
	audioCodecParameters    = 2

	audioChannel     = 1
	audioBitRateMode = 2
	audioSampleRate  = 3
	audioMaxBitrate  = 4

	selectedRecording = 1
	selectedVideo     = 2
	selectedAudio     = 3
)

// Codec/enum values.
const (
	H264ProfileBaseline = 0
	H264ProfileMain     = 1
	H264ProfileHigh     = 2

	H264Level31 = 0
	H264Level32 = 1
	H264Level40 = 2

	AudioCodecAACLC  = 0
	AudioCodecAACELD = 1

	SampleRate8kHz   = 0
	SampleRate16kHz  = 1
	SampleRate24kHz  = 2
	SampleRate32kHz  = 3
	SampleRate441kHz = 4
	SampleRate48kHz  = 5

	EventTriggerMotion   = 1
	EventTriggerDoorbell = 2
)

// buildSupportedDataStreamTransportConfiguration returns the static value
// advertising HomeKit Data Stream over TCP.
func buildSupportedDataStreamTransportConfiguration() []byte {
	inner := newTLV().addByte(transportTypeInner, transportHomeKitDataStream)
	return newTLV().addNested(transferTransportConfiguration, inner).bytes()
}

// buildSetupDataStreamResponse builds the write response carrying the listening
// port and the accessory's key salt.
func buildSetupDataStreamResponse(port uint16, accessorySalt []byte) []byte {
	params := newTLV().addUint16(sessionParamTCPPort, port)
	return newTLV().
		addByte(setupStatus, dataStreamStatusOK).
		addNested(setupSessionParams, params).
		addBytes(setupAccessoryKey, accessorySalt).
		bytes()
}

// setupRequest holds the parsed SetupDataStreamTransport write.
type setupRequest struct {
	command       byte
	transportType byte
	controllerKey []byte
}

func parseSetupDataStreamRequest(b []byte) (setupRequest, error) {
	m, err := parseTLV(b)
	if err != nil {
		return setupRequest{}, err
	}
	salt, ok := m.get(setupControllerKey)
	if !ok || len(salt) != 32 {
		return setupRequest{}, fmt.Errorf("setup: controller key salt must be 32 bytes, got %d", len(salt))
	}
	return setupRequest{
		command:       m.byteVal(setupCommandType, commandStartSession),
		transportType: m.byteVal(setupTransportType, transportHomeKitDataStream),
		controllerKey: salt,
	}, nil
}

// Resolution is a supported recording resolution.
type Resolution struct {
	Width, Height, FrameRate int
}

// buildSupportedCameraRecordingConfiguration advertises the prebuffer length,
// the enabled event triggers and a single fragmented-MP4 media container.
func buildSupportedCameraRecordingConfiguration(prebufferMS, fragmentMS int, triggers uint32) []byte {
	containerParams := newTLV().addInt32(mediaFragmentLength, int32(fragmentMS))
	container := newTLV().
		addByte(mediaContainerType, mediaContainerMP4).
		addNested(mediaContainerParams, containerParams)

	// EVENT_TRIGGER_OPTIONS is an 8-byte field with the bitmask in the low int32.
	trig := make([]byte, 8)
	trig[0] = byte(triggers)
	trig[1] = byte(triggers >> 8)
	trig[2] = byte(triggers >> 16)
	trig[3] = byte(triggers >> 24)

	return newTLV().
		addInt32(recPrebufferLength, int32(prebufferMS)).
		addBytes(recEventTrigger, trig).
		addNested(recMediaContainers, container).
		bytes()
}

// buildSupportedVideoRecordingConfiguration advertises an H.264 codec with the
// given profiles, levels and resolutions.
func buildSupportedVideoRecordingConfiguration(profiles, levels []byte, resolutions []Resolution) []byte {
	params := newTLV()
	params.addList(videoProfileID, singleByteItems(profiles))
	params.addList(videoLevel, singleByteItems(levels))

	attrs := make([][]byte, 0, len(resolutions))
	for _, r := range resolutions {
		attrs = append(attrs, newTLV().
			addUint16(videoWidth, uint16(r.Width)).
			addUint16(videoHeight, uint16(r.Height)).
			addByte(videoFrameRate, byte(r.FrameRate)).
			bytes())
	}

	cfg := newTLV().
		addByte(videoCodecType, 0). // H264
		addNested(videoCodecParameters, params)
	cfg.addList(videoAttributes, attrs)

	return newTLV().addNested(videoCodecConfiguration, cfg).bytes()
}

// buildSupportedAudioRecordingConfiguration advertises one audio codec with the
// given sample rates.
func buildSupportedAudioRecordingConfiguration(codecType byte, channels int, sampleRates []byte) []byte {
	params := newTLV().
		addByte(audioChannel, byte(max(1, channels))).
		addByte(audioBitRateMode, 0) // variable
	params.addList(audioSampleRate, singleByteItems(sampleRates))

	cfg := newTLV().
		addByte(audioCodecType, codecType).
		addNested(audioCodecParameters, params)

	return newTLV().addNested(audioCodecConfiguration, cfg).bytes()
}

func singleByteItems(bs []byte) [][]byte {
	out := make([][]byte, len(bs))
	for i, b := range bs {
		out[i] = []byte{b}
	}
	return out
}

// SelectedConfig is the recording configuration the controller selected.
type SelectedConfig struct {
	PrebufferMS      int
	EventTriggers    uint32
	FragmentLengthMS int
	Video            VideoSelection
	Audio            AudioSelection
	HasAudio         bool
}

// VideoSelection is the chosen H.264 recording profile.
type VideoSelection struct {
	Profile          byte
	Level            byte
	BitrateKbps      int
	IFrameIntervalMS int
	Width            int
	Height           int
	FrameRate        int
}

// AudioSelection is the chosen audio recording profile.
type AudioSelection struct {
	CodecType   byte
	Channels    int
	SampleRate  byte
	BitrateMode byte
	MaxBitrate  int
}

// parseSelectedConfig decodes a SelectedCameraRecordingConfiguration write.
func parseSelectedConfig(b []byte) (SelectedConfig, error) {
	top, err := parseTLV(b)
	if err != nil {
		return SelectedConfig{}, err
	}
	var sc SelectedConfig

	if gen, ok := top.get(selectedRecording); ok {
		g, err := parseTLV(gen)
		if err != nil {
			return SelectedConfig{}, err
		}
		sc.PrebufferMS = int(g.int32(recPrebufferLength, 4000))
		sc.EventTriggers = uint32(g.int32(recEventTrigger, 0))
		if mc, ok := g.get(recMediaContainers); ok {
			m, err := parseTLV(mc)
			if err != nil {
				return SelectedConfig{}, err
			}
			if mp, ok := m.get(mediaContainerParams); ok {
				p, err := parseTLV(mp)
				if err != nil {
					return SelectedConfig{}, err
				}
				sc.FragmentLengthMS = int(p.int32(mediaFragmentLength, 4000))
			}
		}
	}

	if vid, ok := top.get(selectedVideo); ok {
		v, err := parseTLV(vid)
		if err != nil {
			return SelectedConfig{}, err
		}
		if pc, ok := v.get(videoCodecParameters); ok {
			p, err := parseTLV(pc)
			if err != nil {
				return SelectedConfig{}, err
			}
			sc.Video.Profile = p.byteVal(videoProfileID, H264ProfileHigh)
			sc.Video.Level = p.byteVal(videoLevel, H264Level40)
			sc.Video.BitrateKbps = int(p.int32(videoBitrate, 0))
			sc.Video.IFrameIntervalMS = int(p.int32(videoIFrameInterval, 4000))
		}
		if at, ok := v.get(videoAttributes); ok {
			a, err := parseTLV(at)
			if err != nil {
				return SelectedConfig{}, err
			}
			sc.Video.Width = int(a.uint16(videoWidth, 1920))
			sc.Video.Height = int(a.uint16(videoHeight, 1080))
			sc.Video.FrameRate = int(a.byteVal(videoFrameRate, 30))
		}
	}

	if aud, ok := top.get(selectedAudio); ok {
		sc.HasAudio = true
		a, err := parseTLV(aud)
		if err != nil {
			return SelectedConfig{}, err
		}
		sc.Audio.CodecType = a.byteVal(audioCodecType, AudioCodecAACLC)
		if pc, ok := a.get(audioCodecParameters); ok {
			p, err := parseTLV(pc)
			if err != nil {
				return SelectedConfig{}, err
			}
			sc.Audio.Channels = int(p.byteVal(audioChannel, 1))
			sc.Audio.SampleRate = p.byteVal(audioSampleRate, SampleRate32kHz)
			sc.Audio.BitrateMode = p.byteVal(audioBitRateMode, 0)
			sc.Audio.MaxBitrate = int(p.int32(audioMaxBitrate, 0))
		}
	}

	return sc, nil
}
