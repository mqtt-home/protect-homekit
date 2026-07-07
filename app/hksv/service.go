package hksv

// HomeKit services and characteristics for HomeKit Secure Video. brutella/hap
// ships the CameraRecordingManagement characteristics as stubs but not the
// CameraOperatingMode or DataStreamTransportManagement services, and none of
// the TLV8 payloads; these are built here from the library's generic C/S
// primitives with the HKSV type IDs.

import (
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
)

// Service type IDs.
const (
	typeCameraRecordingManagement     = "204"
	typeCameraOperatingMode           = "21A"
	typeDataStreamTransportManagement = "129"
)

// Characteristic type IDs.
const (
	typeActive                                = "B0"
	typeSupportedCameraRecordingConfiguration = "205"
	typeSupportedVideoRecordingConfiguration  = "206"
	typeSupportedAudioRecordingConfiguration  = "207"
	typeSelectedCameraRecordingConfiguration  = "209"
	typeRecordingAudioActive                  = "226"

	typeEventSnapshotsActive    = "223"
	typeHomeKitCameraActive     = "21B"
	typePeriodicSnapshotsActive = "225"
	typeThirdPartyCameraActive  = "21C"
	typeManuallyDisabled        = "227"

	typeSupportedDataStreamTransportConfiguration = "130"
	typeSetupDataStreamTransport                  = "131"
	typeDataStreamVersion                         = "37"
)

// Active characteristic values.
const (
	activeInactive = 0
	activeActive   = 1
)

// newUint8 builds a read/write/notify uint8 characteristic constrained to 0/1.
func newActiveChar(typ string, initial int) *characteristic.Int {
	c := characteristic.NewInt(typ)
	c.Format = characteristic.FormatUInt8
	c.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionWrite, characteristic.PermissionEvents}
	c.SetMinValue(0)
	c.SetMaxValue(1)
	c.SetStepValue(1)
	_ = c.SetValue(initial)
	return c
}

func newTLV8Char(typ string, perms []string) *characteristic.Bytes {
	c := characteristic.NewBytes(typ)
	c.Format = characteristic.FormatTLV8
	c.Permissions = perms
	c.SetValue([]byte{})
	return c
}

// RecordingManagement is the CameraRecordingManagement service.
type RecordingManagement struct {
	*service.S

	Active                *characteristic.Int
	SupportedCamera       *characteristic.Bytes
	SupportedVideo        *characteristic.Bytes
	SupportedAudio        *characteristic.Bytes
	SelectedConfiguration *characteristic.Bytes
	RecordingAudioActive  *characteristic.Int
}

func newRecordingManagement() *RecordingManagement {
	rw := []string{characteristic.PermissionRead, characteristic.PermissionWrite, characteristic.PermissionEvents}
	ro := []string{characteristic.PermissionRead, characteristic.PermissionEvents}

	s := &RecordingManagement{S: service.New(typeCameraRecordingManagement)}
	s.Active = newActiveChar(typeActive, activeInactive)
	s.SupportedCamera = newTLV8Char(typeSupportedCameraRecordingConfiguration, ro)
	s.SupportedVideo = newTLV8Char(typeSupportedVideoRecordingConfiguration, ro)
	s.SupportedAudio = newTLV8Char(typeSupportedAudioRecordingConfiguration, ro)
	s.SelectedConfiguration = newTLV8Char(typeSelectedCameraRecordingConfiguration, rw)
	s.RecordingAudioActive = newActiveChar(typeRecordingAudioActive, activeActive)

	s.AddC(s.Active.C)
	s.AddC(s.SupportedCamera.C)
	s.AddC(s.SupportedVideo.C)
	s.AddC(s.SupportedAudio.C)
	s.AddC(s.SelectedConfiguration.C)
	s.AddC(s.RecordingAudioActive.C)
	return s
}

// OperatingMode is the CameraOperatingMode service.
type OperatingMode struct {
	*service.S

	EventSnapshotsActive    *characteristic.Int
	HomeKitCameraActive     *characteristic.Int
	PeriodicSnapshotsActive *characteristic.Int
	ThirdPartyCameraActive  *characteristic.Int
	ManuallyDisabled        *characteristic.Int
}

func newOperatingMode() *OperatingMode {
	s := &OperatingMode{S: service.New(typeCameraOperatingMode)}
	s.EventSnapshotsActive = newActiveChar(typeEventSnapshotsActive, activeActive)
	s.HomeKitCameraActive = newActiveChar(typeHomeKitCameraActive, activeActive)
	s.PeriodicSnapshotsActive = newActiveChar(typePeriodicSnapshotsActive, activeActive)

	// ThirdPartyCameraActive and ManuallyDisabled are read-only status.
	s.ThirdPartyCameraActive = characteristic.NewInt(typeThirdPartyCameraActive)
	s.ThirdPartyCameraActive.Format = characteristic.FormatUInt8
	s.ThirdPartyCameraActive.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionEvents}
	_ = s.ThirdPartyCameraActive.SetValue(0)

	s.ManuallyDisabled = characteristic.NewInt(typeManuallyDisabled)
	s.ManuallyDisabled.Format = characteristic.FormatUInt8
	s.ManuallyDisabled.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionEvents}
	s.ManuallyDisabled.SetMinValue(0)
	s.ManuallyDisabled.SetMaxValue(1)
	s.ManuallyDisabled.SetStepValue(1)
	_ = s.ManuallyDisabled.SetValue(0)

	s.AddC(s.EventSnapshotsActive.C)
	s.AddC(s.HomeKitCameraActive.C)
	s.AddC(s.PeriodicSnapshotsActive.C)
	s.AddC(s.ThirdPartyCameraActive.C)
	s.AddC(s.ManuallyDisabled.C)
	return s
}

// DataStreamManagement is the DataStreamTransportManagement service.
type DataStreamManagement struct {
	*service.S

	SupportedConfiguration *characteristic.Bytes
	SetupTransport         *characteristic.Bytes
	Version                *characteristic.String
}

func newDataStreamManagement() *DataStreamManagement {
	s := &DataStreamManagement{S: service.New(typeDataStreamTransportManagement)}

	s.SupportedConfiguration = newTLV8Char(typeSupportedDataStreamTransportConfiguration,
		[]string{characteristic.PermissionRead, characteristic.PermissionEvents})
	s.SupportedConfiguration.SetValue(buildSupportedDataStreamTransportConfiguration())

	// SetupDataStreamTransport is written by the controller and read back for the
	// response; it must not send change events.
	s.SetupTransport = characteristic.NewBytes(typeSetupDataStreamTransport)
	s.SetupTransport.Format = characteristic.FormatTLV8
	s.SetupTransport.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionWrite}
	s.SetupTransport.SetValue([]byte{})

	s.Version = characteristic.NewString(typeDataStreamVersion)
	s.Version.Permissions = []string{characteristic.PermissionRead}
	s.Version.SetValue("1.0")

	s.AddC(s.SupportedConfiguration.C)
	s.AddC(s.SetupTransport.C)
	s.AddC(s.Version.C)
	return s
}
