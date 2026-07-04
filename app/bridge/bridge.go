package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	dlog "github.com/brutella/dnssd/log"
	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	hlog "github.com/brutella/hap/log"
	"github.com/mqtt-home/protect-homekit/config"
	"github.com/mqtt-home/protect-homekit/protect"
	"github.com/mqtt-home/protect-homekit/version"
	"github.com/philipparndt/go-logger"
	qrcode "github.com/skip2/go-qrcode"
)

// categoryBridge is the HomeKit accessory category for a bridge.
const categoryBridge = 2

// snapshotCacheTTL bounds how often the NVR is asked for a fresh snapshot;
// the Home app refreshes tiles aggressively.
const snapshotCacheTTL = 3 * time.Second

// Bridge exposes UniFi Protect cameras as HomeKit accessories using
// brutella/hap.
type Bridge struct {
	cfg    config.Config
	client *protect.Client
	events *protect.Events

	server    *hap.Server
	bridgeAcc *accessory.Bridge
	cancel    context.CancelFunc
	done      chan struct{} // closed when the HAP server has fully stopped

	cameras  map[string]*CameraAccessory // by Protect camera id
	byAID    map[uint64]*CameraAccessory
	usedAIDs map[uint64]bool

	// stateMu guards the mutable Protect state consulted at stream/snapshot
	// time (channels, NVR ports), refreshed on every bootstrap.
	stateMu   sync.RWMutex
	camState  map[string]protect.Camera
	bootstrap *protect.Bootstrap

	snapMu    sync.Mutex
	snapshots map[string]cachedSnapshot

	// onUpdate is fired with fresh camera state (web UI live updates).
	onUpdate func(CameraInfo)
}

// CameraInfo is a read-only snapshot of one camera's state for the web UI.
type CameraInfo struct {
	Type       string        `json:"type"` // SSE event discriminator, always "camera"
	AID        uint64        `json:"aid"`
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Model      string        `json:"model"`
	Mac        string        `json:"mac"`
	Firmware   string        `json:"firmware"`
	Online     bool          `json:"online"`
	Motion     bool          `json:"motion"`
	LastMotion int64         `json:"last_motion"`
	LastRing   int64         `json:"last_ring"`
	Doorbell   bool          `json:"doorbell"`
	Codec      string        `json:"codec"`
	Channels   []ChannelInfo `json:"channels"`
}

type ChannelInfo struct {
	Name   string `json:"name"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	FPS    int    `json:"fps"`
	RTSP   bool   `json:"rtsp"`
}

type cachedSnapshot struct {
	data  []byte
	taken time.Time
}

func New(cfg config.Config) *Bridge {
	client := protect.NewClient(cfg.Protect.Host, cfg.Protect.Username, cfg.Protect.Password, cfg.Protect.VerifySSL)
	return &Bridge{
		cfg:       cfg,
		client:    client,
		events:    protect.NewEvents(client),
		cameras:   map[string]*CameraAccessory{},
		byAID:     map[uint64]*CameraAccessory{},
		usedAIDs:  map[uint64]bool{1: true}, // AID 1 is the bridge
		camState:  map[string]protect.Camera{},
		snapshots: map[string]cachedSnapshot{},
	}
}

// Start logs in to Protect, builds the accessories and starts the HAP server
// plus the realtime event stream.
func (b *Bridge) Start() error {
	routeHAPLogging()

	if err := b.client.Login(); err != nil {
		return fmt.Errorf("protect login: %w", err)
	}
	bs, err := b.client.Bootstrap()
	if err != nil {
		return err
	}
	logger.Info("Connected to Protect", "nvr", bs.NVR.Name, "version", bs.NVR.Version, "cameras", len(bs.Cameras))

	bs, err = b.ensureRTSP(bs)
	if err != nil {
		return err
	}
	b.applyBootstrap(bs)

	var accs []*accessory.A
	for _, cam := range bs.Cameras {
		if !b.cfg.Cameras.CameraSelected(cam.Name, cam.ID, cam.Mac) {
			logger.Debug("Skipping camera (filtered)", "camera", cam.Name)
			continue
		}
		if !b.hasUsableChannel(cam) {
			logger.Warn("Skipping camera: no RTSP-enabled channel; enable RTSP in Protect or set cameras.auto_enable_rtsp", "camera", cam.Name)
			continue
		}

		acc := b.buildCamera(cam)
		accs = append(accs, acc.A)
		logger.Info("Configured camera", "camera", cam.Name, "type", cam.Type,
			"doorbell", cam.IsDoorbell(), "aid", acc.Id)
	}
	if len(accs) == 0 {
		return fmt.Errorf("no cameras to bridge")
	}

	b.bridgeAcc = accessory.NewBridge(accessory.Info{
		Name:         b.cfg.HomeKit.BridgeName,
		Manufacturer: "mqtt-home",
		Model:        "protect-homekit",
		SerialNumber: bs.NVR.Mac,
		Firmware:     version.Version,
	})
	b.bridgeAcc.Id = 1

	store := hap.NewFsStore(b.cfg.HomeKit.StorageDir)
	server, err := hap.NewServer(store, b.bridgeAcc.A, accs...)
	if err != nil {
		return fmt.Errorf("create HAP server: %w", err)
	}
	server.Pin = normalizePin(b.cfg.HomeKit.Pin)
	if b.cfg.HomeKit.SetupID != "" {
		server.SetupId = b.cfg.HomeKit.SetupID
	}
	if b.cfg.HomeKit.Port > 0 {
		server.Addr = fmt.Sprintf(":%d", b.cfg.HomeKit.Port)
	}
	if len(b.cfg.HomeKit.Interfaces) > 0 {
		server.Ifaces = b.cfg.HomeKit.Interfaces
	}
	// brutella/hap has no built-in handler for HomeKit snapshot requests
	// (POST /resource on the secured session); register our own that serves
	// JPEGs straight from the NVR.
	server.ServeMux().HandleFunc("/resource", b.handleResource)
	b.server = server

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.done = make(chan struct{})

	go func() {
		defer close(b.done)
		logger.Info("Starting HAP server", "bridge", b.cfg.HomeKit.BridgeName, "pin", b.cfg.HomeKit.Pin, "storage", b.cfg.HomeKit.StorageDir)
		if err := server.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
			logger.Error("HAP server stopped", "error", err)
		}
	}()

	b.events.OnBootstrap = b.onBootstrap
	b.events.OnCameraUpdate = b.onCameraUpdate
	go b.events.Run(ctx, bs)

	b.logPairingInfo()
	logger.Info("protect-homekit bridge started", "cameras", len(b.cameras))
	return nil
}

// SetUpdateListener registers a callback fired whenever a camera's state
// changes (used by the web UI). Must be called before Start.
func (b *Bridge) SetUpdateListener(fn func(CameraInfo)) { b.onUpdate = fn }

func (b *Bridge) BridgeName() string { return b.cfg.HomeKit.BridgeName }
func (b *Bridge) Pin() string        { return b.cfg.HomeKit.Pin }
func (b *Bridge) SetupID() string    { return b.cfg.HomeKit.SetupID }
func (b *Bridge) Healthy() bool      { return b.server != nil }

// SetupURI returns the X-HM:// pairing payload encoded in the HomeKit QR code.
func (b *Bridge) SetupURI() string {
	return setupURI(b.cfg.HomeKit.Pin, categoryBridge, b.cfg.HomeKit.SetupID)
}

// NVRInfo returns name and version of the connected Protect console.
func (b *Bridge) NVRInfo() (string, string) {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	if b.bootstrap == nil {
		return "", ""
	}
	return b.bootstrap.NVR.Name, b.bootstrap.NVR.Version
}

// CameraInfos returns the current state of all bridged cameras.
func (b *Bridge) CameraInfos() []CameraInfo {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	out := make([]CameraInfo, 0, len(b.cameras))
	for id, acc := range b.cameras {
		out = append(out, b.cameraInfoLocked(id, acc))
	}
	return out
}

// cameraInfoLocked builds the web payload; callers hold stateMu.
func (b *Bridge) cameraInfoLocked(id string, acc *CameraAccessory) CameraInfo {
	cam := b.camState[id]
	info := CameraInfo{
		Type:       "camera",
		AID:        acc.Id,
		ID:         id,
		Name:       cam.Name,
		Model:      cam.Type,
		Mac:        cam.Mac,
		Firmware:   cam.FirmwareVersion,
		Online:     cam.Connected(),
		Motion:     cam.IsMotionDetected,
		LastMotion: cam.LastMotion,
		LastRing:   cam.LastRing,
		Doorbell:   cam.IsDoorbell(),
		Codec:      cam.VideoCodec,
	}
	for _, ch := range cam.Channels {
		if !ch.Enabled {
			continue
		}
		info.Channels = append(info.Channels, ChannelInfo{
			Name: ch.Name, Width: ch.Width, Height: ch.Height, FPS: ch.FPS, RTSP: ch.IsRtspEnabled,
		})
	}
	return info
}

// CameraSnapshot returns a JPEG for the web UI (shares the HomeKit snapshot
// cache).
func (b *Bridge) CameraSnapshot(id string, width, height int) ([]byte, error) {
	if _, ok := b.cameras[id]; !ok {
		return nil, fmt.Errorf("unknown camera %s", id)
	}
	return b.snapshot(id, width, height)
}

// notifyUpdate pushes fresh state for one camera to the web UI.
func (b *Bridge) notifyUpdate(id string) {
	if b.onUpdate == nil {
		return
	}
	acc, ok := b.cameras[id]
	if !ok {
		return
	}
	b.stateMu.RLock()
	info := b.cameraInfoLocked(id, acc)
	b.stateMu.RUnlock()
	b.onUpdate(info)
}

// Stop shuts down streams, the event stream and the HAP server. Waiting for
// the HAP shutdown lets dnssd say goodbye so controllers mark the bridge
// offline immediately.
func (b *Bridge) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	for _, acc := range b.cameras {
		acc.streamer.stopAll()
	}
	if b.done != nil {
		select {
		case <-b.done:
		case <-time.After(5 * time.Second):
			logger.Warn("Timed out waiting for HAP server shutdown")
		}
	}
}

// ensureRTSP prepares selected cameras for HomeKit streaming: it enables
// RTSPS channels and switches H.265 ("enhanced encoding") cameras back to
// H.264 — HomeKit only decodes H.264, and this bridge never transcodes video.
// Afterwards it re-bootstraps once so fresh rtspAlias/codec values are
// available.
func (b *Bridge) ensureRTSP(bs *protect.Bootstrap) (*protect.Bootstrap, error) {
	changed := false
	for i := range bs.Cameras {
		cam := &bs.Cameras[i]
		if !b.cfg.Cameras.CameraSelected(cam.Name, cam.ID, cam.Mac) {
			continue
		}

		if !cam.UsesH264() {
			if !b.cfg.Cameras.ForceH264Enabled() {
				logger.Warn("Camera uses H.265 (enhanced encoding); HomeKit cannot stream it. "+
					"Set encoding to Standard in the Protect UI or enable cameras.force_h264",
					"camera", cam.Name, "codec", cam.VideoCodec)
			} else if err := b.client.SetVideoCodec(cam, "h264"); err != nil {
				logger.Warn("Could not switch camera to H.264 (non-admin user?)", "camera", cam.Name, "error", err)
			} else {
				logger.Info("Camera switched to H.264 for HomeKit; Protect recordings will use more storage. "+
					"Set cameras.force_h264: false to opt out", "camera", cam.Name)
				// The encoder only applies the new codec when the camera's
				// video pipeline restarts; without the reboot it keeps
				// streaming H.265 indefinitely.
				if err := b.client.Reboot(cam); err != nil {
					logger.Warn("Camera reboot failed; restart it manually in Protect to apply H.264", "camera", cam.Name, "error", err)
				}
				changed = true
			}
		}

		if !b.cfg.Cameras.AutoEnableRTSPEnabled() {
			continue
		}
		var missing []int
		for _, ch := range cam.Channels {
			if ch.Enabled && !ch.IsRtspEnabled {
				missing = append(missing, ch.ID)
			}
		}
		if len(missing) == 0 {
			continue
		}
		if err := b.client.EnableRTSP(cam, missing); err != nil {
			// Non-admin users can't PATCH cameras; degrade to whatever is
			// already enabled.
			logger.Warn("Could not auto-enable RTSP (non-admin user?)", "camera", cam.Name, "error", err)
			continue
		}
		changed = true
	}

	if !changed {
		return bs, nil
	}
	return b.client.Bootstrap()
}

func (b *Bridge) hasUsableChannel(cam protect.Camera) bool {
	for _, ch := range cam.Channels {
		if ch.Enabled && ch.IsRtspEnabled && ch.RtspAlias != "" {
			return true
		}
	}
	return false
}

func (b *Bridge) buildCamera(cam protect.Camera) *CameraAccessory {
	id := cam.ID
	str := newStreamer(cam.Name, b.cfg.FFmpeg.Path, b.cfg.FFmpeg.AudioEnabled(), b.cfg.FFmpeg.Debug,
		func(width int) (string, error) {
			return b.streamURL(id, width)
		})

	acc := newCameraAccessory(cam, version.Version, str, b.cfg.Cameras.MotionSensorsEnabled())
	acc.Id = b.stableAID(cam.ID)

	b.cameras[cam.ID] = acc
	b.byAID[acc.Id] = acc
	return acc
}

// streamURL picks the best channel for the requested width and returns its
// RTSPS URL. Called on every stream start with fresh bootstrap state.
func (b *Bridge) streamURL(cameraID string, width int) (string, error) {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()

	cam, ok := b.camState[cameraID]
	if !ok {
		return "", fmt.Errorf("unknown camera %s", cameraID)
	}
	ch, ok := selectChannel(cam.Channels, width)
	if !ok {
		return "", fmt.Errorf("no RTSP-enabled channel on %s", cam.Name)
	}
	logger.Debug("Selected channel", "camera", cam.Name, "channel", ch.Name,
		"resolution", fmt.Sprintf("%dx%d", ch.Width, ch.Height), "requested_width", width)
	return b.client.RTSPSURL(b.bootstrap, ch.RtspAlias), nil
}

// selectChannel returns the smallest usable channel that still satisfies the
// requested width, falling back to the largest available one.
func selectChannel(channels []protect.Channel, width int) (protect.Channel, bool) {
	var best protect.Channel
	found := false
	for _, ch := range channels {
		if !ch.Enabled || !ch.IsRtspEnabled || ch.RtspAlias == "" {
			continue
		}
		if !found {
			best, found = ch, true
			continue
		}
		bestSatisfies := best.Width >= width
		chSatisfies := ch.Width >= width
		switch {
		case chSatisfies && !bestSatisfies:
			best = ch
		case chSatisfies && bestSatisfies && ch.Width < best.Width:
			best = ch
		case !chSatisfies && !bestSatisfies && ch.Width > best.Width:
			best = ch
		}
	}
	return best, found
}

// onBootstrap refreshes cached Protect state after every (re)connect.
func (b *Bridge) onBootstrap(bs *protect.Bootstrap) {
	b.applyBootstrap(bs)
	for _, cam := range bs.Cameras {
		if acc, ok := b.cameras[cam.ID]; ok {
			acc.syncState(cam)
			b.notifyUpdate(cam.ID)
		}
	}
}

func (b *Bridge) applyBootstrap(bs *protect.Bootstrap) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	b.bootstrap = bs
	for _, cam := range bs.Cameras {
		b.camState[cam.ID] = cam
	}
}

func (b *Bridge) onCameraUpdate(cameraID string, patch protect.CameraPatch) {
	acc, ok := b.cameras[cameraID]
	if !ok {
		return
	}
	acc.applyPatch(patch)

	b.stateMu.Lock()
	if cam, ok := b.camState[cameraID]; ok {
		if patch.State != nil {
			cam.State = *patch.State
		}
		if patch.IsMotionDetected != nil {
			cam.IsMotionDetected = *patch.IsMotionDetected
		}
		if patch.LastRing != nil {
			cam.LastRing = *patch.LastRing
		}
		if patch.LastMotion != nil {
			cam.LastMotion = *patch.LastMotion
		}
		b.camState[cameraID] = cam
	}
	b.stateMu.Unlock()

	b.notifyUpdate(cameraID)
}

// resourceRequest is the HAP snapshot request body (HAP spec 11.5).
type resourceRequest struct {
	ResourceType string `json:"resource-type"`
	ImageWidth   int    `json:"image-width"`
	ImageHeight  int    `json:"image-height"`
	AID          uint64 `json:"aid"`
}

// handleResource serves HomeKit snapshot requests from the Protect NVR (with
// a short-lived cache, since the Home app polls tiles frequently).
func (b *Bridge) handleResource(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req resourceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ResourceType != "image" {
		http.Error(w, "unsupported resource type", http.StatusBadRequest)
		return
	}

	acc, ok := b.byAID[req.AID]
	if !ok {
		http.Error(w, "unknown accessory", http.StatusNotFound)
		return
	}

	data, err := b.snapshot(acc.ProtectID, req.ImageWidth, req.ImageHeight)
	if err != nil {
		logger.Error("Snapshot failed", "camera", acc.Name(), "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(data)
}

func (b *Bridge) snapshot(cameraID string, width, height int) ([]byte, error) {
	b.snapMu.Lock()
	cached, ok := b.snapshots[cameraID]
	b.snapMu.Unlock()
	if ok && time.Since(cached.taken) < snapshotCacheTTL {
		return cached.data, nil
	}

	data, err := b.client.Snapshot(cameraID, width, height)
	if err != nil {
		// Serve a stale snapshot rather than a broken tile.
		if ok {
			return cached.data, nil
		}
		return nil, err
	}

	b.snapMu.Lock()
	b.snapshots[cameraID] = cachedSnapshot{data: data, taken: time.Now()}
	b.snapMu.Unlock()
	return data, nil
}

// logPairingInfo prints the setup code and a scannable QR code, since this
// bridge has no web UI.
func (b *Bridge) logPairingInfo() {
	uri := setupURI(b.cfg.HomeKit.Pin, categoryBridge, b.cfg.HomeKit.SetupID)
	logger.Info("HomeKit pairing", "pin", b.cfg.HomeKit.Pin, "uri", uri)

	if qr, err := qrcode.New(uri, qrcode.Medium); err == nil {
		fmt.Println(qr.ToSmallString(false))
	}
}

// stableAID derives a stable accessory ID from the Protect camera id so that
// adding/removing cameras doesn't renumber existing ones (which would break
// their HomeKit identity, rooms and automations). AID 1 is the bridge.
func (b *Bridge) stableAID(id string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	aid := h.Sum64()%1_000_000_000 + 2
	for b.usedAIDs[aid] {
		aid++
	}
	b.usedAIDs[aid] = true
	return aid
}

// setupURI builds the HomeKit "X-HM://" pairing payload (same encoding as
// HAP-NodeJS) used to render the pairing QR code.
func setupURI(pin string, category int, setupID string) string {
	code, _ := strconv.Atoi(normalizePin(pin))
	low := uint64(code) | (1 << 28) // bit 28: supports IP transport
	if category&1 == 1 {
		low |= 1 << 31
	}
	high := uint64(category >> 1)
	payload := high<<32 | low
	enc := strings.ToUpper(strconv.FormatUint(payload, 36))
	for len(enc) < 9 {
		enc = "0" + enc
	}
	return "X-HM://" + enc + setupID
}

// normalizePin strips formatting characters so a friendly "031-45-154" config
// value becomes the 8-digit code brutella/hap requires.
func normalizePin(pin string) string {
	var digits []rune
	for _, r := range pin {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	return string(digits)
}

// hapLogWriter routes brutella/hap's logger through go-logger so library
// messages share the application's log format and level.
type hapLogWriter struct{}

func (hapLogWriter) Write(p []byte) (int, error) {
	logger.Debug(strings.TrimRight(string(p), "\n"), "component", "hap")
	return len(p), nil
}

// routeHAPLogging strips brutella's own prefix/timestamp and forwards its Info
// output to go-logger. brutella's Debug logger is left disabled (it's very
// chatty HAP-protocol output) so this only relays the library's notable lines.
func routeHAPLogging() {
	hlog.Info.SetFlags(0)
	hlog.Info.SetPrefix("")
	hlog.Info.SetOutput(hapLogWriter{})

	// dnssd has its own logger (e.g. the "unable to wait for link updates" line).
	dlog.Info.SetFlags(0)
	dlog.Info.SetPrefix("")
	dlog.Info.SetOutput(hapLogWriter{})
}
