package bridge

import (
	"testing"

	"github.com/mqtt-home/protect-homekit/config"
	"github.com/mqtt-home/protect-homekit/protect"
)

func ch(id int, width, height int, rtsp bool) protect.Channel {
	return protect.Channel{
		ID: id, Name: "ch", Enabled: true,
		IsRtspEnabled: rtsp, RtspAlias: "alias",
		Width: width, Height: height,
	}
}

func TestSelectChannel(t *testing.T) {
	channels := []protect.Channel{
		ch(0, 1920, 1080, true),
		ch(1, 1280, 720, true),
		ch(2, 640, 360, true),
	}

	tests := []struct {
		requested int
		wantWidth int
	}{
		{1920, 1920},
		{1280, 1280},
		{640, 640},
		{320, 640},   // smallest that satisfies
		{1000, 1280}, // next size up
		{3840, 1920}, // nothing satisfies -> largest
	}
	for _, tc := range tests {
		got, ok := selectChannel(channels, tc.requested)
		if !ok {
			t.Fatalf("no channel for width %d", tc.requested)
		}
		if got.Width != tc.wantWidth {
			t.Errorf("selectChannel(%d) = %d, want %d", tc.requested, got.Width, tc.wantWidth)
		}
	}
}

func TestSelectChannelSkipsNonRTSP(t *testing.T) {
	channels := []protect.Channel{
		ch(0, 1920, 1080, false),
		ch(1, 1280, 720, true),
	}
	got, ok := selectChannel(channels, 1920)
	if !ok || got.Width != 1280 {
		t.Errorf("expected 1280 channel, got %+v ok=%v", got, ok)
	}

	if _, ok := selectChannel([]protect.Channel{ch(0, 1920, 1080, false)}, 1920); ok {
		t.Error("expected no channel when RTSP is disabled everywhere")
	}

	// Alias missing means unusable, even if flagged enabled.
	noAlias := protect.Channel{ID: 0, Enabled: true, IsRtspEnabled: true, Width: 1920}
	if _, ok := selectChannel([]protect.Channel{noAlias}, 1920); ok {
		t.Error("expected no channel when rtspAlias is empty")
	}
}

func TestStableAID(t *testing.T) {
	b := New(testConfig())
	a1 := b.stableAID("cam-a")
	a2 := b.stableAID("cam-b")
	if a1 == a2 {
		t.Error("expected distinct AIDs")
	}
	if a1 < 2 || a2 < 2 {
		t.Error("AID 1 is reserved for the bridge")
	}

	// Same input in a fresh bridge yields the same AID (stability across
	// restarts).
	b2 := New(testConfig())
	if b2.stableAID("cam-a") != a1 {
		t.Error("AID not stable for same camera id")
	}
}

func TestSetupURI(t *testing.T) {
	// Same encoding as HAP-NodeJS; value verified against mqtt-homekit.
	uri := setupURI("031-45-154", categoryBridge, "PRTC")
	if len(uri) != len("X-HM://")+9+4 {
		t.Errorf("unexpected uri %q", uri)
	}
	if uri[:7] != "X-HM://" {
		t.Errorf("unexpected scheme in %q", uri)
	}
}

func TestNormalizePin(t *testing.T) {
	if got := normalizePin("031-45-154"); got != "03145154" {
		t.Errorf("normalizePin = %q", got)
	}
}

func testConfig() config.Config {
	return config.Config{}
}

func TestSelectRecordingChannelAvoids4K(t *testing.T) {
	// Gartenhaus: High 4K, Medium 720p, Low 360p. Recording must pick Medium,
	// not the 4K channel, to keep the re-encode within budget.
	channels := []protect.Channel{
		ch(0, 3840, 2160, true),
		ch(1, 1280, 720, true),
		ch(2, 640, 360, true),
	}
	got, ok := selectRecordingChannel(channels, recordingMaxSourceWidth)
	if !ok || got.Width != 1280 {
		t.Fatalf("expected 1280-wide Medium channel, got %dx%d ok=%v", got.Width, got.Height, ok)
	}
}

func TestSelectRecordingChannelFallsBackToSmallest(t *testing.T) {
	// All channels exceed the cap (e.g. only 4K/5K variants): use the smallest.
	channels := []protect.Channel{
		ch(0, 3840, 2160, true),
		ch(1, 2560, 1440, true),
	}
	got, ok := selectRecordingChannel(channels, recordingMaxSourceWidth)
	if !ok || got.Width != 2560 {
		t.Fatalf("expected smallest (2560) when all exceed cap, got %d ok=%v", got.Width, ok)
	}
}
