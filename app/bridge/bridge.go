package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	dlog "github.com/brutella/dnssd/log"
	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	hlog "github.com/brutella/hap/log"
	"github.com/mqtt-home/protect-homekit/config"
	"github.com/mqtt-home/protect-homekit/hksv"
	"github.com/mqtt-home/protect-homekit/protect"
	"github.com/mqtt-home/protect-homekit/version"
	"github.com/philipparndt/go-logger"
	qrcode "github.com/skip2/go-qrcode"
)

// categoryBridge is the HomeKit accessory category for a bridge.
const categoryBridge = 2

// hapProtocolVersion is advertised in the mDNS TXT record ("pv"). brutella/hap
// defaults to 1.0, but HomeKit Data Stream / Secure Video are HAP 1.1 features
// and controllers gate them on the advertised version (HAP-NodeJS also
// advertises 1.1).
const hapProtocolVersion = "1.1"

// snapshotCacheTTL bounds how often the NVR is asked for a fresh snapshot;
// the Home app refreshes tiles aggressively.
const snapshotCacheTTL = 3 * time.Second

// Bridge exposes UniFi Protect cameras as HomeKit accessories using
// brutella/hap.
type Bridge struct {
	cfg    config.Config
	client *protect.Client
	events *protect.Events

	// servers holds the running HAP servers. In bridged mode there is one (a
	// bridge with all cameras); in standalone mode (secure_video, required for
	// HKSV) there is one per camera, since HomeKit Secure Video does not work
	// on bridged camera accessories.
	servers    []*hapServer
	standalone bool
	bridgeAcc  *accessory.Bridge // bridged mode only
	cancel     context.CancelFunc

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

// hapServer is one running HAP accessory server plus the pairing details needed
// to render its QR code.
type hapServer struct {
	server   *hap.Server
	done     chan struct{} // closed when the server has fully stopped
	label    string        // bridge name, or camera name in standalone mode
	cameraID string        // set in standalone mode
	pin      string
	setupID  string
	category int
	port     int
}

// CameraPairing is the pairing info for one standalone camera accessory, used
// by the web UI to show a per-camera "Connect" QR code.
type CameraPairing struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Pin      string `json:"pin"`
	SetupURI string `json:"setup_uri"`
	Port     int    `json:"port"`
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

	b.standalone = b.cfg.Cameras.SecureVideoEnabled()

	type builtCamera struct {
		acc *CameraAccessory
		cam protect.Camera
	}
	var built []builtCamera
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
		built = append(built, builtCamera{acc, cam})
		logger.Info("Configured camera", "camera", cam.Name, "type", cam.Type,
			"doorbell", cam.IsDoorbell(), "aid", acc.Id)
	}
	if len(built) == 0 {
		return fmt.Errorf("no cameras to bridge")
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel

	if b.standalone {
		// One HAP server per camera. HKSV only works on standalone camera
		// accessories, not bridged ones.
		for i, bc := range built {
			if err := b.startCameraServer(ctx, bc.acc, bc.cam, i); err != nil {
				return err
			}
		}
	} else {
		accs := make([]*accessory.A, 0, len(built))
		for _, bc := range built {
			accs = append(accs, bc.acc.A)
		}
		if err := b.startBridge(ctx, bs, accs); err != nil {
			return err
		}
	}

	b.events.OnBootstrap = b.onBootstrap
	b.events.OnCameraUpdate = b.onCameraUpdate
	go b.events.Run(ctx, bs)

	b.logPairingInfo()
	logger.Info("protect-homekit bridge started", "cameras", len(b.cameras), "standalone", b.standalone)
	return nil
}

// startBridge starts the single bridged HAP server (secure_video disabled).
func (b *Bridge) startBridge(ctx context.Context, bs *protect.Bootstrap, accs []*accessory.A) error {
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
	server.Protocol = hapProtocolVersion
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
	// JPEGs straight from the NVR (AID identifies the camera).
	server.ServeMux().HandleFunc("/resource", b.handleResource)

	hs := &hapServer{
		server:   server,
		done:     make(chan struct{}),
		label:    b.cfg.HomeKit.BridgeName,
		pin:      b.cfg.HomeKit.Pin,
		setupID:  b.cfg.HomeKit.SetupID,
		category: categoryBridge,
		port:     b.cfg.HomeKit.Port,
	}
	b.servers = append(b.servers, hs)
	b.serve(ctx, hs, b.cfg.HomeKit.StorageDir)
	return nil
}

// startCameraServer starts one standalone HAP server for a single camera
// (secure_video enabled). Each camera gets its own pairing store, port
// (homekit.port + index) and setup id, so it is added to Home individually.
func (b *Bridge) startCameraServer(ctx context.Context, acc *CameraAccessory, cam protect.Camera, index int) error {
	// A standalone accessory is its own primary; brutella assigns AID 1.
	acc.Id = 1

	category := int(accessory.TypeIPCamera)
	if cam.IsDoorbell() {
		category = int(accessory.TypeVideoDoorbell)
	}

	port := 0
	if b.cfg.HomeKit.Port > 0 {
		port = b.cfg.HomeKit.Port + index
	}
	setupID := cameraSetupID(cam.ID)
	storeDir := filepath.Join(b.cfg.HomeKit.StorageDir, "cam-"+sanitizeID(cam.ID))

	server, err := hap.NewServer(hap.NewFsStore(storeDir), acc.A)
	if err != nil {
		return fmt.Errorf("create HAP server for %s: %w", cam.Name, err)
	}
	server.Pin = normalizePin(b.cfg.HomeKit.Pin)
	server.Protocol = hapProtocolVersion
	server.SetupId = setupID
	if port > 0 {
		server.Addr = fmt.Sprintf(":%d", port)
	}
	if len(b.cfg.HomeKit.Interfaces) > 0 {
		server.Ifaces = b.cfg.HomeKit.Interfaces
	}
	// Snapshot requests arrive on this camera's own server, so bind the handler
	// to this camera id rather than relying on the AID.
	id := cam.ID
	server.ServeMux().HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		b.serveSnapshot(w, r, id)
	})

	hs := &hapServer{
		server:   server,
		done:     make(chan struct{}),
		label:    cam.Name,
		cameraID: cam.ID,
		pin:      b.cfg.HomeKit.Pin,
		setupID:  setupID,
		category: category,
		port:     port,
	}
	b.servers = append(b.servers, hs)
	b.serve(ctx, hs, storeDir)
	return nil
}

// serve launches a HAP server in the background until ctx is cancelled.
func (b *Bridge) serve(ctx context.Context, hs *hapServer, storeDir string) {
	go func() {
		defer close(hs.done)
		logger.Info("Starting HAP server", "accessory", hs.label, "port", hs.port, "storage", storeDir)
		if err := hs.server.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
			logger.Error("HAP server stopped", "accessory", hs.label, "error", err)
		}
	}()
}

// SetUpdateListener registers a callback fired whenever a camera's state
// changes (used by the web UI). Must be called before Start.
func (b *Bridge) SetUpdateListener(fn func(CameraInfo)) { b.onUpdate = fn }

func (b *Bridge) BridgeName() string { return b.cfg.HomeKit.BridgeName }
func (b *Bridge) Pin() string        { return b.cfg.HomeKit.Pin }
func (b *Bridge) SetupID() string    { return b.cfg.HomeKit.SetupID }
func (b *Bridge) Healthy() bool      { return len(b.servers) > 0 }

// Standalone reports whether cameras are published as individual accessories
// (secure_video enabled), in which case each has its own pairing QR.
func (b *Bridge) Standalone() bool { return b.standalone }

// CameraPairings returns per-camera pairing info for the web UI "Connect"
// buttons. Empty in bridged mode (there is a single bridge QR instead).
func (b *Bridge) CameraPairings() []CameraPairing {
	if !b.standalone {
		return nil
	}
	out := make([]CameraPairing, 0, len(b.servers))
	for _, hs := range b.servers {
		if hs.cameraID == "" {
			continue
		}
		out = append(out, CameraPairing{
			ID:       hs.cameraID,
			Name:     hs.label,
			Pin:      hs.pin,
			SetupURI: setupURI(hs.pin, hs.category, hs.setupID),
			Port:     hs.port,
		})
	}
	return out
}

// CameraSetupURI returns the pairing URI for a single standalone camera.
func (b *Bridge) CameraSetupURI(cameraID string) (string, bool) {
	for _, hs := range b.servers {
		if hs.cameraID == cameraID {
			return setupURI(hs.pin, hs.category, hs.setupID), true
		}
	}
	return "", false
}

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
		if acc.hksv != nil {
			acc.hksv.Close()
		}
	}
	deadline := time.After(5 * time.Second)
	for _, hs := range b.servers {
		select {
		case <-hs.done:
		case <-deadline:
			logger.Warn("Timed out waiting for HAP server shutdown", "accessory", hs.label)
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

	var secureVideo *hksv.Manager
	if b.cfg.Cameras.SecureVideoEnabled() {
		secureVideo = hksv.NewManager(hksv.Options{
			CameraName: cam.Name,
			FFmpegPath: b.cfg.FFmpeg.Path,
			Debug:      b.cfg.FFmpeg.Debug,
			Resolve:    func(width int) (string, error) { return b.streamURL(id, width) },
			HasMotion:  b.cfg.Cameras.MotionSensorsEnabled(),
			IsDoorbell: cam.IsDoorbell(),
			HasMic:     b.cfg.FFmpeg.AudioEnabled() && (cam.FeatureFlags.HasMic || cam.IsMicEnabled),
		})
	}

	acc := newCameraAccessory(cam, version.Version, str, b.cfg.Cameras.MotionSensorsEnabled(), secureVideo)
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

// handleResource serves HomeKit snapshot requests on the bridged server, where
// the AID identifies which camera the request is for.
func (b *Bridge) handleResource(w http.ResponseWriter, r *http.Request) {
	req, ok := parseResourceRequest(w, r)
	if !ok {
		return
	}
	acc, ok := b.byAID[req.AID]
	if !ok {
		http.Error(w, "unknown accessory", http.StatusNotFound)
		return
	}
	b.writeSnapshot(w, acc.ProtectID, acc.Name(), req.ImageWidth, req.ImageHeight)
}

// serveSnapshot serves a snapshot request on a standalone camera server, where
// the camera is fixed (the request arrives on that camera's own server).
func (b *Bridge) serveSnapshot(w http.ResponseWriter, r *http.Request, cameraID string) {
	req, ok := parseResourceRequest(w, r)
	if !ok {
		return
	}
	b.writeSnapshot(w, cameraID, cameraID, req.ImageWidth, req.ImageHeight)
}

func parseResourceRequest(w http.ResponseWriter, r *http.Request) (resourceRequest, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return resourceRequest{}, false
	}
	var req resourceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return resourceRequest{}, false
	}
	if req.ResourceType != "image" {
		http.Error(w, "unsupported resource type", http.StatusBadRequest)
		return resourceRequest{}, false
	}
	return req, true
}

func (b *Bridge) writeSnapshot(w http.ResponseWriter, cameraID, label string, width, height int) {
	data, err := b.snapshot(cameraID, width, height)
	if err != nil {
		logger.Error("Snapshot failed", "camera", label, "error", err)
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

// logPairingInfo prints the setup code and a scannable QR code for each HAP
// server (one for the bridge, or one per camera in standalone mode).
func (b *Bridge) logPairingInfo() {
	for _, hs := range b.servers {
		uri := setupURI(hs.pin, hs.category, hs.setupID)
		logger.Info("HomeKit pairing", "accessory", hs.label, "pin", hs.pin, "uri", uri)
		if qr, err := qrcode.New(uri, qrcode.Medium); err == nil {
			fmt.Println(hs.label + ":")
			fmt.Println(qr.ToSmallString(false))
		}
	}
}

// cameraSetupID derives a stable 4-character HomeKit setup id from a camera id,
// so each standalone camera advertises a distinct pairing QR.
func cameraSetupID(cameraID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(cameraID))
	v := h.Sum32()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var out [4]byte
	for i := range out {
		out[i] = alphabet[v%uint32(len(alphabet))]
		v /= uint32(len(alphabet))
	}
	return string(out[:])
}

// sanitizeID makes a camera id safe for use as a directory name.
func sanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, id)
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
