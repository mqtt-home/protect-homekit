package bridge

import (
	"crypto/rand"
	"encoding/binary"
	"net/http"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/rtp"
	"github.com/brutella/hap/service"
	"github.com/brutella/hap/tlv8"
	"github.com/mqtt-home/protect-homekit/hksv"
	"github.com/mqtt-home/protect-homekit/protect"
	"github.com/philipparndt/go-logger"
)

// CameraAccessory is one Protect camera exposed to HomeKit: live stream,
// snapshot source, optional motion sensor and — for doorbell models — a
// doorbell service.
type CameraAccessory struct {
	*accessory.A

	Stream     *service.CameraRTPStreamManagement
	Microphone *service.Microphone
	Motion     *service.MotionSensor
	Doorbell   *service.Doorbell

	ProtectID string
	streamer  *streamer
	// hksv is the HomeKit Secure Video manager, nil when HKSV is disabled.
	hksv *hksv.Manager

	// lastRing tracks the ring timestamp so websocket patches only fire the
	// doorbell on an actual change.
	lastRing int64
}

func newCameraAccessory(cam protect.Camera, firmwareFallback string, str *streamer, motionSensor bool, secureVideo *hksv.Manager) *CameraAccessory {
	category := accessory.TypeIPCamera
	if cam.IsDoorbell() {
		category = accessory.TypeVideoDoorbell
	}

	firmware := cam.FirmwareVersion
	if firmware == "" {
		firmware = firmwareFallback
	}

	a := &CameraAccessory{
		A: accessory.New(accessory.Info{
			Name:         cam.Name,
			Manufacturer: "Ubiquiti",
			Model:        cam.Type,
			SerialNumber: cam.Mac,
			Firmware:     firmware,
		}, category),
		ProtectID: cam.ID,
		streamer:  str,
		lastRing:  cam.LastRing,
	}

	if cam.IsDoorbell() {
		a.Doorbell = service.NewDoorbell()
		a.Doorbell.Primary = true
		a.AddS(a.Doorbell.S)
	}

	a.Stream = service.NewCameraRTPStreamManagement()
	a.AddS(a.Stream.S)
	a.setupStreamManagement()

	if cam.FeatureFlags.HasMic || cam.IsMicEnabled {
		a.Microphone = service.NewMicrophone()
		a.AddS(a.Microphone.S)
	}

	if motionSensor {
		a.Motion = service.NewMotionSensor()
		a.Motion.MotionDetected.SetValue(cam.IsMotionDetected)
		a.AddS(a.Motion.S)
	}

	// HomeKit Secure Video: attach the recording, operating-mode and data-stream
	// services. The home hub triggers recording off the motion sensor above.
	if secureVideo != nil {
		a.hksv = secureVideo
		for _, s := range secureVideo.Services() {
			a.AddS(s)
		}
	}

	return a
}

// setupStreamManagement wires the HomeKit RTP stream negotiation (TLV8-based
// SetupEndpoints / SelectedRTPStreamConfiguration dance) to the ffmpeg
// streamer. Same flow as brutella/hkcam.
func (a *CameraAccessory) setupStreamManagement() {
	m := a.Stream

	setTLV8Payload(m.StreamingStatus.Bytes, rtp.StreamingStatus{Status: rtp.StreamingStatusAvailable})
	setTLV8Payload(m.SupportedRTPConfiguration.Bytes, rtp.NewConfiguration(rtp.CryptoSuite_AES_CM_128_HMAC_SHA1_80))
	setTLV8Payload(m.SupportedVideoStreamConfiguration.Bytes, rtp.DefaultVideoStreamConfiguration())
	// Only Opus: transcoding to AAC-ELD would need a libfdk build of ffmpeg.
	setTLV8Payload(m.SupportedAudioStreamConfiguration.Bytes, rtp.AudioStreamConfiguration{
		Codecs:       []rtp.AudioCodecConfiguration{rtp.NewOpusAudioCodecConfiguration()},
		ComfortNoise: false,
	})

	m.SetupEndpoints.OnValueUpdate(func(new, old []byte, r *http.Request) {
		if r == nil {
			return
		}

		var req rtp.SetupEndpoints
		if err := tlv8.Unmarshal(new, &req); err != nil {
			logger.Error("SetupEndpoints: unmarshal tlv8", "camera", a.Name(), "error", err)
			return
		}

		iface, err := ifaceOfRequest(r)
		if err != nil {
			logger.Error("SetupEndpoints: interface lookup", "camera", a.Name(), "error", err)
			return
		}
		ip, err := ipAtInterface(*iface, req.ControllerAddr.IPVersion)
		if err != nil {
			logger.Error("SetupEndpoints: ip lookup", "camera", a.Name(), "error", err)
			return
		}

		resp := rtp.SetupEndpointsResponse{
			SessionId: req.SessionId,
			Status:    rtp.SessionStatusSuccess,
			AccessoryAddr: rtp.Addr{
				IPVersion:    req.ControllerAddr.IPVersion,
				IPAddr:       ip.String(),
				VideoRtpPort: req.ControllerAddr.VideoRtpPort,
				AudioRtpPort: req.ControllerAddr.AudioRtpPort,
			},
			Video:     req.Video,
			Audio:     req.Audio,
			SsrcVideo: randomSsrc(),
			SsrcAudio: randomSsrc(),
		}

		a.streamer.prepare(req, resp)
		setTLV8Payload(m.SetupEndpoints.Bytes, resp)
	})

	m.SelectedRTPStreamConfiguration.OnValueRemoteUpdate(func(buf []byte) {
		var cfg rtp.StreamConfiguration
		if err := tlv8.Unmarshal(buf, &cfg); err != nil {
			logger.Error("SelectedRTPStreamConfiguration: unmarshal tlv8", "camera", a.Name(), "error", err)
			return
		}

		id := cfg.Command.Identifier
		switch cfg.Command.Type {
		case rtp.SessionControlCommandTypeStart:
			if err := a.streamer.start(id, cfg.Video, cfg.Audio); err != nil {
				logger.Error("Starting stream", "camera", a.Name(), "error", err)
			}
		case rtp.SessionControlCommandTypeSuspend:
			a.streamer.suspend(id)
		case rtp.SessionControlCommandTypeResume:
			a.streamer.resume(id)
		case rtp.SessionControlCommandTypeReconfigure:
			// Video is copied 1:1 from the camera, so there is nothing to
			// reconfigure.
			logger.Debug("Stream reconfigure ignored (video is remuxed, not transcoded)", "camera", a.Name())
		case rtp.SessionControlCommandTypeEnd:
			a.streamer.stop(id)
		default:
			logger.Debug("Unknown stream command", "camera", a.Name(), "type", cfg.Command.Type)
		}
	})
}

// applyPatch updates HomeKit state from a partial camera update received over
// the Protect websocket.
func (a *CameraAccessory) applyPatch(patch protect.CameraPatch) {
	if patch.IsMotionDetected != nil && a.Motion != nil {
		a.Motion.MotionDetected.SetValue(*patch.IsMotionDetected)
		logger.Debug("Motion", "camera", a.Name(), "detected", *patch.IsMotionDetected)
	}

	if patch.LastRing != nil && *patch.LastRing != a.lastRing {
		a.lastRing = *patch.LastRing
		if a.Doorbell != nil {
			a.Doorbell.ProgrammableSwitchEvent.SetValue(characteristic.ProgrammableSwitchEventSinglePress)
			logger.Info("Doorbell ring", "camera", a.Name())
		}
	}

	if patch.State != nil {
		logger.Info("Camera state changed", "camera", a.Name(), "state", *patch.State)
	}
}

// syncState re-aligns HomeKit state after a re-bootstrap (reconnect).
func (a *CameraAccessory) syncState(cam protect.Camera) {
	if a.Motion != nil {
		a.Motion.MotionDetected.SetValue(cam.IsMotionDetected)
	}
	// Don't fire the doorbell for rings that happened while disconnected;
	// just take over the timestamp.
	a.lastRing = cam.LastRing
}

// randomSsrc returns a random RTP SSRC in [1, MaxInt32]. ffmpeg's RTP muxer
// rejects negative ssrc values, so the sign bit must stay clear.
func randomSsrc() int32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	v := int32(binary.BigEndian.Uint32(b[:]) & 0x7fffffff)
	if v == 0 {
		v = 1
	}
	return v
}

func setTLV8Payload(c *characteristic.Bytes, v any) {
	if payload, err := tlv8.Marshal(v); err == nil {
		c.SetValue(payload)
	} else {
		logger.Error("Marshaling TLV8 payload", "error", err)
	}
}
