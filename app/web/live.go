package web

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/philipparndt/go-logger"
)

// maxLiveSessions caps concurrent web live streams; each one runs an ffmpeg
// remux process.
const maxLiveSessions = 8

var liveUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 64 * 1024,
	// The API is same-origin behind the ingress; keep parity with the CORS
	// middleware which allows all origins.
	CheckOrigin: func(*http.Request) bool { return true },
}

var liveSessions atomic.Int32

// live streams the camera as fragmented MP4 over a websocket, for playback
// via the browser MediaSource API. Binary messages carry the fMP4 byte
// stream; the client parses the codec string from the init segment.
func (ws *WebServer) live(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	quality := r.URL.Query().Get("q")

	if liveSessions.Load() >= maxLiveSessions {
		http.Error(w, "too many live sessions", http.StatusServiceUnavailable)
		return
	}

	stream, err := ws.b.OpenLiveStream(r.Context(), id, quality)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	conn, err := liveUpgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = stream.Close()
		return
	}
	liveSessions.Add(1)
	defer func() {
		liveSessions.Add(-1)
		_ = stream.Close()
		_ = conn.Close()
	}()

	// Reader goroutine: the client never sends data, but reading is required
	// to process close/ping frames and notice a dropped connection.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, 64*1024)
	for {
		select {
		case <-done:
			return
		default:
		}

		n, err := stream.Read(buf)
		if n > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			logger.Debug("Live stream ended", "camera", id, "error", err)
			return
		}
	}
}
