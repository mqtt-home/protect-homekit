package mqttpub

import (
	"sync"
	"testing"

	"github.com/mqtt-home/protect-homekit/bridge"
)

type recorder struct {
	mu   sync.Mutex
	msgs []message
}

func (r *recorder) publish(topic string, payload any, retained bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, message{topic, payload.(string), retained})
}

// run feeds the updates through a publisher and returns what it sent.
func run(updates ...bridge.CameraInfo) []message {
	rec := &recorder{}
	p := New("home/protect/", rec.publish)
	for _, u := range updates {
		p.OnCamera(u)
	}
	p.Close()
	return rec.msgs
}

func expect(t *testing.T, got []message, want ...message) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d messages %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Fam. Arndt":   "fam-arndt",
		"Gartenhaus":   "gartenhaus",
		"  Tür (Süd) ": "tuer-sued",
		"a/b#c+d":      "a-b-c-d",
		"...":          "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStateIsRetainedAndMotionIsAnEvent(t *testing.T) {
	cam := bridge.CameraInfo{ID: "1", Name: "Fam. Arndt", Online: true, LastRing: 1795588408858}
	moving := cam
	moving.Motion = true
	moving.LastMotion = 1795588418858

	got := run(cam, moving, moving, cam)

	idle := `{"name":"Fam. Arndt","online":true,"motion":false,"last_ring":"2026-11-25T06:33:28Z"}`
	active := `{"name":"Fam. Arndt","online":true,"motion":true,"last_motion":"2026-11-25T06:33:38Z","last_ring":"2026-11-25T06:33:28Z"}`
	expect(t, got,
		// first sight: state only, the old ring is not replayed
		message{"home/protect/fam-arndt", idle, true},
		message{"home/protect/fam-arndt", active, true},
		message{"home/protect/fam-arndt/motion", "true", false},
		// the unchanged repeat sends nothing
		message{"home/protect/fam-arndt", idle, true},
		message{"home/protect/fam-arndt/motion", "false", false},
	)
}

func TestOngoingMotionAtStartIsAnnounced(t *testing.T) {
	got := run(bridge.CameraInfo{ID: "1", Name: "Garten", Online: true, Motion: true})
	expect(t, got,
		message{"home/protect/garten", `{"name":"Garten","online":true,"motion":true}`, true},
		message{"home/protect/garten/motion", "true", false},
	)
}

func TestRingIsSentOncePerRing(t *testing.T) {
	cam := bridge.CameraInfo{ID: "1", Name: "Tür", Online: true}
	rung := cam
	rung.LastRing = 1795588408858

	got := run(cam, rung, rung)
	expect(t, got,
		message{"home/protect/tuer", `{"name":"Tür","online":true,"motion":false}`, true},
		message{"home/protect/tuer", `{"name":"Tür","online":true,"motion":false,"last_ring":"2026-11-25T06:33:28Z"}`, true},
		message{"home/protect/tuer/ring", "true", false},
	)
}

func TestCameraWithoutNameUsesItsID(t *testing.T) {
	got := run(bridge.CameraInfo{ID: "6798FA52", Name: "..."})
	expect(t, got, message{"home/protect/6798fa52", `{"name":"...","online":false,"motion":false}`, true})
}
