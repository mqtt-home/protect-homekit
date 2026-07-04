# protect-homekit

A lightweight Go service that bridges **UniFi Protect** cameras directly to
**Apple HomeKit** — no Homebridge, no Node.js.

Built as a resource-friendly alternative to
[homebridge-unifi-protect](https://github.com/hjdhjd/homebridge-unifi-protect)
for small hosts like a Raspberry Pi running Kubernetes.

## Features

- **Live video** in the Home app: the camera's H.264 stream is remuxed
  (`-c:v copy`), never transcoded — CPU usage stays near zero. Only audio is
  transcoded (AAC → Opus) and can be disabled.
- **Snapshots** for camera tiles, served from the NVR snapshot API with a
  short cache.
- **Motion sensors** fed by the Protect realtime websocket.
- **Doorbells** (G4 Doorbell etc.) appear as HomeKit video doorbells and ring
  in the Home app.
- Automatic RTSPS stream activation in Protect (optional).
- **Web UI** with live camera overview (snapshots, motion, ring events via
  SSE) and the HomeKit pairing QR code.
- Stable accessory IDs derived from the Protect camera id — adding/removing
  cameras never breaks existing HomeKit rooms or automations.
- Single static binary + ffmpeg; multi-arch Docker image (amd64/arm64).

## How it works

```
┌─────────────┐  bootstrap / snapshots / websocket   ┌──────────────────┐
│ UniFi OS    │ ◄──────────────────────────────────► │ protect-homekit  │
│ (Protect)   │                                      │                  │
│             │  rtsps://nvr:7441/<alias>            │  hap (HomeKit)   │
│  cameras ───┼──────────────► ffmpeg ──────────────►│  SRTP → iPhone/  │
└─────────────┘                (video copy)          │  AppleTV hub     │
                                                     └──────────────────┘
```

- Camera list, channels and events come from the Protect API on the console
  (`/proxy/protect/api/...`), authenticated with a local user account.
- For each HomeKit stream request, ffmpeg pulls the best matching RTSPS
  channel (High/Medium/Low) and forwards it as SRTP to the controller.
- Motion and ring events arrive over the Protect updates websocket
  (binary frames, zlib-compressed JSON).

## Requirements

- UniFi OS console with Protect (UDM, UDM Pro, UNVR, CloudKey Gen2+, ...)
- A **local** Protect user (not a Ubiquiti cloud account, no 2FA).
  Admin rights are only needed for `cameras.auto_enable_rtsp`.
- `ffmpeg` with `libopus` (any distro build; included in the Docker image).
- A HomeKit hub (Apple TV / HomePod) for remote access, as with any camera.

## Configuration

See [production/config/config.example.yaml](production/config/config.example.yaml)
for all options with documentation. Minimal config:

```yaml
protect:
  host: https://192.168.1.1
  username: ${PROTECT_USERNAME}
  password: ${PROTECT_PASSWORD}

homekit:
  bridge_name: Protect HomeKit
  pin: "031-45-154"
```

`${NAME}` placeholders are replaced with environment variables.

> **Important:** `homekit.storage_dir` (default: `<config-dir>/hap`) holds the
> pairing keys and must survive restarts — mount it on a persistent volume,
> or HomeKit will drop the pairing on every pod restart.

## Running

```sh
cd app
make build
./build/protect-homekit ../production/config/config.yaml
```

On startup the log prints the setup code and a scannable QR code. Add the
bridge in the Home app via *Add Accessory*; all cameras appear as bridged
accessories.

### Docker

```sh
docker run -d \
  -v /path/to/config:/var/lib/protect-homekit \
  --network host \
  -e PROTECT_USERNAME=homekit \
  -e PROTECT_PASSWORD=... \
  pharndt/protect-homekit:latest
```

Host networking is required: HomeKit discovery uses mDNS and the HAP/SRTP
ports must be reachable from the controllers.

### Kubernetes

Run with `hostNetwork: true`, pin `homekit.port` so it can be allowed through
firewalls, and mount a persistent volume for the config directory (pairing
state). Resource-wise the bridge idles at a few MB of RAM; ffmpeg processes
only run while a stream is being watched and only remux (no transcoding).

## Cameras and channels

Protect cameras expose up to three fixed streams (High/Medium/Low). The
bridge advertises the standard HomeKit resolutions and picks the smallest
channel that satisfies the controller's request, so Apple Watch and cellular
viewers get the low stream while a home hub gets the high one.

RTSPS must be enabled per channel; by default the bridge enables it
automatically (`cameras.auto_enable_rtsp`), matching what
homebridge-unifi-protect does.

### H.265 / "enhanced encoding"

Newer cameras/firmware default to H.265 ("enhanced encoding"). HomeKit can
only stream H.264, and this bridge deliberately never transcodes video, so by
default it switches such cameras to Standard/H.264 encoding via the Protect
API (`cameras.force_h264`). This also affects Protect recordings — H.264
needs roughly twice the storage. Set `force_h264: false` to keep H.265; those
cameras then can't stream to HomeKit (snapshots and motion still work).

## Limitations

- Two-way audio (doorbell talkback) is not implemented.
- HomeKit Secure Video is not implemented.
- New cameras are picked up at startup — restart the service after adopting
  a camera in Protect.
