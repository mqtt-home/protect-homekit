package hksv

// Manager wires the HKSV services, the recording pipeline and the HDS transport
// together for a single camera. It owns the three HomeKit services to add to the
// camera accessory and reacts to the controller writing the recording
// configuration, toggling recording active and setting up data streams.

import (
	"context"
	"crypto/rand"
	"net/http"
	"strconv"
	"sync"

	"github.com/brutella/hap"
	"github.com/brutella/hap/service"
	"github.com/philipparndt/go-logger"
)

// Options configures a camera's HKSV support.
type Options struct {
	CameraName  string
	FFmpegPath  string
	Debug       bool
	Resolve     URLResolver  // resolves the RTSPS URL for a requested width
	Resolutions []Resolution // advertised recording resolutions
	HasMotion   bool         // advertise the motion event trigger
	IsDoorbell  bool         // advertise the doorbell event trigger
	HasMic      bool         // camera can provide audio for recordings
	PrebufferMS int          // prebuffer length (default 4000)
	FragmentMS  int          // media fragment length (default 4000)
}

// Manager owns the HKSV services and recording lifecycle for one camera.
type Manager struct {
	Recording  *RecordingManagement
	Operating  *OperatingMode
	DataStream *DataStreamManagement

	opts    Options
	rec     *recorder
	hasMic  bool
	audioOn bool

	ctx    context.Context
	cancel context.CancelFunc

	// mu guards the reconcile-relevant fields below.
	mu       sync.Mutex
	selected SelectedConfig
	hasSel   bool
}

// NewManager builds the services and recording pipeline for a camera.
func NewManager(opts Options) *Manager {
	if opts.PrebufferMS <= 0 {
		opts.PrebufferMS = 4000
	}
	if opts.FragmentMS <= 0 {
		opts.FragmentMS = 4000
	}
	if len(opts.Resolutions) == 0 {
		opts.Resolutions = DefaultResolutions()
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		Recording:  newRecordingManagement(),
		Operating:  newOperatingMode(),
		DataStream: newDataStreamManagement(),
		opts:       opts,
		rec:        newRecorder(opts.CameraName, opts.FFmpegPath, opts.Debug, opts.Resolve),
		hasMic:     opts.HasMic,
		audioOn:    opts.HasMic,
		ctx:        ctx,
		cancel:     cancel,
	}

	// HomeKit needs the data-stream transport linked to the recording
	// management service to know where to open the recording stream. Without
	// this linked-service relationship the controller refuses to enable
	// recording (erroring before it ever contacts the accessory).
	m.Recording.AddS(m.DataStream.S)

	// Reflect audio availability so the controller doesn't expect audio the
	// camera can't provide.
	if !opts.HasMic {
		_ = m.Recording.RecordingAudioActive.SetValue(activeInactive)
	}

	m.advertiseSupported()
	m.wireCallbacks()
	return m
}

// Services returns the HKSV services to add to the camera accessory.
func (m *Manager) Services() []*service.S {
	return []*service.S{m.Recording.S, m.Operating.S, m.DataStream.S}
}

// LinkTriggerService links the event-trigger service (the motion sensor or
// doorbell) to the recording management service, so HomeKit associates the
// trigger with recording. Call after the accessory's trigger service exists.
func (m *Manager) LinkTriggerService(s *service.S) {
	if s != nil {
		m.Recording.AddS(s)
	}
}

// Close stops recording and tears down any active data streams.
func (m *Manager) Close() {
	m.cancel()
	m.rec.disable()
}

// advertiseSupported publishes the static Supported* recording configurations.
func (m *Manager) advertiseSupported() {
	var triggers uint32
	if m.opts.HasMotion {
		triggers |= EventTriggerMotion
	}
	if m.opts.IsDoorbell {
		triggers |= EventTriggerDoorbell
	}

	m.Recording.SupportedCamera.SetValue(
		buildSupportedCameraRecordingConfiguration(m.opts.PrebufferMS, m.opts.FragmentMS, triggers))
	m.Recording.SupportedVideo.SetValue(
		buildSupportedVideoRecordingConfiguration(
			[]byte{H264ProfileBaseline, H264ProfileMain, H264ProfileHigh},
			[]byte{H264Level31, H264Level32, H264Level40},
			m.opts.Resolutions))
	m.Recording.SupportedAudio.SetValue(
		buildSupportedAudioRecordingConfiguration(AudioCodecAACLC, 1,
			[]byte{SampleRate16kHz, SampleRate24kHz, SampleRate32kHz}))
}

// wireCallbacks connects the characteristic writes to recording state.
func (m *Manager) wireCallbacks() {
	m.Recording.Active.OnValueRemoteUpdate(func(int) { m.reconcile() })
	m.Operating.HomeKitCameraActive.OnValueRemoteUpdate(func(int) { m.reconcile() })
	m.Recording.RecordingAudioActive.OnValueRemoteUpdate(func(v int) {
		m.mu.Lock()
		m.audioOn = v == activeActive && m.hasMic
		m.mu.Unlock()
		m.reconcile()
	})
	m.Recording.SelectedConfiguration.OnValueRemoteUpdate(func(v []byte) {
		sc, err := parseSelectedConfig(v)
		if err != nil {
			logger.Error("HKSV: parse selected configuration", "camera", m.opts.CameraName, "error", err)
			return
		}
		m.mu.Lock()
		m.selected = sc
		m.hasSel = true
		m.mu.Unlock()
		logger.Info("HKSV recording configured", "camera", m.opts.CameraName,
			"resolution", resolutionString(sc), "prebuffer_ms", sc.PrebufferMS, "fragment_ms", sc.FragmentLengthMS)
		m.reconcile()
	})

	// The controller writes SetupDataStreamTransport to open a recording data
	// stream. The value it reads back must contain the listening port and the
	// accessory key salt.
	m.DataStream.SetupTransport.OnValueUpdate(func(newValue, _ []byte, r *http.Request) {
		if r == nil {
			return
		}
		m.handleSetupDataStream(newValue, r)
	})
}

// reconcile enables or disables the prebuffer based on the current state.
func (m *Manager) reconcile() {
	m.mu.Lock()
	sel := m.selected
	hasSel := m.hasSel
	audio := m.audioOn
	m.mu.Unlock()

	if m.canRecord() && hasSel {
		m.rec.enable(sel, audio)
	} else {
		m.rec.disable()
	}
}

// canRecord reports whether recording is currently permitted.
func (m *Manager) canRecord() bool {
	return m.Recording.Active.Value() == activeActive &&
		m.Operating.HomeKitCameraActive.Value() == activeActive
}

// hasConfig reports whether the controller has selected a configuration.
func (m *Manager) hasConfig() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasSel
}

// handleSetupDataStream derives the HDS keys, starts a listener and answers with
// the port and accessory salt.
func (m *Manager) handleSetupDataStream(value []byte, r *http.Request) {
	req, err := parseSetupDataStreamRequest(value)
	if err != nil {
		logger.Error("HKSV: parse SetupDataStreamTransport", "camera", m.opts.CameraName, "error", err)
		return
	}
	if req.command != commandStartSession || req.transportType != transportHomeKitDataStream {
		logger.Debug("HKSV: unsupported data stream setup", "camera", m.opts.CameraName,
			"command", req.command, "transport", req.transportType)
		return
	}

	shared, ok := hap.SharedKeyForRequest(r)
	if !ok {
		logger.Error("HKSV: no HAP session for data stream setup", "camera", m.opts.CameraName)
		return
	}

	accessorySalt := make([]byte, 32)
	if _, err := rand.Read(accessorySalt); err != nil {
		logger.Error("HKSV: generate accessory salt", "camera", m.opts.CameraName, "error", err)
		return
	}

	keys, err := deriveHDSKeys(shared, req.controllerKey, accessorySalt)
	if err != nil {
		logger.Error("HKSV: derive data stream keys", "camera", m.opts.CameraName, "error", err)
		return
	}

	srv, err := NewServer(keys, m.rec, m.opts.CameraName, m.canRecord, m.hasConfig)
	if err != nil {
		logger.Error("HKSV: start data stream listener", "camera", m.opts.CameraName, "error", err)
		return
	}

	// Publish the response so the controller's read-back returns the port/salt.
	m.DataStream.SetupTransport.SetValue(buildSetupDataStreamResponse(uint16(srv.Port()), accessorySalt))
	logger.Debug("HKSV data stream set up", "camera", m.opts.CameraName, "port", srv.Port())

	go srv.Serve(m.ctx)
}

// DefaultResolutions returns the recording resolutions advertised by default.
func DefaultResolutions() []Resolution {
	return []Resolution{
		{1920, 1080, 24},
		{1280, 720, 24},
		{640, 480, 24},
		{320, 240, 24},
	}
}

func resolutionString(sc SelectedConfig) string {
	return strconv.Itoa(sc.Video.Width) + "x" + strconv.Itoa(sc.Video.Height)
}
