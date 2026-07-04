package bridge

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/philipparndt/go-logger"
)

// OpenLiveStream starts an ffmpeg process that remuxes the camera's RTSPS
// stream (H.264 + AAC, both copied) into fragmented MP4 suitable for the
// browser MediaSource API. The returned reader delivers the fMP4 byte
// stream; closing it (or canceling ctx) stops ffmpeg.
//
// quality selects the Protect channel by name ("high", "medium", "low");
// empty defaults to medium — a sane bandwidth for a dashboard tile.
func (b *Bridge) OpenLiveStream(ctx context.Context, cameraID, quality string) (io.ReadCloser, error) {
	url, camName, err := b.liveURL(cameraID, quality)
	if err != nil {
		return nil, err
	}

	args := []string{
		"-hide_banner", "-nostats",
		"-loglevel", "error",
		"-fflags", "+discardcorrupt",
		"-err_detect", "ignore_err",
		"-probesize", "16384",
		"-rtsp_transport", "tcp",
		"-i", url,
		"-map", "0:v:0",
		"-c:v", "copy",
		"-map", "0:a:0?",
		"-c:a", "copy",
		"-f", "mp4",
		// empty_moov: emit the init segment immediately; default_base_moof +
		// per-keyframe/500ms fragments are what MediaSource expects.
		"-movflags", "empty_moov+default_base_moof+frag_keyframe",
		"-frag_duration", "500000",
		"pipe:1",
	}

	cmd := exec.CommandContext(ctx, b.cfg.FFmpeg.Path, args...)
	if b.cfg.FFmpeg.Debug {
		logger.Info("Starting live remux", "camera", camName, "cmd", b.cfg.FFmpeg.Path+" "+strings.Join(args, " "))
		cmd.Stderr = os.Stderr
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}
	logger.Info("Web live stream started", "camera", camName, "quality", qualityOrDefault(quality))

	go func() {
		_ = cmd.Wait()
		logger.Debug("Web live stream ffmpeg exited", "camera", camName)
	}()

	return &liveStream{ReadCloser: stdout, cmd: cmd}, nil
}

// liveStream terminates ffmpeg when the consumer is done reading.
type liveStream struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (l *liveStream) Close() error {
	if l.cmd.Process != nil {
		_ = l.cmd.Process.Signal(syscall.SIGINT)
	}
	return l.ReadCloser.Close()
}

func qualityOrDefault(q string) string {
	if q == "" {
		return "medium"
	}
	return strings.ToLower(q)
}

// liveURL resolves the RTSPS URL of the requested quality channel, falling
// back to the best available one.
func (b *Bridge) liveURL(cameraID, quality string) (string, string, error) {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()

	cam, ok := b.camState[cameraID]
	if !ok {
		return "", "", fmt.Errorf("unknown camera %s", cameraID)
	}

	want := qualityOrDefault(quality)
	var fallback *channelPick
	for _, ch := range cam.Channels {
		if !ch.Enabled || !ch.IsRtspEnabled || ch.RtspAlias == "" {
			continue
		}
		if strings.EqualFold(ch.Name, want) {
			return b.client.RTSPSURL(b.bootstrap, ch.RtspAlias), cam.Name, nil
		}
		if fallback == nil || ch.Width < fallback.Width {
			fallback = &channelPick{Alias: ch.RtspAlias, Width: ch.Width}
		}
	}
	if fallback == nil {
		return "", "", fmt.Errorf("no RTSP-enabled channel on %s", cam.Name)
	}
	return b.client.RTSPSURL(b.bootstrap, fallback.Alias), cam.Name, nil
}

type channelPick struct {
	Alias string
	Width int
}
