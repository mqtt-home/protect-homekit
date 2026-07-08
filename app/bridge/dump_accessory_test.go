package bridge

import (
	"encoding/json"
	"testing"

	"github.com/mqtt-home/protect-homekit/hksv"
	"github.com/mqtt-home/protect-homekit/protect"
)

// buildTestCamera constructs a HKSV-enabled camera accessory for structure tests.
func buildTestCamera(t *testing.T) *CameraAccessory {
	t.Helper()
	cam := protect.Camera{ID: "cam1", Name: "Gartenhaus", Mac: "AABBCCDDEEFF", Type: "G4"}
	cam.FeatureFlags.HasMic = true

	str := newStreamer(cam.Name, "ffmpeg", true, false, false, func(int) (string, error) { return "rtsps://x", nil })
	sv := hksv.NewManager(hksv.Options{
		CameraName: cam.Name,
		Resolve:    func(int) (string, error) { return "rtsps://x", nil },
		HasMotion:  true, HasMic: true,
	})
	t.Cleanup(sv.Close)

	acc := newCameraAccessory(cam, "1.0", str, true, sv)
	acc.Id = 1
	return acc
}

type accStruct struct {
	Services []struct {
		Type  string `json:"type"`
		Chars []struct {
			Type string `json:"type"`
		} `json:"characteristics"`
	} `json:"services"`
}

func cameraServices(t *testing.T, acc *CameraAccessory) accStruct {
	t.Helper()
	b, err := json.Marshal(acc.A)
	if err != nil {
		t.Fatal(err)
	}
	var s accStruct
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestStreamManagementHasActive guards the HKSV streaming-feature fix: HomeKit
// only recognises the camera's "streaming" feature (and thus allows "Stream &
// Allow Recording") when CameraRTPStreamManagement (type 110) carries an Active
// characteristic (type B0). Its absence made the Apple TV reject recording with
// "supported features do not include streaming".
func TestStreamManagementHasActive(t *testing.T) {
	s := cameraServices(t, buildTestCamera(t))

	const streamMgmt = "110"
	const active = "B0"
	found := false
	for _, svc := range s.Services {
		if svc.Type != streamMgmt {
			continue
		}
		found = true
		hasActive := false
		for _, c := range svc.Chars {
			if c.Type == active {
				hasActive = true
			}
		}
		if !hasActive {
			t.Fatal("CameraRTPStreamManagement (110) must have an Active (B0) characteristic for HKSV")
		}
	}
	if !found {
		t.Fatal("camera missing CameraRTPStreamManagement service (110)")
	}
}

// TestCameraHasHKSVServices sanity-checks the full HKSV service set is present.
func TestCameraHasHKSVServices(t *testing.T) {
	s := cameraServices(t, buildTestCamera(t))
	want := map[string]bool{
		"110": false, // CameraRTPStreamManagement
		"21A": false, // CameraOperatingMode
		"204": false, // CameraRecordingManagement
		"129": false, // DataStreamTransportManagement
	}
	for _, svc := range s.Services {
		if _, ok := want[svc.Type]; ok {
			want[svc.Type] = true
		}
	}
	for typ, present := range want {
		if !present {
			t.Fatalf("camera missing required HKSV service type %s", typ)
		}
	}
}
