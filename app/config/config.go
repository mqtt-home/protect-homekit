package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/philipparndt/go-logger"
	yaml "sigs.k8s.io/yaml"
)

type Config struct {
	Protect  ProtectConfig `json:"protect"`
	HomeKit  HomeKitConfig `json:"homekit"`
	Cameras  CamerasConfig `json:"cameras"`
	FFmpeg   FFmpegConfig  `json:"ffmpeg"`
	Web      WebConfig     `json:"web"`
	Pprof    PprofConfig   `json:"pprof"`
	LogLevel string        `json:"loglevel,omitempty"`
}

// WebConfig enables the status/monitoring web UI (camera overview, pairing
// QR code, SSE live updates).
type WebConfig struct {
	Enabled bool `json:"enabled"`
	// Port defaults to 8080.
	Port int `json:"port,omitempty"`
	// LivenessGraceSeconds is how long /api/livez keeps reporting OK while
	// the bridge is unhealthy (restart dampening). Defaults to 4 minutes.
	LivenessGraceSeconds int `json:"liveness_grace_seconds,omitempty"`
}

// ProtectConfig points at the UniFi OS console running Protect (e.g. a
// UDM/UNVR). Use a dedicated local user with access to Protect — not a
// Ubiquiti cloud account, and not one with 2FA enabled.
type ProtectConfig struct {
	// Host is the base URL of the console, e.g. "https://192.168.1.1".
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
	// VerifySSL enables TLS certificate verification. Consoles ship with
	// self-signed certificates, so this defaults to false.
	VerifySSL bool `json:"verify_ssl,omitempty"`
}

type HomeKitConfig struct {
	// BridgeName is the HomeKit bridge name shown in the Home app.
	BridgeName string `json:"bridge_name,omitempty"`
	// Pin is the 8-digit setup code, format "XXX-XX-XXX".
	Pin string `json:"pin,omitempty"`
	// SetupID is the 4-char HomeKit setup id (optional; affects the QR/URI only).
	SetupID string `json:"setup_id,omitempty"`
	// StorageDir holds the persisted pairing keys/state. MUST survive restarts
	// (mount a volume in Kubernetes) or HomeKit pairing is lost on every restart.
	// Defaults to "<config-dir>/hap".
	StorageDir string `json:"storage_dir,omitempty"`
	// Port is the HAP TCP port. 0 lets the OS choose (fine unless you need a
	// stable port through a firewall with hostNetwork).
	Port int `json:"port,omitempty"`
	// Interfaces restricts the network interfaces the bridge advertises on
	// (e.g. ["en0"]). On a multi-homed host, leaving this empty makes the
	// controller try unreachable secondary/VM IPs and fail to connect. Empty
	// means all interfaces.
	Interfaces []string `json:"interfaces,omitempty"`
}

// CamerasConfig selects and shapes the cameras taken over from Protect.
type CamerasConfig struct {
	// Include limits the bridge to the listed cameras (by Protect name, id or
	// MAC, case-insensitive). Empty means all cameras.
	Include []string `json:"include,omitempty"`
	// Exclude removes cameras from the bridge (by name, id or MAC).
	Exclude []string `json:"exclude,omitempty"`
	// MotionSensors adds a HomeKit motion sensor per camera fed by Protect
	// motion events. Enabled by default.
	MotionSensors *bool `json:"motion_sensors,omitempty"`
	// AutoEnableRTSP turns on the RTSPS stream of camera channels in Protect
	// when it is disabled (same behavior as homebridge-unifi-protect). Requires
	// an admin user. Enabled by default; when disabled, enable the streams
	// manually in the Protect UI (camera > Advanced > RTSP).
	AutoEnableRTSP *bool `json:"auto_enable_rtsp,omitempty"`
	// ForceH264 switches cameras that use Protect "enhanced encoding" (H.265)
	// back to Standard/H.264. HomeKit streaming only supports H.264, and this
	// bridge deliberately never transcodes video, so H.265 cameras cannot
	// stream without this. Note: this also affects Protect recordings (H.264
	// needs more storage). Enabled by default; disable to keep H.265 and
	// switch cameras manually in the Protect UI (Camera > Video > Encoding).
	ForceH264 *bool `json:"force_h264,omitempty"`
}

func (c CamerasConfig) MotionSensorsEnabled() bool {
	return c.MotionSensors == nil || *c.MotionSensors
}

func (c CamerasConfig) AutoEnableRTSPEnabled() bool {
	return c.AutoEnableRTSP == nil || *c.AutoEnableRTSP
}

func (c CamerasConfig) ForceH264Enabled() bool {
	return c.ForceH264 == nil || *c.ForceH264
}

// matches reports whether the camera identified by name/id/mac is in the list.
func matches(list []string, name, id, mac string) bool {
	for _, e := range list {
		e = strings.TrimSpace(e)
		if strings.EqualFold(e, name) || strings.EqualFold(e, id) || strings.EqualFold(e, mac) {
			return true
		}
	}
	return false
}

// CameraSelected applies the include/exclude filters.
func (c CamerasConfig) CameraSelected(name, id, mac string) bool {
	if len(c.Include) > 0 && !matches(c.Include, name, id, mac) {
		return false
	}
	return !matches(c.Exclude, name, id, mac)
}

// FFmpegConfig controls the ffmpeg processes used to relay RTSPS streams from
// Protect to HomeKit (SRTP). Video is remuxed without transcoding, so CPU
// usage stays minimal even on a Raspberry Pi.
type FFmpegConfig struct {
	// Path of the ffmpeg binary, defaults to "ffmpeg" (from $PATH).
	Path string `json:"path,omitempty"`
	// Audio transcodes the camera audio (AAC) to Opus for HomeKit. Requires
	// ffmpeg with libopus. Enabled by default; disable for video-only streams.
	Audio *bool `json:"audio,omitempty"`
	// Debug logs the full ffmpeg command lines and process output.
	Debug bool `json:"debug,omitempty"`
}

func (c FFmpegConfig) AudioEnabled() bool {
	return c.Audio == nil || *c.Audio
}

// PprofConfig enables the Go pprof profiling endpoint. Disabled by default;
// only enable on trusted networks — pprof exposes runtime internals.
type PprofConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Port defaults to 6060.
	Port int `json:"port,omitempty"`
	// Bind restricts the listen address (e.g. "127.0.0.1"). Empty = all
	// interfaces.
	Bind string `json:"bind,omitempty"`
}

var envVariableRegex = regexp.MustCompile(`\${([^}]+)}`)

// replaceEnvVariables substitutes "${NAME}" placeholders with environment
// variable values (same syntax as the other mqtt-home/philipparndt gateways).
func replaceEnvVariables(input []byte) []byte {
	return envVariableRegex.ReplaceAllFunc(input, func(match []byte) []byte {
		name := string(match[2 : len(match)-1])
		return []byte(os.Getenv(name))
	})
}

func LoadConfig(file string) (Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		logger.Error("Error reading config file", "error", err)
		return Config{}, err
	}

	data = replaceEnvVariables(data)

	// YAML configs are converted to JSON so the same structs work for both
	// formats.
	if ext := strings.ToLower(filepath.Ext(file)); ext == ".yaml" || ext == ".yml" {
		data, err = yaml.YAMLToJSON(data)
		if err != nil {
			logger.Error("Converting YAML config", "error", err)
			return Config{}, err
		}
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		logger.Error("Unmarshaling JSON", "error", err)
		return Config{}, err
	}

	if cfg.Protect.Host == "" {
		return Config{}, fmt.Errorf("protect.host is required")
	}
	if !strings.HasPrefix(cfg.Protect.Host, "http") {
		cfg.Protect.Host = "https://" + cfg.Protect.Host
	}
	cfg.Protect.Host = strings.TrimSuffix(cfg.Protect.Host, "/")
	if cfg.Protect.Username == "" || cfg.Protect.Password == "" {
		return Config{}, fmt.Errorf("protect.username and protect.password are required")
	}

	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.HomeKit.BridgeName == "" {
		cfg.HomeKit.BridgeName = "Protect HomeKit"
	}
	if cfg.HomeKit.Pin == "" {
		cfg.HomeKit.Pin = "031-45-154"
	}
	if cfg.HomeKit.SetupID == "" {
		// A stable 4-char setup id is needed for the pairing QR code. Changing it
		// later only affects the QR/discovery, not existing pairings.
		cfg.HomeKit.SetupID = "PRTC"
	}
	if cfg.FFmpeg.Path == "" {
		cfg.FFmpeg.Path = "ffmpeg"
	}
	if cfg.Web.Port == 0 {
		cfg.Web.Port = 8080
	}
	if cfg.Pprof.Port == 0 {
		cfg.Pprof.Port = 6060
	}

	return cfg, nil
}
