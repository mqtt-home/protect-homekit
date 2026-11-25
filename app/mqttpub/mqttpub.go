// Package mqttpub publishes the state of the bridged cameras to MQTT, so
// automations outside HomeKit can react to Protect motion and ring events.
package mqttpub

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mqtt-home/protect-homekit/bridge"
	"github.com/philipparndt/go-logger"
)

// State is the retained payload on `<topic>/<camera>`. Keys are snake_case
// and stable; times are RFC 3339 and absent when Protect never reported one.
type State struct {
	Name       string `json:"name"`
	Online     bool   `json:"online"`
	Motion     bool   `json:"motion"`
	LastMotion string `json:"last_motion,omitempty"`
	LastRing   string `json:"last_ring,omitempty"`
}

// PublishFunc sends one message; the default is mqtt.PublishAbsolute.
type PublishFunc func(topic string, payload any, retained bool)

type message struct {
	topic    string
	payload  string
	retained bool
}

// Publisher turns camera updates into MQTT messages:
//
//	<topic>/<camera>         retained JSON State, on every change
//	<topic>/<camera>/motion  "true" / "false", only when motion starts or ends
//	<topic>/<camera>/ring    "true", once per ring
//
// The motion and ring topics are events and never retained, so a consumer
// that (re)connects does not react to something that happened earlier.
type Publisher struct {
	topic   string
	publish PublishFunc

	mu   sync.Mutex
	seen map[string]seen

	queue chan message
	done  chan struct{}
}

type seen struct {
	state  string
	motion bool
	ring   int64
}

// New starts a publisher below the base topic. Messages are sent from their
// own goroutine: an update from Protect never waits for the broker.
func New(topic string, publish PublishFunc) *Publisher {
	p := &Publisher{
		topic:   strings.TrimSuffix(topic, "/"),
		publish: publish,
		seen:    map[string]seen{},
		queue:   make(chan message, 256),
		done:    make(chan struct{}),
	}
	go p.run()
	return p
}

func (p *Publisher) run() {
	defer close(p.done)
	for m := range p.queue {
		p.publish(m.topic, m.payload, m.retained)
	}
}

// Close sends what is queued and stops the publisher.
func (p *Publisher) Close() {
	close(p.queue)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
}

func (p *Publisher) enqueue(m message) {
	select {
	case p.queue <- m:
	default:
		logger.Warn("MQTT queue full, dropping message", "topic", m.topic)
	}
}

// OnCamera is the bridge's update listener.
func (p *Publisher) OnCamera(info bridge.CameraInfo) {
	slug := Slug(info.Name)
	if slug == "" {
		slug = Slug(info.ID)
	}
	base := p.topic + "/" + slug

	data, err := json.Marshal(State{
		Name:       info.Name,
		Online:     info.Online,
		Motion:     info.Motion,
		LastMotion: rfc3339(info.LastMotion),
		LastRing:   rfc3339(info.LastRing),
	})
	if err != nil {
		logger.Error("Encoding camera state", "error", err)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	prev, known := p.seen[info.ID]
	next := seen{state: string(data), motion: info.Motion, ring: info.LastRing}
	p.seen[info.ID] = next

	if !known || prev.state != next.state {
		p.enqueue(message{base, next.state, true})
	}
	// The first sight of a camera is its current state, not an event: only
	// an ongoing motion is announced, an old ring is not.
	if (known && prev.motion != next.motion) || (!known && next.motion) {
		p.enqueue(message{base + "/motion", boolString(next.motion), false})
	}
	if known && next.ring > prev.ring {
		p.enqueue(message{base + "/ring", "true", false})
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func rfc3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

var umlauts = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss")

// Slug makes a camera name usable as one topic level: lower case, letters
// and digits, everything else collapsed to single dashes ("Fam. Arndt" ->
// "fam-arndt").
func Slug(name string) string {
	var sb strings.Builder
	dash := false
	for _, r := range umlauts.Replace(strings.ToLower(name)) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			if dash && sb.Len() > 0 {
				sb.WriteByte('-')
			}
			dash = false
			sb.WriteRune(r)
			continue
		}
		dash = true
	}
	return sb.String()
}
