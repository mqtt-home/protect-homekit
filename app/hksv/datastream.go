package hksv

// Data-stream protocol: the control "hello" handshake and the "dataSend"
// recording flow that pushes fMP4 fragments to the controller over an HDS
// connection. One Server owns the dedicated TCP listener created for a single
// SetupDataStreamTransport request; once the controller connects it runs the
// protocol until the recording ends or the connection closes.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/philipparndt/go-logger"
)

// maxChunkSize bounds a single dataSend "data" packet; larger fragments are
// split across sequential chunks.
const maxChunkSize = 0x40000 // 262144

// helloTimeout bounds how long the accessory waits for the controller's hello.
const helloTimeout = 10 * time.Second

// HDSProtocolSpecificErrorReason values (dataSend close reasons / open errors).
const (
	reasonNotAllowed        = 1
	reasonBusy              = 2
	reasonUnexpectedFailure = 5
	reasonInvalidConfig     = 9
)

// Fragment is one piece of the fMP4 recording stream. The first fragment of a
// session is the initialization segment (ftyp+moov); the rest are media
// fragments (moof+mdat). IsLast marks the accessory-side end of the recording;
// Err signals an abnormal end.
type Fragment struct {
	Data   []byte
	IsLast bool
	Err    error
}

// Recorder produces the fMP4 fragment stream for one recording session. The
// returned channel yields the init segment first, then media fragments, and is
// closed when recording ends. Cancelling ctx must stop production and release
// any ffmpeg process.
type Recorder interface {
	StartRecording(ctx context.Context) (<-chan Fragment, error)
}

// Server hosts the TCP listener for one prepared data-stream transport and runs
// the HDS recording protocol once the controller connects.
type Server struct {
	keys       hdsKeys
	recorder   Recorder
	cameraName string

	// canRecord reports whether recording is currently permitted (recording
	// Active and HomeKitCameraActive). hasConfig reports whether the controller
	// has written a SelectedCameraRecordingConfiguration.
	canRecord func() bool
	hasConfig func() bool

	ln   net.Listener
	port int
}

// NewServer binds a dedicated ephemeral TCP listener and returns a Server ready
// to accept the controller's connection. Port returns the bound port to hand
// back in the SetupDataStreamTransport response.
func NewServer(keys hdsKeys, recorder Recorder, cameraName string, canRecord, hasConfig func() bool) (*Server, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, fmt.Errorf("hds: listen: %w", err)
	}
	return &Server{
		keys:       keys,
		recorder:   recorder,
		cameraName: cameraName,
		canRecord:  canRecord,
		hasConfig:  hasConfig,
		ln:         ln,
		port:       ln.Addr().(*net.TCPAddr).Port,
	}, nil
}

// Port is the TCP port the controller should connect to.
func (s *Server) Port() int { return s.port }

// Serve accepts one controller connection and runs the recording protocol,
// blocking until it ends. It always closes the listener before returning.
func (s *Server) Serve(ctx context.Context) {
	defer s.ln.Close()

	// Stop the accept if the controller never connects.
	if tl, ok := s.ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(helloTimeout))
	}
	// Abort the blocking Accept when the parent context is cancelled.
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()

	conn, err := s.ln.Accept()
	if err != nil {
		if ctx.Err() == nil {
			logger.Debug("HKSV data stream: no controller connection", "camera", s.cameraName, "error", err)
		}
		return
	}
	defer conn.Close()

	hc, err := newHDSConn(conn, s.keys)
	if err != nil {
		logger.Error("HKSV data stream: cipher init", "camera", s.cameraName, "error", err)
		return
	}

	sess := &dataStreamSession{
		conn:       hc,
		recorder:   s.recorder,
		cameraName: s.cameraName,
		canRecord:  s.canRecord,
		hasConfig:  s.hasConfig,
	}
	sess.run(ctx, conn)
}

// dataStreamSession carries the per-connection protocol state.
type dataStreamSession struct {
	conn       *hdsConn
	recorder   Recorder
	cameraName string
	canRecord  func() bool
	hasConfig  func() bool

	writeMu sync.Mutex

	mu        sync.Mutex
	streamID  int64
	recording bool
	cancelRec context.CancelFunc
}

// send serializes an outgoing message onto the connection.
func (s *dataStreamSession) send(header Dict, message any) error {
	payload, err := encodeMessage(header, message)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.writeFrame(payload)
}

// run reads and dispatches frames until the connection ends. The controller
// must send a control/hello first; other traffic before then is rejected.
func (s *dataStreamSession) run(ctx context.Context, raw net.Conn) {
	defer s.stopRecording()

	// Bound the wait for the initial hello.
	_ = raw.SetReadDeadline(time.Now().Add(helloTimeout))

	greeted := false
	for {
		payload, err := s.conn.readFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				logger.Debug("HKSV data stream closed", "camera", s.cameraName, "error", err)
			}
			return
		}
		msg, err := decodeMessage(payload)
		if err != nil {
			logger.Debug("HKSV data stream: bad message", "camera", s.cameraName, "error", err)
			continue
		}

		if !greeted {
			if msg.protocol != "control" || msg.topic != "hello" || msg.kind != msgRequest {
				logger.Debug("HKSV data stream: expected hello", "camera", s.cameraName, "protocol", msg.protocol, "topic", msg.topic)
				return
			}
			greeted = true
			_ = raw.SetReadDeadline(time.Time{}) // clear the hello deadline
			s.handleHello(msg)
			continue
		}

		s.handle(ctx, msg)
	}
}

func (s *dataStreamSession) handle(ctx context.Context, msg *hdsMessage) {
	switch {
	case msg.protocol == "control" && msg.topic == "hello" && msg.kind == msgRequest:
		s.handleHello(msg)
	case msg.protocol == "dataSend" && msg.topic == "open" && msg.kind == msgRequest:
		s.handleOpen(ctx, msg)
	case msg.protocol == "dataSend" && msg.topic == "ack" && msg.kind == msgEvent:
		logger.Debug("HKSV recording acknowledged", "camera", s.cameraName)
	case msg.protocol == "dataSend" && msg.topic == "close" && msg.kind == msgEvent:
		logger.Debug("HKSV recording closed by controller", "camera", s.cameraName)
		s.stopRecording()
	default:
		logger.Debug("HKSV data stream: unhandled message", "camera", s.cameraName, "protocol", msg.protocol, "topic", msg.topic, "kind", msg.kind)
	}
}

func (s *dataStreamSession) handleHello(msg *hdsMessage) {
	header := Dict{
		{"protocol", "control"},
		{"response", "hello"},
		{"id", Int64(msg.id)},
		{"status", Int64(hdsStatusSuccess)},
	}
	if err := s.send(header, Dict{}); err != nil {
		logger.Debug("HKSV data stream: hello response failed", "camera", s.cameraName, "error", err)
	}
}

// handleOpen validates and answers a dataSend open request, then starts pushing
// fragments on success.
func (s *dataStreamSession) handleOpen(ctx context.Context, msg *hdsMessage) {
	target, _ := msg.message["target"].(string)
	typ, _ := msg.message["type"].(string)
	streamID, _ := asInt64(msg.message["streamId"])

	if target != "controller" || typ != "ipcamera.recording" {
		s.rejectOpen(msg.id, reasonUnexpectedFailure)
		return
	}
	if !s.canRecord() {
		s.rejectOpen(msg.id, reasonNotAllowed)
		return
	}
	if !s.hasConfig() {
		s.rejectOpen(msg.id, reasonInvalidConfig)
		return
	}

	s.mu.Lock()
	if s.recording {
		s.mu.Unlock()
		s.rejectOpen(msg.id, reasonBusy)
		return
	}
	recCtx, cancel := context.WithCancel(ctx)
	s.recording = true
	s.streamID = streamID
	s.cancelRec = cancel
	s.mu.Unlock()

	// Success response.
	header := Dict{
		{"protocol", "dataSend"},
		{"response", "open"},
		{"id", Int64(msg.id)},
		{"status", Int64(hdsStatusSuccess)},
	}
	if err := s.send(header, Dict{{"status", Int64(hdsStatusSuccess)}}); err != nil {
		logger.Debug("HKSV open response failed", "camera", s.cameraName, "error", err)
		cancel()
		return
	}

	logger.Info("HKSV recording started", "camera", s.cameraName, "streamId", streamID)
	go s.streamFragments(recCtx, streamID)
}

// rejectOpen answers an open request with a protocol-specific error.
func (s *dataStreamSession) rejectOpen(id int64, reason int) {
	header := Dict{
		{"protocol", "dataSend"},
		{"response", "open"},
		{"id", Int64(id)},
		{"status", Int64(hdsStatusProtocolSpecificError)},
	}
	if err := s.send(header, Dict{{"status", Int64(reason)}}); err != nil {
		logger.Debug("HKSV open rejection failed", "camera", s.cameraName, "error", err)
	}
	logger.Debug("HKSV recording open rejected", "camera", s.cameraName, "reason", reason)
}

// streamFragments drives the recorder and sends each fragment as one or more
// dataSend "data" events.
func (s *dataStreamSession) streamFragments(ctx context.Context, streamID int64) {
	defer s.stopRecording()

	frags, err := s.recorder.StartRecording(ctx)
	if err != nil {
		logger.Error("HKSV recording failed to start", "camera", s.cameraName, "error", err)
		s.sendClose(streamID, reasonUnexpectedFailure)
		return
	}

	seq := 1 // 1 = mediaInitialization, then increments for each media fragment
	for {
		select {
		case <-ctx.Done():
			return
		case frag, ok := <-frags:
			if !ok {
				return
			}
			if frag.Err != nil {
				logger.Warn("HKSV recording error", "camera", s.cameraName, "error", frag.Err)
				s.sendClose(streamID, reasonUnexpectedFailure)
				return
			}
			if err := s.sendData(streamID, frag.Data, seq, frag.IsLast); err != nil {
				if ctx.Err() == nil {
					logger.Debug("HKSV send fragment failed", "camera", s.cameraName, "error", err)
				}
				return
			}
			seq++
			if frag.IsLast {
				logger.Info("HKSV recording complete", "camera", s.cameraName, "fragments", seq-1)
				return
			}
		}
	}
}

// sendData splits a fragment into <=maxChunkSize chunks and sends each as a
// dataSend "data" event, following the metadata rules HKSV expects. Only the
// very first chunk of the very first fragment is tagged mediaInitialization;
// endOfStream is set on the last chunk of every fragment (true only on the
// final fragment), matching the reference controller's expectations.
func (s *dataStreamSession) sendData(streamID int64, data []byte, seq int, isLast bool) error {
	total := len(data)
	chunkSeq := 1
	for offset := 0; ; {
		end := min(offset+maxChunkSize, total)
		chunk := data[offset:end]
		lastChunk := end >= total

		dataType := "mediaFragment"
		if seq == 1 && chunkSeq == 1 {
			dataType = "mediaInitialization"
		}

		metadata := Dict{
			{"dataType", dataType},
			{"dataSequenceNumber", seq},
			{"dataChunkSequenceNumber", chunkSeq},
			{"isLastDataChunk", lastChunk},
		}
		if chunkSeq == 1 {
			metadata = append(metadata, KV{"dataTotalSize", total})
		}

		message := Dict{
			{"streamId", streamID},
			{"packets", []any{Dict{
				{"data", chunk},
				{"metadata", metadata},
			}}},
		}
		if lastChunk {
			message = append(message, KV{"endOfStream", isLast})
		}

		header := Dict{{"protocol", "dataSend"}, {"event", "data"}}
		if err := s.send(header, message); err != nil {
			return err
		}

		offset = end
		chunkSeq++
		if lastChunk {
			return nil
		}
	}
}

// sendClose tells the controller the accessory is ending the stream.
func (s *dataStreamSession) sendClose(streamID int64, reason int) {
	header := Dict{{"protocol", "dataSend"}, {"event", "close"}}
	message := Dict{{"streamId", streamID}, {"reason", Int64(reason)}}
	if err := s.send(header, message); err != nil {
		logger.Debug("HKSV close send failed", "camera", s.cameraName, "error", err)
	}
}

// stopRecording cancels any in-flight recording. Safe to call multiple times.
func (s *dataStreamSession) stopRecording() {
	s.mu.Lock()
	cancel := s.cancelRec
	s.cancelRec = nil
	s.recording = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
