package hksv

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/service"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(Options{
		CameraName: "Test Cam",
		FFmpegPath: "ffmpeg",
		Resolve:    func(int) (string, error) { return "rtsps://example/stream", nil },
		HasMotion:  true,
		HasMic:     true,
	})
	t.Cleanup(m.Close)
	return m
}

func TestManagerConstruction(t *testing.T) {
	m := testManager(t)

	if got := len(m.Services()); got != 3 {
		t.Fatalf("expected 3 services, got %d", got)
	}
	if len(m.Recording.SupportedCamera.Value()) == 0 {
		t.Fatal("SupportedCamera not advertised")
	}
	if len(m.Recording.SupportedVideo.Value()) == 0 {
		t.Fatal("SupportedVideo not advertised")
	}
	if len(m.Recording.SupportedAudio.Value()) == 0 {
		t.Fatal("SupportedAudio not advertised")
	}
	if !bytes.Equal(m.DataStream.SupportedConfiguration.Value(), []byte{0x01, 0x03, 0x01, 0x01, 0x00}) {
		t.Fatal("SupportedDataStreamTransportConfiguration wrong")
	}
	if m.DataStream.Version.Value() != "1.0" {
		t.Fatalf("data stream version = %q", m.DataStream.Version.Value())
	}
	if m.canRecord() {
		t.Fatal("should not be recordable before Active/HomeKitCameraActive set")
	}
}

func TestManagerNoMicSetsAudioInactive(t *testing.T) {
	m := NewManager(Options{
		CameraName: "No Mic",
		Resolve:    func(int) (string, error) { return "", nil },
		HasMic:     false,
	})
	t.Cleanup(m.Close)
	if m.Recording.RecordingAudioActive.Value() != activeInactive {
		t.Fatal("expected RecordingAudioActive inactive without a mic")
	}
}

// TestManagerServicesMarshal ensures the custom HKSV services serialize into the
// HAP accessory database that HomeKit reads. A misconfigured characteristic
// (bad format/permissions) would surface here rather than only at pairing.
func TestManagerServicesMarshal(t *testing.T) {
	m := testManager(t)
	acc := accessory.New(accessory.Info{Name: "Test Cam"}, accessory.TypeIPCamera)
	for _, s := range m.Services() {
		acc.AddS(s)
	}

	data, err := json.Marshal(acc)
	if err != nil {
		t.Fatalf("marshal accessory: %v", err)
	}
	js := string(data)
	for _, typ := range []string{
		typeCameraRecordingManagement,
		typeCameraOperatingMode,
		typeDataStreamTransportManagement,
		typeSetupDataStreamTransport,
		typeSupportedCameraRecordingConfiguration,
	} {
		if !bytes.Contains(data, []byte(`"`+typ+`"`)) {
			t.Fatalf("service/characteristic type %q missing from accessory JSON:\n%s", typ, js)
		}
	}
}

// TestRecordingServiceLinks verifies the linked-service relationships HomeKit
// requires to enable recording: the data-stream transport and the event-trigger
// service must both be linked to the recording management service.
func TestRecordingServiceLinks(t *testing.T) {
	m := testManager(t)

	linked := func(target *service.S) bool {
		for _, s := range m.Recording.S.Linked {
			if s == target {
				return true
			}
		}
		return false
	}

	if !linked(m.DataStream.S) {
		t.Fatal("DataStreamTransportManagement must be linked to CameraRecordingManagement")
	}

	motion := service.NewMotionSensor()
	m.LinkTriggerService(motion.S)
	if !linked(motion.S) {
		t.Fatal("trigger service must be linked to CameraRecordingManagement")
	}
}

func TestCanRecordGating(t *testing.T) {
	m := testManager(t)
	// Active defaults inactive; HomeKitCameraActive defaults active (camera on).
	if m.canRecord() {
		t.Fatal("recording must be off until Active is set")
	}
	_ = m.Recording.Active.SetValue(activeActive)
	if !m.canRecord() {
		t.Fatal("should be recordable once Active is set (camera is on by default)")
	}
	// Turning the camera off in the Home app must stop recording.
	_ = m.Operating.HomeKitCameraActive.SetValue(activeInactive)
	if m.canRecord() {
		t.Fatal("HomeKitCameraActive off must block recording")
	}
}
