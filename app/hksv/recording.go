package hksv

// Recording pipeline. While a camera's HKSV recording is active, a persistent
// ffmpeg process remuxes the camera's RTSPS stream into fragmented MP4. The
// most recent fragments are retained in a ring (the prebuffer) so a recording
// can include footage from just before the triggering event. When the
// controller opens a recording data stream, StartRecording replays the init
// segment and the buffered prebuffer, then forwards live fragments.
//
// Video is copied (never transcoded), matching the rest of the bridge; only
// audio, when enabled, is transcoded to AAC-LC (which stock ffmpeg supports,
// unlike the Opus path used for live streaming).

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/philipparndt/go-logger"
)

// subscriberBuffer bounds how many fragments may queue for one recording
// consumer before it is considered stalled and dropped.
const subscriberBuffer = 256

// prebufferGuard is how many extra fragments beyond the prebuffer window are
// retained, to be safe around fragment-length rounding.
const prebufferGuard = 2

// URLResolver returns the RTSPS URL for the channel best matching a requested
// video width.
type URLResolver func(width int) (string, error)

// recorder implements Recorder using a persistent ffmpeg prebuffer.
type recorder struct {
	cameraName string
	ffmpegPath string
	debug      bool
	resolve    URLResolver

	mu      sync.Mutex
	enabled bool
	audio   bool
	cfg     SelectedConfig
	cancel  context.CancelFunc
	initSeg []byte
	ring    [][]byte
	ringMax int
	subs    map[chan Fragment]struct{}
}

// newRecorder creates a recorder for one camera.
func newRecorder(cameraName, ffmpegPath string, debug bool, resolve URLResolver) *recorder {
	return &recorder{
		cameraName: cameraName,
		ffmpegPath: ffmpegPath,
		debug:      debug,
		resolve:    resolve,
		subs:       map[chan Fragment]struct{}{},
	}
}

// enable starts (or reconfigures) the prebuffer for the given selected
// configuration. Safe to call repeatedly; a config change restarts ffmpeg.
func (r *recorder) enable(cfg SelectedConfig, audio bool) {
	r.mu.Lock()
	if r.enabled && r.cfg == cfg && r.audio == audio {
		r.mu.Unlock()
		return
	}
	// Restart with the new configuration.
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.enabled = true
	r.cfg = cfg
	r.audio = audio
	r.initSeg = nil
	r.ring = nil

	frag := cfg.FragmentLengthMS
	if frag <= 0 {
		frag = 4000
	}
	prebuffer := max(cfg.PrebufferMS, frag)
	r.ringMax = prebuffer/frag + prebufferGuard

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	go r.prebufferLoop(ctx, cfg, audio)
	logger.Info("HKSV prebuffer enabled", "camera", r.cameraName,
		"resolution", fmt.Sprintf("%dx%d", cfg.Video.Width, cfg.Video.Height), "audio", audio)
}

// disable stops the prebuffer and drops any subscribers.
func (r *recorder) disable() {
	r.mu.Lock()
	if !r.enabled {
		r.mu.Unlock()
		return
	}
	r.enabled = false
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	subs := r.subs
	r.subs = map[chan Fragment]struct{}{}
	r.initSeg = nil
	r.ring = nil
	r.mu.Unlock()

	for ch := range subs {
		close(ch)
	}
	logger.Info("HKSV prebuffer disabled", "camera", r.cameraName)
}

// prebufferLoop runs ffmpeg and restarts it on failure until ctx is cancelled.
func (r *recorder) prebufferLoop(ctx context.Context, cfg SelectedConfig, audio bool) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		if err := r.runFFmpeg(ctx, cfg, audio); err != nil && ctx.Err() == nil {
			logger.Warn("HKSV prebuffer ffmpeg exited", "camera", r.cameraName, "error", err,
				"after", time.Since(start).Round(time.Millisecond))
		}
		if ctx.Err() != nil {
			return
		}
		// Reset backoff if ffmpeg ran for a meaningful time.
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// runFFmpeg launches one ffmpeg process and pumps its fMP4 output into the
// prebuffer/subscribers until it exits or ctx is cancelled.
func (r *recorder) runFFmpeg(ctx context.Context, cfg SelectedConfig, audio bool) error {
	url, err := r.resolve(cfg.Video.Width)
	if err != nil {
		return fmt.Errorf("resolve stream url: %w", err)
	}

	args := r.ffmpegArgs(cfg, audio, url)
	cmd := exec.CommandContext(ctx, r.ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	tail := &tailWriter{}
	if r.debug {
		logger.Info("HKSV starting ffmpeg", "camera", r.cameraName, "cmd", r.ffmpegPath+" "+strings.Join(args, " "))
		cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	} else {
		cmd.Stderr = tail
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	splitErr := splitFragments(stdout,
		func(initSeg []byte) error { r.setInit(initSeg); return nil },
		func(frag []byte) error { r.pushFragment(frag); return nil },
	)
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return nil
	}
	if splitErr != nil && splitErr != io.EOF {
		return fmt.Errorf("%w (ffmpeg: %s)", splitErr, tail.String())
	}
	if waitErr != nil {
		return fmt.Errorf("ffmpeg: %v (%s)", waitErr, tail.String())
	}
	return io.ErrUnexpectedEOF // stream ended without error -> restart
}

// ffmpegArgs builds the prebuffer command: RTSPS in, H.264 copy + optional AAC
// audio, fragmented MP4 out to stdout.
func (r *recorder) ffmpegArgs(cfg SelectedConfig, audio bool, url string) []string {
	loglevel := "error"
	if r.debug {
		loglevel = "info"
	}
	fragMS := cfg.FragmentLengthMS
	if fragMS <= 0 {
		fragMS = 4000
	}

	args := []string{
		"-hide_banner", "-nostats",
		"-loglevel", loglevel,
		"-fflags", "+discardcorrupt+genpts",
		"-err_detect", "ignore_err",
		"-probesize", "1048576",
		"-analyzeduration", "2000000",
		"-rtsp_transport", "tcp",
		"-i", url,
		"-map", "0:v:0",
		"-c:v", "copy",
	}

	if audio {
		samplerate := sampleRateHz(cfg.Audio.SampleRate)
		bitrate := cfg.Audio.MaxBitrate
		if bitrate <= 0 {
			bitrate = 64
		}
		channels := cfg.Audio.Channels
		if channels <= 0 {
			channels = 1
		}
		args = append(args,
			"-map", "0:a:0?",
			"-c:a", "aac",
			"-profile:a", "aac_low",
			"-ac", strconv.Itoa(channels),
			"-ar", strconv.Itoa(samplerate),
			"-b:a", strconv.Itoa(bitrate)+"k",
		)
	}

	args = append(args,
		"-f", "mp4",
		// empty_moov emits the init segment immediately; frag_keyframe starts
		// each fragment on a keyframe so fragments are independently decodable.
		"-movflags", "+empty_moov+default_base_moof+frag_keyframe+omit_tfhd_offset",
		"-frag_duration", strconv.Itoa(fragMS*1000),
		"pipe:1",
	)
	return args
}

// setInit records the initialization segment for new subscribers.
func (r *recorder) setInit(initSeg []byte) {
	r.mu.Lock()
	r.initSeg = append([]byte(nil), initSeg...)
	// A fresh init segment means a new ffmpeg session; the old ring is stale.
	r.ring = nil
	r.mu.Unlock()
}

// pushFragment appends a fragment to the prebuffer ring and fans it out to
// active subscribers, dropping any that have stalled.
func (r *recorder) pushFragment(frag []byte) {
	f := append([]byte(nil), frag...)
	r.mu.Lock()
	r.ring = append(r.ring, f)
	if len(r.ring) > r.ringMax {
		r.ring = r.ring[len(r.ring)-r.ringMax:]
	}
	var stalled []chan Fragment
	for ch := range r.subs {
		select {
		case ch <- Fragment{Data: f}:
		default:
			stalled = append(stalled, ch)
		}
	}
	for _, ch := range stalled {
		delete(r.subs, ch)
	}
	r.mu.Unlock()

	for _, ch := range stalled {
		logger.Warn("HKSV recording consumer stalled; dropping", "camera", r.cameraName)
		close(ch)
	}
}

// StartRecording registers a subscriber that first receives the init segment
// and buffered prebuffer, then live fragments until ctx is cancelled.
func (r *recorder) StartRecording(ctx context.Context) (<-chan Fragment, error) {
	r.mu.Lock()
	if !r.enabled {
		r.mu.Unlock()
		return nil, fmt.Errorf("recording not enabled")
	}
	if r.initSeg == nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("prebuffer not ready (no init segment yet)")
	}

	ch := make(chan Fragment, subscriberBuffer)
	ch <- Fragment{Data: append([]byte(nil), r.initSeg...)}
	for _, f := range r.ring {
		select {
		case ch <- Fragment{Data: f}:
		default:
			// Prebuffer larger than the buffer: keep the most recent fragments.
		}
	}
	r.subs[ch] = struct{}{}
	r.mu.Unlock()

	go func() {
		<-ctx.Done()
		r.removeSub(ch)
	}()
	return ch, nil
}

// removeSub detaches and closes a subscriber if still present.
func (r *recorder) removeSub(ch chan Fragment) {
	r.mu.Lock()
	if _, ok := r.subs[ch]; ok {
		delete(r.subs, ch)
		r.mu.Unlock()
		close(ch)
		return
	}
	r.mu.Unlock()
}

// sampleRateHz maps the HKSV sample-rate enum to Hz.
func sampleRateHz(rate byte) int {
	switch rate {
	case SampleRate8kHz:
		return 8000
	case SampleRate16kHz:
		return 16000
	case SampleRate24kHz:
		return 24000
	case SampleRate32kHz:
		return 32000
	case SampleRate441kHz:
		return 44100
	case SampleRate48kHz:
		return 48000
	default:
		return 32000
	}
}

// tailWriter keeps the last part of what is written to it, so ffmpeg's stderr
// can be attached to log lines without unbounded growth.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

const tailWriterSize = 2048

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailWriterSize {
		t.buf = t.buf[len(t.buf)-tailWriterSize:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
