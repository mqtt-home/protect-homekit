package hksv

import (
	"bytes"
	"testing"
)

func TestSupportedDataStreamTransportConfiguration(t *testing.T) {
	got := buildSupportedDataStreamTransportConfiguration()
	want := []byte{0x01, 0x03, 0x01, 0x01, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x, want %x", got, want)
	}
}

func TestSetupResponseAndRequestRoundTrip(t *testing.T) {
	salt := bytes.Repeat([]byte{0x11}, 32)
	resp := buildSetupDataStreamResponse(51234, salt)
	m, err := parseTLV(resp)
	if err != nil {
		t.Fatal(err)
	}
	if m.byteVal(setupStatus, 0xFF) != dataStreamStatusOK {
		t.Fatal("status not OK")
	}
	if gotSalt, _ := m.get(setupAccessoryKey); !bytes.Equal(gotSalt, salt) {
		t.Fatal("salt mismatch")
	}
	params, _ := m.get(setupSessionParams)
	pm, err := parseTLV(params)
	if err != nil {
		t.Fatal(err)
	}
	if pm.uint16(sessionParamTCPPort, 0) != 51234 {
		t.Fatalf("port = %d", pm.uint16(sessionParamTCPPort, 0))
	}
}

func TestParseSetupRequest(t *testing.T) {
	salt := bytes.Repeat([]byte{0x22}, 32)
	req := newTLV().
		addByte(setupCommandType, commandStartSession).
		addByte(setupTransportType, transportHomeKitDataStream).
		addBytes(setupControllerKey, salt).
		bytes()

	got, err := parseSetupDataStreamRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got.command != commandStartSession || got.transportType != transportHomeKitDataStream {
		t.Fatalf("bad fields: %+v", got)
	}
	if !bytes.Equal(got.controllerKey, salt) {
		t.Fatal("salt mismatch")
	}
}

func TestParseSetupRequestBadSalt(t *testing.T) {
	req := newTLV().addBytes(setupControllerKey, []byte{1, 2, 3}).bytes()
	if _, err := parseSetupDataStreamRequest(req); err == nil {
		t.Fatal("expected error for short salt")
	}
}

func TestSelectedConfigRoundTrip(t *testing.T) {
	// Build a SelectedCameraRecordingConfiguration the way a controller would.
	general := newTLV().
		addInt32(recPrebufferLength, 4000).
		addInt32(recEventTrigger, EventTriggerMotion).
		addNested(recMediaContainers, newTLV().
			addByte(mediaContainerType, mediaContainerMP4).
			addNested(mediaContainerParams, newTLV().addInt32(mediaFragmentLength, 4000)))

	video := newTLV().
		addByte(videoCodecType, 0).
		addNested(videoCodecParameters, newTLV().
			addByte(videoProfileID, H264ProfileHigh).
			addByte(videoLevel, H264Level40).
			addInt32(videoBitrate, 2000).
			addInt32(videoIFrameInterval, 4000)).
		addNested(videoAttributes, newTLV().
			addUint16(videoWidth, 1920).
			addUint16(videoHeight, 1080).
			addByte(videoFrameRate, 24))

	audio := newTLV().
		addByte(audioCodecType, AudioCodecAACLC).
		addNested(audioCodecParameters, newTLV().
			addByte(audioChannel, 1).
			addByte(audioBitRateMode, 0).
			addByte(audioSampleRate, SampleRate32kHz).
			addInt32(audioMaxBitrate, 64))

	sel := newTLV().
		addNested(selectedRecording, general).
		addNested(selectedVideo, video).
		addNested(selectedAudio, audio).
		bytes()

	sc, err := parseSelectedConfig(sel)
	if err != nil {
		t.Fatal(err)
	}
	if sc.PrebufferMS != 4000 || sc.EventTriggers != EventTriggerMotion || sc.FragmentLengthMS != 4000 {
		t.Fatalf("general wrong: %+v", sc)
	}
	if sc.Video.Profile != H264ProfileHigh || sc.Video.Level != H264Level40 {
		t.Fatalf("video codec wrong: %+v", sc.Video)
	}
	if sc.Video.BitrateKbps != 2000 || sc.Video.IFrameIntervalMS != 4000 {
		t.Fatalf("video params wrong: %+v", sc.Video)
	}
	if sc.Video.Width != 1920 || sc.Video.Height != 1080 || sc.Video.FrameRate != 24 {
		t.Fatalf("video attrs wrong: %+v", sc.Video)
	}
	if !sc.HasAudio || sc.Audio.CodecType != AudioCodecAACLC || sc.Audio.SampleRate != SampleRate32kHz {
		t.Fatalf("audio wrong: %+v", sc.Audio)
	}
	if sc.Audio.MaxBitrate != 64 {
		t.Fatalf("audio bitrate = %d", sc.Audio.MaxBitrate)
	}
}

func TestSupportedVideoConfigParses(t *testing.T) {
	blob := buildSupportedVideoRecordingConfiguration(
		[]byte{H264ProfileHigh}, []byte{H264Level40},
		[]Resolution{{1920, 1080, 24}, {1280, 720, 24}},
	)
	top, err := parseTLV(blob)
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok := top.get(videoCodecConfiguration)
	if !ok {
		t.Fatal("missing codec configuration")
	}
	c, err := parseTLV(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.byteVal(videoCodecType, 0xFF) != 0 {
		t.Fatal("codec type not H264")
	}
	// Two resolutions -> two ATTRIBUTES list entries.
	if n := len(c.list(videoAttributes)); n != 2 {
		t.Fatalf("expected 2 attributes entries, got %d", n)
	}
}

func TestSupportedAudioConfigParses(t *testing.T) {
	blob := buildSupportedAudioRecordingConfiguration(AudioCodecAACLC, 1, []byte{SampleRate16kHz, SampleRate32kHz})
	top, err := parseTLV(blob)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := top.get(audioCodecConfiguration)
	c, err := parseTLV(cfg)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := c.get(audioCodecParameters)
	p, err := parseTLV(params)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.list(audioSampleRate)); n != 2 {
		t.Fatalf("expected 2 sample rates, got %d", n)
	}
}
