package bridge

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brutella/hap/rtp"
	"github.com/philipparndt/go-logger"
)

// streamInput resolves the RTSPS input URL for a requested video width. It is
// re-evaluated on every stream start so channel/bootstrap changes are picked
// up.
type streamInput func(width int) (url string, err error)

// streamer relays a camera's RTSPS stream to HomeKit controllers via ffmpeg.
// The H.264 video is copied without transcoding (Protect cameras already
// produce HomeKit-compatible H.264), only audio is transcoded to Opus — this
// keeps CPU usage near zero, even on a Raspberry Pi.
type streamer struct {
	cameraName string
	ffmpegPath string
	audio      bool
	debug      bool
	input      streamInput

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	req  rtp.SetupEndpoints
	resp rtp.SetupEndpointsResponse
	cmd  *exec.Cmd
}

func newStreamer(cameraName, ffmpegPath string, audio, debug bool, input streamInput) *streamer {
	return &streamer{
		cameraName: cameraName,
		ffmpegPath: ffmpegPath,
		audio:      audio,
		debug:      debug,
		input:      input,
		sessions:   map[string]*session{},
	}
}

// prepare registers a pending session negotiated via SetupEndpoints.
func (s *streamer) prepare(req rtp.SetupEndpoints, resp rtp.SetupEndpointsResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[string(req.SessionId)] = &session{req: req, resp: resp}
}

func (s *streamer) start(id []byte, video rtp.VideoParameters, audio rtp.AudioParameters) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[string(id)]
	if !ok {
		return fmt.Errorf("stream session not found")
	}
	if sess.cmd != nil {
		return nil // already running
	}

	inputURL, err := s.input(int(video.Attributes.Width))
	if err != nil {
		return err
	}

	args := s.ffmpegArgs(sess, inputURL, video, audio)
	cmd := exec.Command(s.ffmpegPath, args...)
	// Keep the tail of stderr so an ffmpeg failure is diagnosable from the
	// normal log, not only with ffmpeg.debug enabled.
	tail := &tailBuffer{}
	if s.debug {
		logger.Info("Starting ffmpeg", "camera", s.cameraName, "cmd", s.ffmpegPath+" "+strings.Join(args, " "))
		cmd.Stdout = os.Stdout
		cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	} else {
		cmd.Stderr = tail
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	sess.cmd = cmd
	logger.Info("Stream started", "camera", s.cameraName,
		"controller", sess.req.ControllerAddr.IPAddr,
		"requested", fmt.Sprintf("%dx%d", video.Attributes.Width, video.Attributes.Height))

	// Reap the process when it exits on its own (e.g. the controller vanished
	// and the srtp timeout fired) so the session doesn't leak.
	started := time.Now()
	go func(key string) {
		err := cmd.Wait()
		s.mu.Lock()
		if cur, ok := s.sessions[key]; ok && cur.cmd == cmd {
			cur.cmd = nil
			delete(s.sessions, key)
		}
		s.mu.Unlock()
		// An early exit means the stream never worked — surface why.
		if err != nil || time.Since(started) < 5*time.Second {
			logger.Warn("ffmpeg exited unexpectedly", "camera", s.cameraName,
				"after", time.Since(started).Round(time.Millisecond),
				"error", err, "stderr", tail.String())
		} else {
			logger.Debug("ffmpeg exited", "camera", s.cameraName, "error", err)
		}
	}(string(id))

	return nil
}

// ffmpegArgs builds the relay command: RTSPS in (TCP), H.264 copy + optional
// Opus audio out over SRTP to the controller.
func (s *streamer) ffmpegArgs(sess *session, inputURL string, video rtp.VideoParameters, audio rtp.AudioParameters) []string {
	loglevel := "error"
	if s.debug {
		loglevel = "info"
	}

	addr := sess.req.ControllerAddr
	mtu := "1378"
	if addr.IPVersion == rtp.IPAddrVersionv6 {
		mtu = "1228"
	}

	// Input flags follow homebridge-unifi-protect: tolerate Protect's RTSPS
	// quirks, keep latency low and start fast (the SDP carries the codec
	// parameters, so a tiny probesize is enough).
	args := []string{
		"-hide_banner", "-nostats",
		"-loglevel", loglevel,
		"-fflags", "+discardcorrupt",
		"-err_detect", "ignore_err",
		"-max_delay", "500000",
		"-flags", "low_delay",
		"-probesize", "16384",
		"-avioflags", "direct",
		"-rtsp_transport", "tcp",
		"-i", inputURL,
		"-map", "0:v:0",
		"-c:v", "copy",
		"-payload_type", strconv.Itoa(int(video.RTP.PayloadType)),
		"-ssrc", strconv.Itoa(int(sess.resp.SsrcVideo)),
		"-f", "rtp",
		// Without flushing after every packet, ffmpeg's IO layer batches the
		// RTP packets and the controller only renders a frame per burst —
		// the stream looks frozen.
		"-flush_packets", "1",
		"-srtp_out_suite", "AES_CM_128_HMAC_SHA1_80",
		"-srtp_out_params", sess.req.Video.SrtpKey(),
		fmt.Sprintf("srtp://%s:%d?rtcpport=%d&pkt_size=%s",
			addr.IPAddr, addr.VideoRtpPort, addr.VideoRtpPort, mtu),
	}

	// Only Opus is advertised in the supported audio configuration, so a
	// controller requesting audio always selects Opus.
	if s.audio && audio.CodecType == rtp.AudioCodecType_Opus {
		samplerate := "16000"
		if audio.CodecParams.Samplerate == rtp.AudioCodecSampleRate24Khz {
			samplerate = "24000"
		} else if audio.CodecParams.Samplerate == rtp.AudioCodecSampleRate8Khz {
			samplerate = "8000"
		}

		args = append(args,
			"-map", "0:a:0?",
			"-c:a", "libopus",
			"-application", "lowdelay",
			"-frame_duration", "20",
			"-ac", "1",
			"-ar", samplerate,
			"-b:a", "24k",
			"-payload_type", strconv.Itoa(int(audio.RTP.PayloadType)),
			"-ssrc", strconv.Itoa(int(sess.resp.SsrcAudio)),
			"-f", "rtp",
			"-flush_packets", "1",
			"-srtp_out_suite", "AES_CM_128_HMAC_SHA1_80",
			"-srtp_out_params", sess.req.Audio.SrtpKey(),
			fmt.Sprintf("srtp://%s:%d?rtcpport=%d&pkt_size=188",
				addr.IPAddr, addr.AudioRtpPort, addr.AudioRtpPort),
		)
	}

	return args
}

func (s *streamer) stop(id []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := string(id)
	sess, ok := s.sessions[key]
	if !ok {
		return
	}
	if sess.cmd != nil && sess.cmd.Process != nil {
		proc := sess.cmd.Process
		_ = proc.Signal(syscall.SIGINT)
		// The Wait reaper goroutine cleans up; force-kill stragglers.
		go func() {
			<-time.After(3 * time.Second)
			_ = proc.Kill()
		}()
	}
	delete(s.sessions, key)
	logger.Info("Stream stopped", "camera", s.cameraName)
}

func (s *streamer) suspend(id []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[string(id)]; ok && sess.cmd != nil && sess.cmd.Process != nil {
		_ = sess.cmd.Process.Signal(syscall.SIGSTOP)
	}
}

func (s *streamer) resume(id []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[string(id)]; ok && sess.cmd != nil && sess.cmd.Process != nil {
		_ = sess.cmd.Process.Signal(syscall.SIGCONT)
	}
}

// stopAll terminates every active stream (shutdown).
func (s *streamer) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, sess := range s.sessions {
		if sess.cmd != nil && sess.cmd.Process != nil {
			_ = sess.cmd.Process.Signal(syscall.SIGINT)
		}
		delete(s.sessions, key)
	}
}

// tailBuffer keeps the last part of what was written to it (bounded), so
// ffmpeg's stderr can be attached to log messages without unbounded growth.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailBufferSize = 2048

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBufferSize {
		t.buf = t.buf[len(t.buf)-tailBufferSize:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// ipAtInterface returns the ip of iface for the requested version
// (rtp.IPAddrVersionv4 / v6). Borrowed from brutella/hkcam.
func ipAtInterface(iface net.Interface, version uint8) (net.IP, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}

	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}

		switch version {
		case rtp.IPAddrVersionv4:
			if ip.To4() != nil {
				return ip, nil
			}
		case rtp.IPAddrVersionv6:
			if ip.To16() != nil {
				return ip, nil
			}
		}
	}

	return nil, fmt.Errorf("%s: no ip address found for version %d", iface.Name, version)
}

// ifaceOfRequest returns the network interface on which the HAP request was
// received, so the stream response advertises an address the controller can
// actually reach. Borrowed from brutella/hkcam.
func ifaceOfRequest(r *http.Request) (*net.Interface, error) {
	v := r.Context().Value(http.LocalAddrContextKey)
	if v == nil {
		return nil, fmt.Errorf("no local address in request context")
	}

	host, _, err := net.SplitHostPort(v.(net.Addr).String())
	if err != nil {
		return nil, err
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// v6 addresses may carry the zone: "fe80::1%eth0"
		comps := strings.Split(host, "%")
		if len(comps) == 2 {
			return net.InterfaceByName(comps[1])
		}
		return nil, fmt.Errorf("unable to parse ip %s", host)
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			addrIP, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			if reflect.DeepEqual(addrIP, ip) {
				return &iface, nil
			}
		}
	}

	return nil, fmt.Errorf("could not find interface for connection")
}
