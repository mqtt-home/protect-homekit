package bridge

import (
	"strings"
	"testing"

	"github.com/brutella/hap/rtp"
)

func streamArgs(t *testing.T, aacEld bool, codec byte) []string {
	t.Helper()
	s := newStreamer("cam", "ffmpeg", true, aacEld, false, func(int) (string, error) { return "rtsps://x", nil })
	sess := &session{
		req:  rtp.SetupEndpoints{ControllerAddr: rtp.Addr{IPAddr: "10.0.0.1", VideoRtpPort: 5000, AudioRtpPort: 5002}},
		resp: rtp.SetupEndpointsResponse{},
	}
	video := rtp.VideoParameters{Attributes: rtp.VideoCodecAttributes{Width: 1280, Height: 720}}
	audio := rtp.AudioParameters{CodecType: codec}
	return s.ffmpegArgs(sess, "rtsps://x", video, audio)
}

func TestStreamSelectsAacEldWhenSupported(t *testing.T) {
	args := strings.Join(streamArgs(t, true, rtp.AudioCodecType_AAC_ELD), " ")
	if !strings.Contains(args, "libfdk_aac") || !strings.Contains(args, "aac_eld") {
		t.Fatalf("AAC-ELD stream must use libfdk_aac aac_eld, got: %s", args)
	}
}

func TestStreamSelectsOpus(t *testing.T) {
	args := strings.Join(streamArgs(t, true, rtp.AudioCodecType_Opus), " ")
	if !strings.Contains(args, "libopus") {
		t.Fatalf("Opus stream must use libopus, got: %s", args)
	}
	if strings.Contains(args, "libfdk_aac") {
		t.Fatal("Opus selection must not invoke libfdk_aac")
	}
}
